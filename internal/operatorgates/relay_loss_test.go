package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/internal/relayloss"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/relaishq/relais/pkg/relay"
)

func TestRelayLossIsolationFitsBoundedWindow(t *testing.T) {
	want := int((relayLossStepCap + 2*relayloss.Budget + relayLossCollectionCap + relayLossStepCap + relayLossSafety) / time.Second)
	if got := RelayLossIsolationSeconds(); got != want || got > 300 || got <= 120 {
		t.Fatal(got, want)
	}
}
func TestRelayLossProcedure(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, cause string
		unknown              bool
		second               bool
		promoted             bool
	}{
		{"pass", "pass", "", false, false, false},
		{"slow", "fail", "relay-loss-slow", false, false, false},
		{"missing", "inconclusive", "relay-loss-missing", false, false, false},
		{"unrecovered", "fail", "relay-loss-unrecovered", false, false, false},
		{"target-insufficient", "inconclusive", "relay-loss-target-insufficient", false, false, false},
		{"restore-failed", "fail", "relay-loss-restore-failed", false, false, false},
		{"unknown-isolate", "inconclusive", "", true, false, false},
		{"judge-missing", "inconclusive", "relay-loss-missing", false, false, false},
		{"restore-failed-judge-missing", "fail", "relay-loss-restore-failed", false, false, false},
		{"unknown-restore", "inconclusive", "", false, false, false},
		{"unknown-collect", "inconclusive", "", false, false, false},
		{"unknown-start", "inconclusive", "", false, true, false},
		{"rejoin-failed", "fail", "relay-loss-restore-failed", false, false, false},
		{"second-host", "pass", "", false, true, false},
		{"transient-inspection", "pass", "", false, false, false},
		{"already-promoted", "pass", "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, clock := testProcedure(t)
			p := g.p
			var ops []string
			statusReads := 0
			inspections := 0
			renewed := false
			status := controlplane.Status{Ready: true, Relays: []controlplane.RelayStatus{{Name: "preview"}, {Name: "preview-two"}}}
			sessions := []string{"a", "b", "c", "d"}
			for i, id := range sessions {
				slot := "preview"
				if i >= 2 {
					slot = "preview-two"
				}
				if tc.second {
					slot = "preview"
					if i < 3 {
						slot = "preview-two"
					}
				}
				status.Calls = append(status.Calls, controlplane.CallStatus{ID: id, Relay: &controlplane.CallRelayStatus{Slot: slot}})
			}
			preflight := status
			if tc.name == "target-insufficient" {
				preflight.Calls = preflight.Calls[:1]
			}
			var isolateAt time.Duration
			p.Step = func(_ context.Context, op, slot string, _ time.Duration, extra ProcessExtra) (StepResult, error) {
				ops = append(ops, op+":"+slot)
				clock.Advance(time.Second)
				r := StepResult{Certainty: operator.Succeeded}
				switch op {
				case "status":
					statusReads++
					if statusReads == 1 {
						r.Status = preflight
					} else {
						r.Status = status
					}
				case "relay-status", "relay-inspect":
					if op == "relay-inspect" {
						inspections++
						if tc.name == "transient-inspection" && inspections == 1 {
							return StepResult{Certainty: operator.Failed}, fault("precondition-failed:observation")
						}
					}
					addr := "192.0.2.1:3478"
					if slot == "relay-two" {
						addr = "192.0.2.2:3478"
					}
					r.Inspection.Status = relay.RelayStatus{Public: netip.MustParseAddrPort(addr), Stats: relay.Stats{Workers: 2}, Continuity: &relay.ContinuityStatus{Epoch: 1}}
					if renewed {
						r.Inspection.Status.Continuity.Epoch = 2
					}
					r.Inspection.Active = "relais-relay.service"
					if op == "relay-inspect" && tc.promoted {
						renewed = true
						r.Inspection.Status.Continuity.Epoch = 2
					}
				case "relay-isolate":
					isolateAt = clock.Elapsed()
					if extra.IsolationSeconds != RelayLossIsolationSeconds() {
						t.Fatal(extra)
					}
					if tc.unknown {
						r.Certainty = operator.Unknown
					}
				case "relay-start", "relay-loss-stop":
					renewed = tc.name != "rejoin-failed"
					if tc.name == "unknown-start" {
						r.Certainty = operator.Unknown
					}
				case "relay-restore":
					if tc.name == "unknown-restore" {
						r.Certainty = operator.Unknown
					}
					if strings.HasPrefix(tc.name, "restore-failed") {
						return r, fault("host-command-failed")
					}
				}
				return r, nil
			}
			scenario, _ := operator.BuildScenarioIdentity(operator.Scenario{SchemaVersion: 1, RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, Caller: operator.Identity{SourceCommit: strings.Repeat("a", 40)}, Judge: operator.Identity{SourceCommit: strings.Repeat("a", 40)}}, "")
			collect := func(context.Context, time.Duration) (string, []json.RawMessage, error) {
				if isolateAt > 0 && clock.Elapsed()-isolateAt != 2*relayloss.Budget {
					t.Fatal("observation changed", clock.Elapsed()-isolateAt)
				}
				ops = append(ops, "collect")
				if tc.name == "unknown-collect" {
					return "", nil, fault("command-result-unknown")
				}
				var wire strings.Builder
				var samples []json.RawMessage
				appendJSON := func(v any) {
					raw, e := json.Marshal(v)
					if e != nil {
						t.Fatal(e)
					}
					wire.Write(raw)
					wire.WriteByte('\n')
				}
				base := clock.Now.Add(-clock.Elapsed())
				for i := 1; i <= 50; i++ {
					s := loadgen.Stats{At: base.Add(time.Duration(i) * time.Second), Interval: time.Second, GOMAXPROCS: 2, Lateness: loadgen.Lateness{Packets: 100}}
					m := loadgen.Message{Type: "stats", PID: 42, Stats: &s}
					raw, _ := json.Marshal(m)
					samples = append(samples, raw)
					appendJSON(m)
				}
				for i, id := range sessions {
					c := &callharness.Report{StartedAt: base, FirstVideoSentAt: time.Second, HungUpAt: 50 * time.Second, SentVideo: callharness.SentTrack{Frames: 100}, Tracks: []callharness.TrackReport{{Kind: "audio", SSRC: 1, Packets: 100, MediaGap: 20 * time.Millisecond, LastArrival: 50 * time.Second}, {Kind: "video", SSRC: 2, Packets: 100, MediaGap: 20 * time.Millisecond, LastArrival: 50 * time.Second, Video: &callharness.VideoReport{}}}}
					var episodes []callharness.RelayRestartEpisode
					affected := i < 2
					if tc.second {
						affected = i < 3
					}
					if affected && tc.name != "target-insufficient" {
						c.ICERestarts = 1
						candidate := "192.0.2.2:3478"
						if tc.second {
							candidate = "192.0.2.1:3478"
						}
						e := callharness.RelayRestartEpisode{RestartStartNS: 32 * time.Second, AppliedNS: 33 * time.Second, Generation: 1, AnswerCandidate: candidate, LastBeforeNS: 30 * time.Second, FirstVideoNS: 34 * time.Second, FirstAudioNS: 34 * time.Second, ArrivalsBeforeApplied: []callharness.RelayArrivalCount{{Kind: "audio", SSRC: 1}, {Kind: "video", SSRC: 2}}}
						if tc.name == "slow" {
							e.FirstVideoNS = 36 * time.Second
						}
						if tc.name == "unrecovered" {
							e.FirstVideoNS = 0
							e.FirstAudioNS = 0
						}
						if tc.name != "missing" {
							episodes = []callharness.RelayRestartEpisode{e}
						}
					}
					appendJSON(struct {
						loadgen.Message
						Episodes []callharness.RelayRestartEpisode `json:"relayLossEpisodes,omitempty"`
					}{loadgen.Message{Type: "call", PID: 42, CallID: i, Session: id, Call: c}, episodes})
				}
				appendJSON(loadgen.Message{Type: "done", PID: 42})
				return wire.String(), samples, nil
			}
			input := relayjudge.Input{RunID: "r", Scenario: scenario, PID: 42, Sessions: sessions}
			out := RunRelayLossGate(context.Background(), g.c, RelayLossPorts{ProcedurePorts: p, Input: input, Collect: collect, Judge: func(_ context.Context, in relayjudge.Input) (relayjudge.Result, error) {
				if strings.Contains(tc.name, "judge-missing") {
					return relayjudge.Result{}, errors.New("judge unavailable")
				}
				return relayjudge.Judge(in), nil
			}}, 20*time.Minute)
			if out.Process.RelayLoss == nil || len(out.Process.RelayLoss.Calls) != 4 || out.Process.RelayLoss.Outcome != tc.outcome {
				t.Fatal(out, ops)
			}
			if tc.cause != "" && !slices.Contains(out.Process.RelayLoss.Causes, tc.cause) {
				t.Fatal(out, ops)
			}
			if tc.unknown {
				if len(ops) != 4 || ops[3] != "relay-isolate:relay" || out.Reason != "command-result-unknown" {
					t.Fatal(ops, out)
				}
				return
			}
			if tc.name == "unknown-restore" || tc.name == "unknown-start" || tc.name == "unknown-collect" {
				want := 6
				if tc.name == "unknown-collect" {
					want = 5
				}
				if tc.name == "unknown-start" {
					want = 7
				}
				if len(ops) != want || out.Reason != "command-result-unknown" {
					t.Fatal(ops, out)
				}
				return
			}
			if tc.name == "target-insufficient" {
				if slices.Contains(ops, "relay-isolate:relay") || out.Reason != "relay-loss-target-insufficient" {
					t.Fatal(ops, out)
				}
				return
			}
			target := "relay"
			if tc.second {
				target = "relay-two"
			}
			if slices.Index(ops, "collect") > slices.Index(ops, "relay-restore:"+target) {
				t.Fatal("restored before observation collection", ops)
			}
			if tc.name == "rejoin-failed" {
				return
			}
			if strings.HasPrefix(tc.name, "restore-failed") {
				if len(ops) != 6 {
					t.Fatal(ops)
				}
				return
			}
			if tc.second {
				if !slices.Contains(ops, "relay-start:relay-two") || slices.Contains(ops, "relay-loss-stop:relay") {
					t.Fatal(ops)
				}
			} else if !slices.Contains(ops, "relay-replenish:relay") || slices.Contains(ops, "relay-loss-stop:relay") == tc.promoted {
				t.Fatal(fmt.Sprint(ops))
			}
		})
	}
}
