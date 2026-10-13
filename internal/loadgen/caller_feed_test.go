package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

// The offline retention fixture in callharness measures history costs. This
// complements it with the actual HTTP handlers and UDP-writing media pacers.
func TestDistributedFeedWithVideoPacers(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	local, err := callharness.Start(callharness.Options{Relay: true})
	require.NoError(t, err)
	defer func() { require.NoError(t, local.Close()) }()
	clock, err := callharness.NewClockTracker(0)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, clock.Add(callharness.ClockProbe{CallerSend: now, CallerReceive: now.Add(time.Millisecond), ServerReceive: now.Round(0).Add(time.Millisecond / 2), ServerSend: now.Round(0).Add(time.Millisecond / 2)}))
	caller, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: local.SignalingURL(), RelayAddr: local.RelayAddr(), Distributed: &callharness.DistributedOptions{RunID: "feed-test", Clock: clock}}})
	require.NoError(t, err)
	defer func() { require.NoError(t, caller.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	meter := &Meter{}
	calls := make([]*callharness.Call, 3)
	for i := range calls {
		calls[i], err = caller.Dial(ctx, callharness.CallOptions{Video: true, SendTiming: meter.Observe, SendPacing: meter.ObservePacing, Recording: callharness.RecordingOptions{History: 180 * time.Second, DecodeEvery: 30}})
		require.NoError(t, err)
	}
	mux := http.NewServeMux()
	registerDistributedFeed(mux, calls)
	post := func(path string, body any) {
		data, e := json.Marshal(body)
		require.NoError(t, e)
		reply := httptest.NewRecorder()
		mux.ServeHTTP(reply, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data)))
		require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
	}
	done := make(chan error, 3)
	for _, c := range calls {
		go func() { done <- c.SendMedia(ctx, time.Second) }()
	}
	time.Sleep(100 * time.Millisecond)
	began := time.Now()
	for round := 0; round < 10; round++ {
		var events []callharness.DistributedEvent
		for _, c := range calls {
			id := callharness.OperationIdentity{RunID: "feed-test", OperationID: fmt.Sprint(round), SessionID: c.SessionID(), Worker: "0", Epoch: 1}
			post("/arm-event", id)
			events = append(events, callharness.DistributedEvent{Identity: id, Kind: "move", Start: time.Now(), End: time.Now()})
		}
		post("/record-events", events)
		time.Sleep(30 * time.Millisecond)
	}
	handling := time.Since(began)
	for range calls {
		require.NoError(t, <-done)
	}
	observed := meter.sample()
	t.Logf("real HTTP feed/UDP pacers: handling=%s p99=%s max=%s skipped=%d", handling, observed.all.P99, observed.all.Max, observed.pacing.SkippedFrames)
	if os.Getenv("RELAIS_TEST_STRICT_TIMING") == "1" {
		require.Less(t, observed.all.P99, 20*time.Millisecond)
		require.Zero(t, observed.pacing.SkippedFrames)
	}
	for _, c := range calls {
		report, e := c.Hangup(ctx)
		require.NoError(t, e)
		require.Len(t, report.Moves, 10)
		require.Len(t, report.Distributed.Armed, 10)
		require.Empty(t, report.Distributed.Missing)
		require.Positive(t, report.ReportBuildDuration)
	}
}
