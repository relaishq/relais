package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func TestEventRetention(t *testing.T) {
	for _, tc := range []struct {
		configured  time.Duration
		distributed bool
		want        time.Duration
		bad         bool
	}{
		{0, false, 15 * time.Second, false}, {0, true, 180 * time.Second, false}, {180 * time.Second, true, 180 * time.Second, false}, {15 * time.Second, true, 0, true}, {-1, false, 0, true},
	} {
		got, err := eventRetention(tc.configured, tc.distributed)
		if tc.bad {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		}
	}
}
func TestDistributedJoinUsesEveryIdentityField(t *testing.T) {
	id := callharness.OperationIdentity{RunID: "run", OperationID: "kill", SessionID: "session", Worker: "generation", Epoch: 3}
	e := EventResult{Event: Event{Kind: "kill"}, Identities: []callharness.OperationIdentity{id}, At: time.Now()}
	r := &callharness.Report{StartedAt: time.Now().Add(-time.Hour)}
	m := callharness.MoveReport{Kind: "takeover", Identity: &id, Lost: true, LossCause: "ownership_expired", Error: "failed"}
	require.True(t, MatchesEvent(e, r, m), "failed events still join regardless of timestamp skew")
	for _, field := range []string{"run", "operation", "session", "worker", "epoch"} {
		changed := id
		switch field {
		case "run":
			changed.RunID = "other"
		case "operation":
			changed.OperationID = "other"
		case "session":
			changed.SessionID = "other"
		case "worker":
			changed.Worker = "other"
		case "epoch":
			changed.Epoch++
		}
		m.Identity = &changed
		require.False(t, MatchesEvent(e, r, m), field)
	}
}
func TestLocalAndIdentityJoinEquivalent(t *testing.T) {
	start := time.Now()
	for _, kind := range []string{"move", "kill", "drain"} {
		id := callharness.OperationIdentity{RunID: "run", OperationID: kind, SessionID: "session", Worker: "0", Epoch: 1}
		e := EventResult{Event: Event{Kind: kind, Worker: 0}, At: start.Add(time.Second), FinishedAt: start.Add(2 * time.Second), Affected: 1, Sessions: []string{"session"}}
		m := callharness.MoveReport{Kind: "move", From: 0, To: 0, Start: time.Second, End: 2 * time.Second, Measurement: callharness.EventMeasurement{AudioLoss: callharness.MetricVerdict{Trusted: true}, ResumeVerdict: callharness.MetricVerdict{Trusted: true}, Audio: callharness.FreshnessReport{Verdict: callharness.MetricVerdict{Trusted: true}}}}
		if kind == "kill" {
			m.Kind = "takeover"
		}
		rep := &callharness.Report{StartedAt: start, Moves: []callharness.MoveReport{m}}
		p := []ProcessReport{{Calls: []Message{{Session: "session", Call: rep}}}}
		local, localCauses := yardstick(p, []EventResult{e})
		e.Identities = []callharness.OperationIdentity{id}
		rep.Moves[0].Identity = &id
		distributed, distributedCauses := yardstick(p, []EventResult{e})
		require.Equal(t, local, distributed)
		require.Equal(t, localCauses, distributedCauses)
	}
}

func TestDistributedReportNeverFallsBackToTimestampJoin(t *testing.T) {
	at := time.Now()
	e := EventResult{Event: Event{Kind: "kill", Worker: 0}, At: at}
	r := &callharness.Report{StartedAt: at, Distributed: &callharness.DistributedRecording{RunID: "run"}}
	move := callharness.MoveReport{Kind: "takeover", From: 0}
	require.False(t, MatchesEvent(e, r, move), "missing distributed identities cannot use the local timestamp matcher")
}

func TestFailedPeriodicProbeIsMissingSample(t *testing.T) {
	mode := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if mode == 3 {
			_, err := w.Write([]byte("{}"))
			require.NoError(t, err)
			return
		}
		if mode == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		received := time.Now()
		sent := received
		if mode == 2 {
			sent = received.Add(-time.Second)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]time.Time{"received_at": received, "sent_at": sent}))
	}))
	defer server.Close()
	tracker, err := callharness.NewClockTracker(100)
	require.NoError(t, err)
	require.NoError(t, sampleDistributedClock(context.Background(), tracker, server.Client(), server.URL))
	mode = 1
	require.Error(t, sampleDistributedClock(context.Background(), tracker, server.Client(), server.URL))
	require.NoError(t, tracker.Err())
	require.Len(t, tracker.Probes(), 1)
	mode = 3
	require.Error(t, sampleDistributedClock(context.Background(), tracker, server.Client(), server.URL))
	require.NoError(t, tracker.Err())
	require.Len(t, tracker.Probes(), 1)
	mode = 0
	require.NoError(t, sampleDistributedClock(context.Background(), tracker, server.Client(), server.URL))
	require.Len(t, tracker.Probes(), 2)
	mode = 2
	require.Error(t, sampleDistributedClock(context.Background(), tracker, server.Client(), server.URL))
	require.Error(t, tracker.Err())
}

func TestDistributedRunPoolsAcrossCallsAndAllowsMoveOutlier(t *testing.T) {
	baseline := make([]time.Duration, 50)
	for i := range baseline {
		baseline[i] = 20 * time.Millisecond
	}
	v := callharness.MetricVerdict{Trusted: true}
	var calls []Message
	var events []EventResult
	for i := 0; i < 20; i++ {
		session := fmt.Sprint("session-", i)
		id := callharness.OperationIdentity{RunID: "run", OperationID: fmt.Sprint("move-", i), SessionID: session, Worker: "0", Epoch: 1}
		gap := 20 * time.Millisecond
		individual := "pass"
		if i == 19 {
			gap = 180 * time.Millisecond
			individual = "fail"
		}
		m := callharness.MoveReport{Kind: "move", Identity: &id, Measurement: callharness.EventMeasurement{AudioLoss: v, ResumeVerdict: v, Audio: callharness.FreshnessReport{Verdict: v}}, Distributed: &callharness.DistributedMeasurement{WindowOutcome: "pass", Outcome: individual, Gaps: []callharness.DistributedGap{{Kind: "audio", BaselineCertain: baseline, BaselinePossible: baseline, EventGap: callharness.DurationInterval{Lower: gap, Upper: gap}}}}}
		call := &callharness.Report{Distributed: &callharness.DistributedRecording{RunID: "run", Outcome: "inconclusive"}, Moves: []callharness.MoveReport{m}}
		events = append(events, EventResult{Event: Event{Kind: "move"}, Affected: 1, Sessions: []string{session}, Identities: []callharness.OperationIdentity{id}})
		if i < 10 {
			kill := id
			kill.OperationID = fmt.Sprint("kill-", i)
			takeover := m
			takeover.Kind = "takeover"
			takeover.Identity = &kill
			takeover.Distributed = &callharness.DistributedMeasurement{WindowOutcome: "pass", Outcome: "pass"}
			call.Moves = append(call.Moves, takeover)
			events = append(events, EventResult{Event: Event{Kind: "kill"}, Affected: 1, Sessions: []string{session}, Identities: []callharness.OperationIdentity{kill}})
		}
		calls = append(calls, Message{Session: session, Call: call})
	}
	r := RunReport{Callers: 20, Processes: []ProcessReport{{Complete: true, Calls: calls}}, Events: events}
	r.Judge()
	require.NotNil(t, r.Distributed)
	require.Equal(t, "pass", r.Distributed.Outcome, r.Distributed.Causes)
	require.Equal(t, "valid", r.Outcome, r.Causes)
	for _, row := range r.Yardstick {
		require.Equal(t, "pass", row.Outcome, row)
	}
	// A second outlier changes pooled p95 and must fail the distributed row.
	calls[18].Call.Moves[0].Distributed.Gaps[0].EventGap = callharness.DurationInterval{Lower: 180 * time.Millisecond, Upper: 180 * time.Millisecond}
	r.Judge()
	require.Equal(t, "fail", r.Distributed.Outcome)
	require.Equal(t, "valid", r.Outcome, "known caller failure does not invalidate generator evidence")
	for _, row := range r.Yardstick {
		if row.Metric == "distributed_window" {
			require.Equal(t, "fail", row.Outcome)
		}
	}
}
