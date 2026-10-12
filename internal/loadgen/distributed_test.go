package loadgen

import (
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
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
