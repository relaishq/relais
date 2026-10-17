package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestStoreStallADRArithmetic(t *testing.T) {
	for _, tc := range []struct {
		age       time.Duration
		rtp, rtcp uint64
	}{{1500 * time.Millisecond, 9440, 205}, {5 * time.Second, 31315, 642}} {
		rtp, rtcp, cap := storeStallMargins(tc.age, mediaworker.CheckpointState{})
		require.Equal(t, tc.rtp, rtp)
		require.Equal(t, tc.rtcp, rtcp)
		require.EqualValues(t, 22702, cap)
	}
	rtp, _, cap := storeStallMargins(time.Second, mediaworker.CheckpointState{RTPPacketRate: 10000})
	require.EqualValues(t, 12565, rtp)
	require.EqualValues(t, 12702, cap)
}

func stallFixture(loss bool) (*callharness.Report, controlplane.Status, *storeStallEvidence, time.Time, time.Time, time.Time) {
	start := time.Unix(100, 0)
	stalled, killed, until := start.Add(time.Second), start.Add(2500*time.Millisecond), start.Add(10*time.Second)
	age := 1600 * time.Millisecond
	if loss {
		age = 5 * time.Second
		killed = start.Add(6 * time.Second)
	}
	event := controlplane.MoveResult{ID: "session", Kind: "takeover", From: "0", To: "1", SnapshotAge: age, CheckpointAge: age - time.Millisecond, CheckpointStoredAt: start, Checkpoint: mediaworker.CheckpointState{Successes: 5}, End: killed.Add(500 * time.Millisecond), CheckpointPolicy: "scaled"}
	rtp, rtcp, _ := storeStallMargins(age, event.Checkpoint)
	event.SequenceMargin = uint16(rtp)
	event.SRTCPIndexMargin = uint32(rtcp)
	e := &storeStallEvidence{Variant: "moderate", Takeover: event, EnvelopeEvents: 1}
	status := controlplane.Status{Calls: []controlplane.CallStatus{{Owner: "1"}}, Takeovers: []controlplane.MoveResult{event}}
	report := &callharness.Report{StartedAt: start, OfferAnswerExchanges: 1, RemoteAddr: "127.0.0.1:1234", ConnectionStates: []callharness.StateChange{{State: "connected"}}, Moves: []callharness.MoveReport{{}}, Tracks: []callharness.TrackReport{{Kind: "audio", Arrivals: []time.Duration{2 * time.Second}, LastArrival: 10 * time.Second}, {Kind: "video", Arrivals: []time.Duration{2 * time.Second}, LastArrival: 10 * time.Second}}}
	if loss {
		e.Variant = "long"
		e.Takeover.Lost = true
		e.Takeover.LossCause = "checkpoint_unsafe"
		e.Takeover.CheckpointPolicy = "definitive-loss"
		e.Takeover.SequenceMargin = 0
		e.Takeover.SRTCPIndexMargin = 0
		e.LossEvents = 1
		e.LeaseReleased = true
		e.RoutesCleared = true
		status.Calls = nil
		status.LostCount = 1
		status.Takeovers[0] = e.Takeover
		for i := range report.Tracks {
			report.Tracks[i].LastArrival = killed.Sub(start)
		}
	}
	return report, status, e, stalled, killed, until
}

func TestJudgeStoreStall(t *testing.T) {
	for _, loss := range []bool{false, true} {
		report, status, e, stalled, killed, until := stallFixture(loss)
		require.Empty(t, judgeStoreStall(report, status, e, report.RemoteAddr, stalled, killed, until))
	}
	for _, tc := range []struct {
		name   string
		loss   bool
		mutate func(*callharness.Report, *controlplane.Status, *storeStallEvidence)
	}{
		{"decryption", false, func(r *callharness.Report, _ *controlplane.Status, _ *storeStallEvidence) {
			r.DecryptionFailures.AuthTag = 1
		}},
		{"reconnect", false, func(r *callharness.Report, _ *controlplane.Status, _ *storeStallEvidence) { r.ICERestarts = 1 }},
		{"renegotiation", false, func(r *callharness.Report, _ *controlplane.Status, _ *storeStallEvidence) { r.Renegotiations = 1 }},
		{"no-stalled-media", false, func(r *callharness.Report, _ *controlplane.Status, _ *storeStallEvidence) { r.Tracks[0].Arrivals = nil }},
		{"no-resumed-media", false, func(r *callharness.Report, _ *controlplane.Status, _ *storeStallEvidence) {
			r.Tracks[0].LastArrival = 2 * time.Second
		}},
		{"plain-margin", false, func(_ *callharness.Report, _ *controlplane.Status, e *storeStallEvidence) {
			e.Takeover.SequenceMargin = 8192
		}},
		{"missing-counter", false, func(_ *callharness.Report, _ *controlplane.Status, e *storeStallEvidence) { e.EnvelopeEvents = 0 }},
		{"double-counter", false, func(_ *callharness.Report, _ *controlplane.Status, e *storeStallEvidence) { e.EnvelopeEvents = 2 }},
		{"resume-over-cap", true, func(_ *callharness.Report, _ *controlplane.Status, e *storeStallEvidence) { e.Takeover.Lost = false }},
		{"loss-media", true, func(r *callharness.Report, _ *controlplane.Status, _ *storeStallEvidence) {
			r.Tracks[0].LastArrival = 10 * time.Second
		}},
		{"lease-retained", true, func(_ *callharness.Report, _ *controlplane.Status, e *storeStallEvidence) { e.LeaseReleased = false }},
		{"route-retained", true, func(_ *callharness.Report, _ *controlplane.Status, e *storeStallEvidence) { e.RoutesCleared = false }},
		{"loss-not-counted", true, func(_ *callharness.Report, s *controlplane.Status, _ *storeStallEvidence) { s.LostCount = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, e, a, k, u := stallFixture(tc.loss)
			tc.mutate(r, &s, e)
			require.NotEmpty(t, judgeStoreStall(r, s, e, r.RemoteAddr, a, k, u))
		})
	}
}

func TestValidateStoreStall(t *testing.T) {
	require.NoError(t, validateStoreStall("moderate", 0, "", "loopback", false))
	require.NoError(t, validateStoreStall("long", 6*time.Second, "", "loopback", false))
	require.NoError(t, validateStoreStall("live", 3500*time.Millisecond, "", "loopback", false))
	for _, tc := range []struct {
		variant        string
		duration       time.Duration
		addr, topology string
		other          bool
	}{
		{"", time.Second, "", "loopback", false}, {"other", 0, "", "loopback", false}, {"moderate", -time.Second, "", "loopback", false}, {"long", 0, "127.0.0.1:6379", "loopback", false}, {"moderate", 0, "", "netns", false}, {"long", 0, "", "loopback", true},
	} {
		require.Error(t, validateStoreStall(tc.variant, tc.duration, tc.addr, tc.topology, tc.other))
	}
}

func TestMissingTakeoverStillCollectsAndJudgesCallerReport(t *testing.T) {
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, cause := range []string{"ownership_expired", "checkpoint_unsafe", "checkpoint_missing", "recovery_failed"} {
			_, _ = fmt.Fprintf(w, "%s 0\n", lossCounter(cause))
		}
		_, _ = w.Write([]byte("# TYPE relais_checkpoint_envelope_events_total counter\nrelais_checkpoint_envelope_events_total{policy=\"scaled\"} 0\nrelais_checkpoint_envelope_events_total{policy=\"definitive-loss\"} 0\n"))
	}))
	defer metrics.Close()
	report, _, e, stalled, killed, until := stallFixture(true)
	report.DecryptionFailures.AuthTag = 1
	options := &storeStallOptions{Variant: "long", Duration: 5 * time.Second, Actual: 5 * time.Second, TimedOut: true, controlURL: metrics.URL, before: map[string]float64{envelopeCounter("scaled"): 0, envelopeCounter("definitive-loss"): 0}}
	for _, cause := range []string{"ownership_expired", "checkpoint_unsafe", "checkpoint_missing", "recovery_failed"} {
		options.before[lossCounter(cause)] = 0
	}
	dir := t.TempDir()
	result, err := options.judge(context.Background(), report, nil, controlplane.Status{}, clusterprocess.Ready{Media: report.RemoteAddr}, stalled, killed, until, dir)
	require.NoError(t, err)
	require.False(t, result.Pass)
	require.Equal(t, "missing-takeover", result.StoreStall.Outcome)
	require.True(t, result.StoreStall.TimedOut)
	require.Equal(t, 1, result.Decrypt)
	require.Contains(t, result.StoreStall.Failures, "caller decryption failures")
	for _, name := range []string{"caller-report.json", "store-stall.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.NotEmpty(t, b)
	}
	e.Observations = []storeStallObservation{{}}
	report.DecryptionFailures.AuthTag = 0
	require.Empty(t, judgeLiveStoreStall(report, e, stalled, killed))
	report.Tracks[0].Arrivals = nil
	require.Contains(t, judgeLiveStoreStall(report, e, stalled, killed), "no audio media past envelope during stall")
}

func TestJudgeOwnershipExpiredStoreStall(t *testing.T) {
	for _, variant := range []string{"live"} {
		for _, mutation := range []string{"none", "missing-metric", "double-metric", "missing-status", "double-status", "wrong-cause", "cached-route", "resumed-media", "decryption"} {
			t.Run(variant+"/"+mutation, func(t *testing.T) {
				report, status, e, stalled, killed, _ := stallFixture(true)
				e.Variant, e.ActualStall = variant, 5*time.Second
				report.Moves[0].Lost, report.Moves[0].LossCause = true, "ownership_expired"
				e.EnvelopeEvents = 0
				e.Takeover.LossCause = "ownership_expired"
				e.Takeover.CheckpointPolicy = ""
				e.Takeover.SnapshotAge, e.Takeover.CheckpointAge = 0, 0
				e.ControlCounters = map[string]float64{lossCounter("ownership_expired"): 1}
				status.Takeovers = []controlplane.MoveResult{e.Takeover}
				e.Observations = []storeStallObservation{{RelayRoutes: 0}}
				switch mutation {
				case "missing-metric":
					e.LossEvents = 0
				case "double-metric":
					e.LossEvents = 2
				case "missing-status":
					status.Takeovers = nil
				case "double-status":
					status.Takeovers = append(status.Takeovers, e.Takeover)
				case "wrong-cause":
					e.Takeover.LossCause = "recovery_failed"
				case "cached-route":
					e.Observations[0].RelayRoutes = 1
				case "resumed-media":
					report.Tracks[0].LastArrival = 10 * time.Second
				case "decryption":
					report.DecryptionFailures.AuthTag = 1
				}
				failures := judgeOwnershipExpired(report, status, e, report.RemoteAddr, stalled, killed)
				if mutation == "none" {
					require.Empty(t, failures)
				} else {
					require.NotEmpty(t, failures)
				}
			})
		}
	}
}

func TestJudgeLiveSurvivalRejectsLossGapAndStandbyTakeover(t *testing.T) {
	for _, mutation := range []string{"none", "loss", "gap", "standby", "epoch", "stopped", "fenced", "decrypt"} {
		t.Run(mutation, func(t *testing.T) {
			report, status, e, stalled, _, until := stallFixture(false)
			resumed := stalled.Add(5 * time.Second)
			e.Owner = "0"
			e.Variant = "live"
			e.LossEvents = 0
			e.EnvelopeEvents = 0
			e.RelayEpochBefore = 1
			e.RelayEpochAfter = 1
			e.StandbyWaiting = true
			e.Observations = []storeStallObservation{{WorkerSessions: 1, RelayRoutes: 1, LeaseExists: true, SnapshotExists: true}}
			status.Calls[0].Owner = "0"
			status.Takeovers = nil
			report.Moves = nil
			for i := range report.Tracks {
				report.Tracks[i].Arrivals = nil
				for at := time.Second; at <= 10*time.Second; at += 100 * time.Millisecond {
					report.Tracks[i].Arrivals = append(report.Tracks[i].Arrivals, at)
				}
			}
			switch mutation {
			case "loss":
				status.LostCount = 1
			case "gap":
				report.Tracks[0].Arrivals = []time.Duration{time.Second, 5 * time.Second, 10 * time.Second}
			case "standby":
				e.StandbyWaiting = false
			case "epoch":
				e.RelayEpochAfter = 2
			case "stopped":
				report.Tracks[0].LastArrival = 6 * time.Second
			case "fenced":
				e.Observations[0].WorkerSessions = 0
			case "decrypt":
				report.DecryptionFailures.AuthTag = 1
			}
			f := judgeLiveSurvival(report, status, e, report.RemoteAddr, stalled, resumed, until)
			if mutation == "none" {
				require.Empty(t, f)
			} else {
				require.NotEmpty(t, f)
			}
		})
	}
}

// Accounting precedes asynchronous fencing. Confirm a bounded cleanup before
// checking that old media cannot resume, rather than treating continued old
// forwarding as a new snapshot adoption.
func TestExpiredStoreStallRequiresBoundedConfirmedCleanup(t *testing.T) {
	for _, mutation := range []string{"none", "missing-cleanup", "slow-cleanup", "media-after-cleanup"} {
		t.Run(mutation, func(t *testing.T) {
			report, status, e, stalled, _, _ := stallFixture(true)
			resumed := report.StartedAt.Add(36 * time.Second)
			e.Variant = "expired"
			e.ActualStall = 35 * time.Second
			e.ResumedAt = resumed
			e.Takeover.End = resumed.Add(100 * time.Millisecond)
			e.Takeover.LossCause = "ownership_expired"
			e.Takeover.CheckpointPolicy = ""
			e.Takeover.SnapshotAge = 0
			e.Takeover.CheckpointAge = 0
			e.EnvelopeEvents = 0
			e.ControlCounters = map[string]float64{lossCounter("ownership_expired"): 1}
			status.Takeovers = []controlplane.MoveResult{e.Takeover}
			report.Moves[0].Lost = true
			report.Moves[0].LossCause = "ownership_expired"
			completed := resumed.Add(time.Second)
			e.Observations = []storeStallObservation{{CompletedAt: resumed.Add(200 * time.Millisecond), RelayRoutes: 1, WorkerSessions: 1}, {CompletedAt: completed}}
			for i := range report.Tracks {
				report.Tracks[i].LastArrival = completed.Sub(report.StartedAt) - 50*time.Millisecond
			}
			switch mutation {
			case "missing-cleanup":
				e.Observations = e.Observations[:1]
			case "slow-cleanup":
				e.Observations[1].CompletedAt = resumed.Add(4 * time.Second)
			case "media-after-cleanup":
				report.Tracks[0].LastArrival = completed.Sub(report.StartedAt) + 100*time.Millisecond
			}
			failures := judgeOwnershipExpired(report, status, e, report.RemoteAddr, stalled, resumed)
			if mutation == "none" {
				require.Empty(t, failures)
			} else {
				require.NotEmpty(t, failures)
			}
		})
	}
	require.NoError(t, validateStoreStall("expired", 35*time.Second, "", "loopback", false))
}

func TestLongStoreStallRejectsOwnershipExpiry(t *testing.T) {
	r, status, e, stalled, killed, until := stallFixture(true)
	e.Takeover.LossCause = "ownership_expired"
	e.Takeover.SnapshotAge = 0
	e.Takeover.CheckpointAge = 0
	e.Takeover.CheckpointPolicy = ""
	e.EnvelopeEvents = 0
	e.ActualStall = 5 * time.Second
	e.ControlCounters = map[string]float64{lossCounter("ownership_expired"): 1}
	e.Observations = []storeStallObservation{{RelayRoutes: 0}}
	status.Takeovers = []controlplane.MoveResult{e.Takeover}
	r.Moves[0].Lost = true
	r.Moves[0].LossCause = "ownership_expired"
	require.NotEmpty(t, judgeStoreStall(r, status, e, r.RemoteAddr, stalled, killed, until))
	require.NotEmpty(t, judgeOwnershipExpired(r, status, e, r.RemoteAddr, stalled, killed))
}

func TestCustomStaleTakeoverAllowsIntentionalAudioSilence(t *testing.T) {
	report, status, e, stalled, killed, until := stallFixture(false)
	e.Agent = &storeStallAgentEvidence{Name: "delayed-loopback/v1"}
	// The delayed agent returns audio after takeover, then records another turn.
	// It need not still be emitting in the last 200ms before Hangup.
	report.Tracks[0].LastArrival = e.Takeover.End.Sub(report.StartedAt) + time.Second
	require.Empty(t, judgeStoreStall(report, status, e, report.RemoteAddr, stalled, killed, until))
	report.Tracks[0].LastArrival = e.Takeover.End.Sub(report.StartedAt) - time.Millisecond
	require.Contains(t, judgeStoreStall(report, status, e, report.RemoteAddr, stalled, killed, until), "no custom audio after stale takeover")
}
