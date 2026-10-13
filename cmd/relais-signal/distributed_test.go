package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

type authenticatedTransport struct{ base http.RoundTripper }

func (a authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+testToken)
	return a.base.RoundTrip(r)
}

func TestDistributedExternalCallerMoveAndKill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	local, err := callharness.Start(callharness.Options{Relay: true, Workers: 2, SnapshotInterval: 50 * time.Millisecond})
	require.NoError(t, err)
	defer func() { require.NoError(t, local.Close()) }()
	target, err := url.Parse(strings.TrimSuffix(local.SignalingURL(), "/calls"))
	require.NoError(t, err)
	skew := 3 * time.Second
	srv, client := frontendWithClock(t, httputil.NewSingleHostReverseProxy(target), func() time.Time { return time.Now().Add(skew) })
	client.Transport = authenticatedTransport{base: client.Transport}
	clock, err := callharness.NewClockTracker(100)
	require.NoError(t, err)
	probe := func() {
		p, err := callharness.ProbeClock(ctx, client, srv.URL+"/clock")
		require.NoError(t, err)
		require.NoError(t, clock.Add(p))
		offset, err := p.Offset()
		require.NoError(t, err)
		require.LessOrEqual(t, offset.Lower, skew)
		require.GreaterOrEqual(t, offset.Upper, skew)
	}
	probe()
	// Control and workers share the injected three-second server clock in this
	// fixture. The input bounds state zero relative error against the frontend.
	external, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: srv.URL + "/calls", RelayAddr: local.RelayAddr(), HTTPClient: client, Distributed: &callharness.DistributedOptions{RunID: "e2e", Clock: clock}}})
	require.NoError(t, err)
	defer func() { require.NoError(t, external.Close()) }()
	c, err := external.Dial(ctx, callharness.CallOptions{Worker: 0, Recording: callharness.RecordingOptions{History: 180 * time.Second, DecodeEvery: 30}})
	require.NoError(t, err)
	media := make(chan error, 1)
	go func() { media <- c.SendMedia(ctx, 16*time.Second) }()
	// This is a local fixture timer, never used as an event timestamp.
	wait := func(d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	wait(2 * time.Second)
	status, err := local.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	moveID := callharness.OperationIdentity{RunID: "e2e", OperationID: "move-1", SessionID: c.SessionID(), Worker: "0", Epoch: status.Calls[0].Epoch}
	require.NoError(t, c.ArmDistributedEvent(moveID))
	wait(20 * time.Millisecond)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String()+"/calls/"+c.SessionID()+"/move?to=1", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var moved controlplane.MoveResult
	require.NoError(t, json.NewDecoder(response.Body).Decode(&moved))
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	moveStart, moveEnd := moved.Start.Add(skew), moved.End.Add(skew)
	moved.Start, moved.End = moveStart, moveEnd
	wait(20 * time.Millisecond)
	require.NoError(t, c.RecordDistributedEvent(callharness.DistributedEvent{Identity: moveID, Kind: "move", Start: moveStart, End: moveEnd, Result: moved}))
	wait(6 * time.Second)
	probe()
	status, err = local.Status(ctx)
	require.NoError(t, err)
	killID := callharness.OperationIdentity{RunID: "e2e", OperationID: "kill-1", SessionID: c.SessionID(), Worker: "1", Epoch: status.Calls[0].Epoch}
	require.NoError(t, c.ArmDistributedEvent(killID))
	wait(20 * time.Millisecond)
	require.NoError(t, local.Kill(1))
	// Timestamp the completed in-process kill action on its server host. This
	// fixture simulates process death; it does not claim an OS SIGKILL receipt.
	killed := time.Now().Add(skew)
	var taken controlplane.MoveResult
	require.Eventually(t, func() bool {
		status, err := local.Status(ctx)
		if err != nil {
			return false
		}
		for _, e := range status.Takeovers {
			if e.ID == c.SessionID() && e.Kind == "takeover" {
				taken = e
				return true
			}
		}
		return false
	}, 3*time.Second, 20*time.Millisecond)
	taken.Start, taken.End = taken.Start.Add(skew), taken.End.Add(skew)
	wait(20 * time.Millisecond)
	require.NoError(t, c.RecordDistributedEvent(callharness.DistributedEvent{Identity: killID, Kind: "takeover", Start: killed, End: taken.End, Result: taken}))
	// An inconsistent receipt is retained, but cannot pass when the mapped
	// event falls outside its caller arm/receipt bracket.
	badID := moveID
	badID.OperationID = "bad-bound"
	require.NoError(t, c.ArmDistributedEvent(badID))
	wait(20 * time.Millisecond)
	bad := moved
	bad.From = "0"
	bad.Start = time.Now().Add(skew + 10*time.Second)
	bad.End = bad.Start.Add(time.Millisecond)
	require.NoError(t, c.RecordDistributedEvent(callharness.DistributedEvent{Identity: badID, Kind: "move", Start: bad.Start, End: bad.End, Result: bad}))
	require.NoError(t, <-media)
	probe()
	report, err := c.Hangup(ctx)
	require.NoError(t, err)
	require.Len(t, report.Moves, 3)
	byID := map[string]callharness.MoveReport{}
	for _, m := range report.Moves {
		require.NotNil(t, m.Identity)
		byID[m.Identity.OperationID] = m
	}
	require.True(t, byID["move-1"].Measurement.Conclusive(), byID["move-1"].Measurement.Summary())
	require.True(t, byID["kill-1"].Measurement.Conclusive(), byID["kill-1"].Measurement.Summary())
	require.Positive(t, byID["move-1"].Distributed.Start.Latest.Sub(byID["move-1"].Distributed.Start.Earliest))
	require.Equal(t, "pass", byID["move-1"].Distributed.Outcome, byID["move-1"].Distributed.Causes)
	require.Equal(t, "fail", byID["kill-1"].Distributed.Outcome, byID["kill-1"].Distributed.Causes)
	require.True(t, byID["kill-1"].Distributed.AudioLoss.Trusted)
	require.True(t, byID["kill-1"].Distributed.AudioLoss.Failed)
	for _, g := range byID["kill-1"].Distributed.Gaps {
		require.Equal(t, "pass", g.Outcome)
	}
	require.NotNil(t, byID["kill-1"].Distributed.KillToLive)
	require.Less(t, byID["kill-1"].Distributed.KillToLive.Upper, 2*time.Second)
	require.Equal(t, "inconclusive", byID["bad-bound"].Distributed.Outcome)
	require.Contains(t, strings.Join(byID["bad-bound"].Distributed.Causes, ";"), "host clock bound")
	require.NotNil(t, report.Distributed)
	require.Len(t, report.Distributed.Probes, 3)
	require.Equal(t, "fail", report.Distributed.Outcome, "known content loss survives insufficient trials and uncertain clock evidence")
}
