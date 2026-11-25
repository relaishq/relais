package operatorgates

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/internal/relayloss"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

const relayLossStepCap = 60 * time.Second
const relayLossCollectionCap = 30 * time.Second
const relayLossRejoinCap = 60 * time.Second
const relayLossSafety = 10 * time.Second

// Include bounded caller publication in the margin: observation must finish
// before firewall removal. Service/epoch/rejoin waits happen after removal.
func RelayLossIsolationSeconds() int {
	return int((relayLossStepCap + 2*relayloss.Budget + relayLossCollectionCap + relayLossStepCap + relayLossSafety) / time.Second)
}

// Names are committed by the independently hash-checked boot template's run.sh.
var relayLossSlots = map[string]string{"relay": "preview", "relay-two": "preview-two"}

type RelayLossPorts struct {
	ProcedurePorts
	Input   relayjudge.Input
	Collect func(context.Context, time.Duration) (string, []json.RawMessage, error)
	Judge   func(context.Context, relayjudge.Input) (relayjudge.Result, error)
}

// RunRelayLossGate has no distributed arm/feed or ordinary media pool. It uses
// the same step latch as the other gates, and never retries an unknown effect.
func RunRelayLossGate(ctx context.Context, c *ProcessConfig, p RelayLossPorts, end time.Duration) ProcedureResult {
	g := newProcedure(c, p.ProcedurePorts, end)
	in := p.Input
	in.Version = 1
	in.Restore = "missing"
	out := ProcedureResult{Status: operator.Inconclusive}
	var procErr error
	var priorEpoch uint64
	var isolated, callLoss bool
	step := func(op, slot string, extra ProcessExtra) (StepResult, error) {
		return g.step(ctx, op, slot, min(g.end, g.p.Clock.Elapsed()+relayLossStepCap), extra)
	}
	run := func() error {
		if p.Collect == nil || p.Judge == nil || len(in.Sessions) != 4 {
			return fault("relay-loss-missing")
		}
		if err := p.Sleep(ctx, CalibrationWarmup); err != nil {
			return err
		}
		endpoints := map[string]string{}
		epochs := map[string]uint64{}
		for _, slot := range []string{"relay", "relay-two"} {
			r, err := step("relay-status", slot, ProcessExtra{})
			if err != nil {
				return err
			}
			if !r.Inspection.Status.Public.IsValid() || r.Inspection.Status.Continuity == nil {
				return fault("relay-loss-missing")
			}
			endpoints[slot] = r.Inspection.Status.Public.String()
			epochs[slot] = r.Inspection.Status.Continuity.Epoch
		}
		// Exactly one control read supplies every preflight assignment.
		statusRow, err := step("status", "shared", ProcessExtra{RelayLossStatus: true})
		status := statusRow.Status
		if err != nil {
			return err
		}
		if !relayLossHealthy(status) {
			return fault("relay-loss-missing")
		}
		counts := map[string]int{}
		seen := map[string]bool{}
		for _, call := range status.Calls {
			if !slices.Contains(in.Sessions, call.ID) || seen[call.ID] || call.Relay == nil || call.Relay.Pending != "" || call.Relay.Generation >= maxSafeEpoch {
				return fault("relay-loss-missing")
			}
			host := ""
			for _, slot := range []string{"relay", "relay-two"} {
				if call.Relay.Slot == relayLossSlots[slot] {
					host = slot
				}
			}
			if host == "" {
				return fault("relay-loss-missing")
			}
			seen[call.ID] = true
			counts[host]++
			in.Preflight = append(in.Preflight, relayjudge.Assignment{Session: call.ID, Slot: host, Generation: call.Relay.Generation})
		}
		if len(seen) != 4 || len(status.Calls) != 4 {
			return fault("relay-loss-missing")
		}
		in.TargetSlot = "relay"
		if counts["relay-two"] > counts["relay"] {
			in.TargetSlot = "relay-two"
		}
		survivor := "relay-two"
		if in.TargetSlot == "relay-two" {
			survivor = "relay"
		}
		in.SurvivorEndpoint = endpoints[survivor]
		priorEpoch = epochs[in.TargetSlot]
		if counts[in.TargetSlot] < 2 {
			in.TargetInsufficient = true
			return nil
		}
		if p.Broker != nil {
			if err := p.Broker.CheckStopped(ctx); err != nil {
				return err
			}
		}
		if _, err = step("relay-isolate", in.TargetSlot, ProcessExtra{IsolationSeconds: RelayLossIsolationSeconds()}); err != nil {
			return err
		}
		if g.unknown {
			return fault("command-result-unknown")
		}
		isolated = true
		// The receipt is a procedure bound only; media grading stays caller-local.
		if err = p.Sleep(ctx, 2*relayloss.Budget); err != nil {
			return err
		}
		return nil
	}
	procErr = run()
	if !g.unknown && p.Collect != nil {
		raw, samples, err := p.Collect(ctx, min(g.end, p.Clock.Elapsed()+relayLossCollectionCap))
		in.CallerJSONL = raw
		in.Samples = samples
		if err != nil && procErr == nil {
			procErr = err
		}
		if code(err) == "command-result-unknown" {
			g.unknown = true
		}
	}
	if isolated && !g.unknown {
		in.Restore = "failed"
		if _, err := step("relay-restore", in.TargetSlot, ProcessExtra{}); err != nil {
			procErr = err
		} else {
			restore := func() error {
				if in.TargetSlot == "relay-two" {
					if _, err := step("relay-start", "relay-two", ProcessExtra{}); err != nil {
						return err
					}
				} else {
					// Healing may retain the cut-off epoch. Let a promoted standby survive;
					// otherwise stop the old active and let the waiting standby claim it.
					inspectEnd := min(g.end, p.Clock.Elapsed()+relayLossRejoinCap)
					var r StepResult
					var err error
					for p.Clock.Elapsed() < inspectEnd {
						r, err = g.step(ctx, "relay-inspect", "relay", inspectEnd, ProcessExtra{})
						if err == nil {
							break
						}
						if g.unknown || r.Certainty != operator.Failed {
							return err
						}
						if err = p.Sleep(ctx, min(500*time.Millisecond, inspectEnd-p.Clock.Elapsed())); err != nil {
							return err
						}
					}
					if err != nil || p.Clock.Elapsed() >= inspectEnd {
						return fault("relay-loss-restore-failed")
					}
					if r.Inspection.Status.Continuity == nil {
						return fault("relay-loss-restore-failed")
					}
					if r.Inspection.Status.Continuity.Epoch <= priorEpoch {
						if _, err = step("relay-loss-stop", "relay", ProcessExtra{ActiveUnit: r.Inspection.Active}); err != nil {
							return err
						}
					}
				}
				rejoinEnd := min(g.end, p.Clock.Elapsed()+relayLossRejoinCap)
				joined := false
				for p.Clock.Elapsed() < rejoinEnd {
					target, readErr := g.step(ctx, "relay-status", in.TargetSlot, rejoinEnd, ProcessExtra{})
					if readErr != nil && (g.unknown || target.Certainty != operator.Failed) {
						return readErr
					}
					current := target.Inspection.Status
					if readErr != nil || current.Continuity == nil || current.Continuity.Epoch <= priorEpoch || current.Stats.Workers < 2 {
						if err := p.Sleep(ctx, min(500*time.Millisecond, rejoinEnd-p.Clock.Elapsed())); err != nil {
							return err
						}
						continue
					}
					r, err := g.step(ctx, "status", "shared", rejoinEnd, ProcessExtra{RelayLossStatus: true, RecoveryStatus: true})
					if code(err) == "known-call-loss" {
						callLoss = true
						if g.unknown {
							return err
						}
					} else if err != nil && (g.unknown || r.Certainty != operator.Failed) {
						return err
					}
					if (err == nil || code(err) == "known-call-loss") && relayLossHealthy(r.Status) {
						joined = true
						break
					}
					if err = p.Sleep(ctx, min(500*time.Millisecond, rejoinEnd-p.Clock.Elapsed())); err != nil {
						return err
					}
				}
				if !joined {
					return fault("relay-loss-restore-failed")
				}
				if in.TargetSlot == "relay" {
					if _, err := step("relay-replenish", "relay", ProcessExtra{}); err != nil {
						return err
					}
				}
				return nil
			}
			if err := restore(); err != nil {
				procErr = err
			} else {
				in.Restore = "restored"
			}
		}
	} else if !isolated && !g.unknown {
		in.Restore = "restored"
	}
	if callLoss && procErr == nil {
		procErr = fault("known-call-loss")
	}
	if in.TargetInsufficient {
		out.Reason = "relay-loss-target-insufficient"
	}
	if isolated && in.Restore == "failed" && !g.unknown {
		out.Reason = "relay-loss-restore-failed"
	}
	if g.unknown {
		in.Restore = "missing"
		out.Reason = "command-result-unknown"
	} else if procErr != nil && out.Reason == "" {
		out.Reason = code(procErr)
	}
	if p.Judge != nil {
		judged, err := p.Judge(ctx, in)
		if err == nil {
			out.Process.RelayLoss = &judged
			out.Status = operator.Passed
			if judged.Outcome == "fail" {
				out.Status = operator.Failure
			} else if judged.Outcome != "pass" {
				out.Status = operator.Inconclusive
			}
			out.Media = &MediaSummary{Status: out.Status, ExpectedCalls: 4, CompletedCalls: len(judged.Calls)}
			if len(judged.Causes) > 0 && out.Reason == "" {
				out.Reason = judged.Causes[0]
			}
		}
	}
	if out.Process.RelayLoss == nil {
		missing := relayjudge.Result{RunID: in.RunID, Outcome: "inconclusive", Causes: []string{"relay-loss-missing"}, Calls: make([]relayjudge.CallResult, 4)}
		for i := range missing.Calls {
			row := &missing.Calls[i]
			row.Ordinal = i
			row.Role = "unknown"
			row.Outcome = "inconclusive"
			row.Causes = []string{"relay-loss-missing"}
			if i < len(in.Sessions) {
				row.Session = in.Sessions[i]
			}
		}
		out.Process.RelayLoss = &missing
		if out.Reason == "" {
			out.Reason = "relay-loss-missing"
		}
	}
	if in.Restore == "failed" && !g.unknown {
		out.Status = operator.Failure
		out.Process.RelayLoss.Outcome = "fail"
		for i := range out.Process.RelayLoss.Calls {
			row := &out.Process.RelayLoss.Calls[i]
			row.Outcome = "fail"
			if !slices.Contains(row.Causes, "relay-loss-restore-failed") {
				row.Causes = append(row.Causes, "relay-loss-restore-failed")
			}
		}
		if !slices.Contains(out.Process.RelayLoss.Causes, "relay-loss-restore-failed") {
			out.Process.RelayLoss.Causes = append(out.Process.RelayLoss.Causes, "relay-loss-restore-failed")
		}
	}
	if procErr != nil || g.unknown {
		if out.Status != operator.Failure {
			out.Status = operator.Inconclusive
		}
	}
	if code(procErr) == "known-call-loss" || callLoss {
		out.Status = operator.Failure
		out.Process.RelayLoss.Outcome = "fail"
		if !slices.Contains(out.Process.RelayLoss.Causes, "relay-loss-call-lost") {
			out.Process.RelayLoss.Causes = append(out.Process.RelayLoss.Causes, "relay-loss-call-lost")
		}
	}
	out.Completed = !in.TargetInsufficient && procErr == nil && !g.unknown && in.Restore == "restored"
	if out.Media == nil {
		out.Media = &MediaSummary{Status: out.Status, ExpectedCalls: 4}
	} else {
		out.Media.Status = out.Status
	}
	out.Process.CompletedSteps = g.evidence.CompletedSteps
	return out
}
func relayLossHealthy(s controlplane.Status) bool {
	if !s.Ready || len(s.Relays) != 2 {
		return false
	}
	for _, name := range []string{"preview", "preview-two"} {
		if !slices.ContainsFunc(s.Relays, func(r controlplane.RelayStatus) bool {
			return r.Name == name && !r.Lost && !r.Pending && !r.Draining && r.Error == ""
		}) {
			return false
		}
	}
	return true
}
