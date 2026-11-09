package operatorgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/operator"
)

func TestEvidenceSnapshotRetainsGatePopulation(t *testing.T) {
	r := &EvidenceRuntime{RunID: "run", ClockReceiptSHA256: "receipt"}
	r.Add(operatorevidence.Phase{Gate: operator.Rollout}, false)
	first := r.snapshot()
	r.Add(operatorevidence.Phase{Gate: operator.ControlRestart}, true)
	second := r.snapshot()
	r.Add(operatorevidence.Phase{Gate: operator.UpgradeRollback}, false)
	if len(first.phases) != 1 || first.missing || len(second.phases) != 2 || !second.missing || len(r.phases) != 3 || first.RunID != r.RunID || first.ClockReceiptSHA256 != r.ClockReceiptSHA256 {
		t.Fatal("MUTATION: gate-media-population", first, second, r)
	}
}

func TestGateMediaRefreshSafety(t *testing.T) {
	for _, mode := range []string{"pass", "error", "nil", "late-pass", "late-failure", "refresh-failure", "prior-failure", "refusal"} {
		t.Run(mode, func(t *testing.T) {
			s, c := coverageFixtureSession(t)
			prior := &MediaSummary{Status: operator.Passed}
			if mode == "prior-failure" {
				prior.Status = operator.Failure
			}
			var deadline time.Time
			out := s.RunSequence(context.Background(), SequencePorts{
				Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
					r := ProcedureResult{Status: operator.Inconclusive, Completed: true, AwaitingPool: true, Media: prior}
					if mode == "refusal" {
						r.Completed, r.AwaitingPool, r.Reason = false, false, "precondition-failed"
					}
					return r
				},
				Finalize: func(ctx context.Context, _ time.Duration) (*MediaSummary, error) {
					deadline, _ = ctx.Deadline()
					return &MediaSummary{Status: operator.Passed}, nil
				},
				RefreshMedia: func(ctx context.Context, _ operator.Gate) (*MediaSummary, error) {
					got, _ := ctx.Deadline()
					if got != deadline {
						t.Fatal("refresh extended judge deadline")
					}
					if mode == "error" {
						return &MediaSummary{Status: operator.Passed}, fmt.Errorf("private-judge-output")
					}
					if mode == "nil" {
						return nil, nil
					}
					if mode == "late-pass" || mode == "late-failure" {
						c.Advance(s.Budget.PoolEnd() - c.Elapsed())
					}
					if mode == "refresh-failure" || mode == "late-failure" {
						return &MediaSummary{Status: operator.Failure}, fmt.Errorf("independent-judge-error")
					}
					return &MediaSummary{Status: operator.Passed}, nil
				},
			})
			want := operator.Inconclusive
			if mode == "pass" {
				want = operator.Passed
			}
			if mode == "refresh-failure" || mode == "prior-failure" || mode == "late-failure" {
				want = operator.Failure
			}
			if out.Status != want || out.Pool.Status != operator.Passed {
				t.Fatal("MUTATION: gate-media-refresh-safety", mode, out)
			}
			if mode == "prior-failure" && out.Procedures[0].Media.Status != operator.Failure {
				t.Fatal("MUTATION: gate-media-prior-failure", out.Procedures[0].Media)
			}
			if mode == "error" && s.Final(out.Gates).Diagnostics[0].Reason != "media-refresh-unavailable" {
				t.Fatal("MUTATION: gate-media-refresh-error-diagnostic", s.Final(out.Gates).Diagnostics)
			}
		})
	}
}

func TestGateMediaRefreshLowersPassedGate(t *testing.T) {
	for _, cause := range []string{"clock-coverage-missing", "clock-coverage-falsified"} {
		t.Run(cause, func(t *testing.T) {
			s, _ := coverageFixtureSession(t)
			s.gates = []operator.Gate{operator.Failover, operator.Load}
			out := s.RunSequence(context.Background(), SequencePorts{
				Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
					return ProcedureResult{Completed: true, Status: operator.Passed, Media: &MediaSummary{Status: operator.Passed}}
				},
				Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
					return &MediaSummary{Status: operator.Passed}, nil
				},
				RefreshMedia: func(context.Context, operator.Gate) (*MediaSummary, error) {
					return &MediaSummary{Status: operator.Inconclusive, UndecidedMetrics: []operatorevidence.UndecidedMetric{{Metric: "first_content", Causes: []string{cause}}}}, nil
				},
			})
			for i, gate := range out.Gates {
				if out.Status != operator.Inconclusive || out.Pool.Status != operator.Passed || gate.Status != operator.Inconclusive || out.Procedures[i].Status != operator.Inconclusive || out.Procedures[i].Media.UndecidedMetrics[0].Causes[0] != cause {
					t.Fatal("MUTATION: gate-media-refresh-passed-status", out)
				}
			}
		})
	}
}
