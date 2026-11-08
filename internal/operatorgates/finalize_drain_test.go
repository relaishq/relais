package operatorgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func finalDrainFixture(t *testing.T) (*Session, *FakeClock, *EvidenceRuntime) {
	t.Helper()
	s, c := fixtureSession(t)
	s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}}
	s.retainClockAttempt(clocks.Evidence{Version: 2, Clocks: map[string]clocks.Sample{"shared": {TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000, LeapStatus: "N"}, {UnixSeconds: 1793930021, LeapStatus: "N"}}}}}})
	pool := &EvidenceRuntime{RunID: "run", Judge: finalCoverageJudge{}, Coverage: func([]observe.CoverageWindow) *clocks.CoverageEvidence {
		e := s.coverageSnapshot(clocks.FixedCoverageBounds())
		e.Windows = []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010250000000", EndUnixNS: "1793930020250000000"}}
		e.Evaluate()
		return e
	}}
	return s, c, pool
}

func TestFinalDrainQualificationAndSafety(t *testing.T) {
	for _, mode := range []string{"closed", "open", "read-error", "missing-reader", "known-failure", "refusal", "break"} {
		t.Run(mode, func(t *testing.T) {
			s, c, pool := finalDrainFixture(t)
			if mode == "known-failure" {
				pool.Judge = mediaJudgeFunc(func(context.Context, operatorevidence.Pool) (operatorevidence.Summary, error) {
					return operatorevidence.Summary{Status: operator.Failure}, nil
				})
			}
			before, err := pool.Media(context.Background())
			require.NoError(t, err)
			require.NotEqual(t, operator.Passed, before.Status)
			var coverage *clocks.CoverageEvidence
			var read ClockCoverageReader = func(context.Context, string, string) (string, error) {
				if mode == "read-error" {
					return "", fmt.Errorf("unavailable")
				}
				last := int64(1793930023)
				if mode == "open" {
					last = 1793930021
				}
				if mode == "break" {
					s.clockMissingIntervals = map[string]observe.CoverageBreak{"shared": {FromUnixSeconds: 1793930015, UnixSeconds: 1793930015, Kind: "log-loss"}}
				}
				return packedCoverageSample(t, last, true), nil
			}
			if mode == "missing-reader" {
				read = nil
			}
			out := s.RunSequence(context.Background(), SequencePorts{
				Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
					p := ProcedureResult{Status: before.Status, Completed: true, AwaitingPool: true, Media: before}
					if mode == "refusal" {
						p.Reason, p.Completed, p.AwaitingPool = "precondition-failed", false, false
					}
					return p
				},
				Finalize: func(ctx context.Context, end time.Duration) (*MediaSummary, error) {
					m, e, err := s.finalizePool(ctx, end, pool, clocks.FixedCoverageBounds(), read, func(_ context.Context, d time.Duration) error { c.Advance(d); return nil })
					coverage = e
					return m, err
				},
			})
			require.NotNil(t, out.Pool)
			switch mode {
			case "closed":
				if out.Status != operator.Passed || !coverage.DrainComplete {
					t.Fatal("MUTATION: postjudge-drain", out)
				}
			case "known-failure":
				if out.Status != operator.Failure || out.Pool.Status != operator.Failure {
					t.Fatal("MUTATION: drain-failure-hidden", out)
				}
			case "refusal":
				require.Equal(t, operator.Inconclusive, out.Status)
				require.Equal(t, "precondition-failed", out.Gates[0].Reason)
			default:
				require.Equal(t, operator.Inconclusive, out.Pool.Status)
				require.Equal(t, operator.Inconclusive, out.Status)
			}
			if mode == "open" || mode == "read-error" || mode == "missing-reader" {
				require.Equal(t, before, out.Pool)
			}
		})
	}
}

func TestPrejudgeDrainDeadline(t *testing.T) {
	for _, budget := range []time.Duration{time.Second, 10 * time.Second, 30 * time.Second, PoolReserve - 9*time.Second, PoolReserve} {
		s, c, pool := finalDrainFixture(t)
		s.gates = nil
		s.Budget.end = budget
		original := s.Budget.end
		pool.Judge = mediaJudgeFunc(func(context.Context, operatorevidence.Pool) (operatorevidence.Summary, error) {
			return operatorevidence.Summary{Status: operator.Passed}, nil
		})
		parent, done := context.WithTimeout(context.Background(), time.Minute)
		parentEnd, _ := parent.Deadline()
		out := s.RunSequence(parent, SequencePorts{Finalize: func(ctx context.Context, end time.Duration) (*MediaSummary, error) {
			summary, _, err := s.finalizePool(ctx, end, pool, clocks.FixedCoverageBounds(), func(ctx context.Context, _ string, _ string) (string, error) {
				deadline, _ := ctx.Deadline()
				require.False(t, deadline.After(parentEnd))
				return packedCoverageSample(t, 1793930021, true), nil
			}, func(_ context.Context, d time.Duration) error { c.Advance(d); return nil })
			require.NoError(t, err)
			return summary, err
		}})
		done()
		if c.Elapsed() != min(max(time.Duration(0), budget-(PoolReserve-10*time.Second)), 10*time.Second) || s.Budget.end != original || out.Status != operator.Passed || out.Pool == nil || out.Pool.Status != operator.Passed {
			t.Fatal("MUTATION: drain-deadline-extension", budget, c.Elapsed(), s.Budget.end, out)
		}
	}
}

// Simulate a judge that needs one second and refuses an exhausted context.
// Release leaves only five seconds of the immutable session budget.
func TestPrejudgeDrainReservesJudge(t *testing.T) {
	for _, status := range []operator.Verdict{operator.Failure, operator.Passed} {
		t.Run(string(status), func(t *testing.T) {
			s, c, pool := finalDrainFixture(t)
			reads := 0
			pool.Judge = mediaJudgeFunc(func(ctx context.Context, _ operatorevidence.Pool) (operatorevidence.Summary, error) {
				if ctx.Err() != nil || s.Budget.Remaining() < time.Second {
					return operatorevidence.Summary{}, fmt.Errorf("judge-incomplete")
				}
				c.Advance(time.Second)
				return operatorevidence.Summary{Status: status}, nil
			})
			out := s.RunSequence(context.Background(), SequencePorts{
				Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
					return ProcedureResult{Status: operator.Inconclusive, Completed: true, AwaitingPool: true, Media: &MediaSummary{Status: operator.Inconclusive}}
				},
				ReleaseCallers: func(context.Context, string, time.Duration) bool {
					c.Advance(s.Budget.Remaining() - 5*time.Second)
					return true
				},
				Finalize: func(ctx context.Context, end time.Duration) (*MediaSummary, error) {
					m, _, err := s.finalizePool(ctx, end, pool, clocks.FixedCoverageBounds(), func(context.Context, string, string) (string, error) {
						reads++
						return packedCoverageSample(t, 1793930021, true), nil
					}, func(_ context.Context, d time.Duration) error { c.Advance(d); return nil })
					return m, err
				},
			})
			if out.Status != status || out.Pool == nil || out.Pool.Status != status || reads != 0 {
				t.Fatal("MUTATION: drain-judge-reserve-dropped", status, out, reads)
			}
		})
	}
}
