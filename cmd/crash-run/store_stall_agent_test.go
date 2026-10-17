package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func agentStallFixture() *storeStallEvidence {
	before := &mediaworker.AgentSessionStatus{Name: "delayed-loopback/v1", CheckpointKnown: true,
		Tasks: []mediaworker.AgentTaskStatus{{Turn: 1, Published: 1}}}
	during := *before
	during.Progress.Consumed = 2
	during.PlaybackPackets = 1
	during.Tasks = []mediaworker.AgentTaskStatus{{Turn: 2, Published: 1}}
	return &storeStallEvidence{Variant: "live", Agent: &storeStallAgentEvidence{Name: before.Name, Before: before, During: []mediaworker.AgentSessionStatus{during}}}
}

func TestJudgeStoreStallAgentChecks(t *testing.T) {
	require.Empty(t, judgeStoreStallAgent(&storeStallEvidence{}))
	require.Empty(t, judgeStoreStallAgent(agentStallFixture()))
	for _, change := range []string{"before", "name", "checkpoint", "errors", "during", "progress", "queued-before", "unpublished", "takeover-missing", "takeover-unknown", "takeover-invalid", "takeover-no-resume"} {
		t.Run(change, func(t *testing.T) {
			e := agentStallFixture()
			switch change {
			case "before":
				e.Agent.Before = nil
			case "name":
				e.Agent.Before.Name = "wrong"
			case "checkpoint":
				e.Agent.Before.CheckpointKnown = false
			case "errors":
				e.Agent.Errors = []string{"failed"}
			case "during":
				e.Agent.During = nil
			case "progress":
				e.Agent.During[0].Progress.Consumed = 0
			case "queued-before":
				e.Agent.During[0].Tasks[0].Turn = 1
			case "unpublished":
				e.Agent.During[0].Tasks[0].Published = 0
			case "takeover-missing":
				e.Variant = "moderate"
			case "takeover-unknown":
				e.Variant = "moderate"
				e.Agent.After = &mediaworker.AgentSessionStatus{App: json.RawMessage(`{"resume":{"move_only":"known"}}`)}
			case "takeover-no-resume":
				e.Variant = "moderate"
				e.Agent.After = &mediaworker.AgentSessionStatus{App: json.RawMessage(`{}`)}
			case "takeover-invalid":
				e.Variant = "moderate"
				e.Agent.After = &mediaworker.AgentSessionStatus{App: json.RawMessage(`bad`)}
			}
			expected := map[string]string{
				"before": "pre-stall checkpoint missing", "name": "pre-stall checkpoint missing", "checkpoint": "pre-stall checkpoint missing",
				"errors": "diagnostics failed", "during": "no in-outage local diagnostic evidence", "progress": "local input did not advance",
				"queued-before": "new local task publication not observed", "unpublished": "new local task publication not observed",
				"takeover-missing": "stale takeover diagnostics missing", "takeover-unknown": "stale takeover did not report unknown", "takeover-invalid": "stale takeover did not report unknown",
				"takeover-no-resume": "stale takeover did not report unknown",
			}[change]
			require.Contains(t, strings.Join(judgeStoreStallAgent(e), "; "), expected)
		})
	}
	e := agentStallFixture()
	e.Variant = "moderate"
	e.Agent.After = &mediaworker.AgentSessionStatus{App: json.RawMessage(`{"resume":{"move_only":"outcome_unknown"}}`)}
	require.Empty(t, judgeStoreStallAgent(e))
}

func TestCustomConclusiveEventsChecksEveryVerdict(t *testing.T) {
	makeResult := func() result {
		m := callharness.EventMeasurement{SettleWindow: time.Second}
		for _, v := range []*callharness.MetricVerdict{&m.VideoLoss, &m.Video.Verdict, &m.ResumeVerdict, &m.FirstContentVerdict, &m.FirstNewContentVerdict, &m.FirstLiveVerdict} {
			v.Trusted = true
		}
		return result{Measurement: m}
	}
	require.Equal(t, 1, conclusiveEvents([]result{makeResult()}, true))
	for i := 0; i < 7; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r := makeResult()
			m := &r.Measurement
			if i == 6 {
				m.SettleWindow = 0
			} else {
				[]*callharness.MetricVerdict{&m.VideoLoss, &m.Video.Verdict, &m.ResumeVerdict, &m.FirstContentVerdict, &m.FirstNewContentVerdict, &m.FirstLiveVerdict}[i].Trusted = false
			}
			require.Zero(t, conclusiveEvents([]result{r}, true))
		})
	}
}

func TestCustomLiveSurvivalChecks(t *testing.T) {
	for _, mutation := range []string{"none", "no-recovered-audio", "no-outage-audio", "video-gap", "video-stopped", "lost", "moved", "owner", "loss-counter", "envelope-counter", "epoch-zero", "epoch-changed", "standby", "remote", "moves", "disconnected", "sessions", "routes", "lease", "snapshot", "no-observations", "decrypt", "restart", "renegotiation", "offer", "tracks"} {
		t.Run(mutation, func(t *testing.T) {
			report, status, e, stalled, _, until := stallFixture(false)
			resumed := stalled.Add(5 * time.Second)
			e.Owner = "0"
			e.Variant = "live"
			e.EnvelopeEvents = 0
			e.RelayEpochBefore = 1
			e.RelayEpochAfter = 1
			e.StandbyWaiting = true
			e.Agent = agentStallFixture().Agent
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
			// Deliberately delayed custom audio is valid; video remains continuous.
			report.Tracks[0].Arrivals = []time.Duration{2 * time.Second, 7 * time.Second}
			switch mutation {
			case "no-recovered-audio":
				report.Tracks[0].Arrivals = []time.Duration{2 * time.Second}
			case "no-outage-audio":
				report.Tracks[0].Arrivals = []time.Duration{7 * time.Second}
			case "video-gap":
				report.Tracks[1].Arrivals = []time.Duration{2 * time.Second, 10 * time.Second}
			case "video-stopped":
				report.Tracks[1].LastArrival = 6 * time.Second
			case "lost":
				status.LostCount = 1
			case "moved":
				status.Takeovers = []controlplane.MoveResult{{}}
			case "owner":
				status.Calls[0].Owner = "other"
			case "loss-counter":
				e.LossEvents = 1
			case "envelope-counter":
				e.EnvelopeEvents = 1
			case "epoch-zero":
				e.RelayEpochBefore = 0
				e.RelayEpochAfter = 0
			case "epoch-changed":
				e.RelayEpochAfter = 2
			case "standby":
				e.StandbyWaiting = false
			case "remote":
				report.RemoteAddr = "wrong"
			case "moves":
				report.Moves = []callharness.MoveReport{{}}
			case "disconnected":
				report.ConnectionStates = []callharness.StateChange{{State: "disconnected"}}
			case "sessions":
				e.Observations[0].WorkerSessions = 0
			case "routes":
				e.Observations[0].RelayRoutes = 0
			case "lease":
				e.Observations[0].LeaseExists = false
			case "snapshot":
				e.Observations[0].SnapshotExists = false
			case "no-observations":
				e.Observations = nil
			case "decrypt":
				report.DecryptionFailures.AuthTag = 1
			case "restart":
				report.ICERestarts = 1
			case "renegotiation":
				report.Renegotiations = 1
			case "offer":
				report.OfferAnswerExchanges = 0
			case "tracks":
				report.Tracks = report.Tracks[:1]
			}
			failures := judgeLiveSurvival(report, status, e, "127.0.0.1:1234", stalled, resumed, until)
			if mutation == "none" {
				require.Empty(t, failures)
			} else {
				require.NotEmpty(t, failures)
			}
		})
	}
}
