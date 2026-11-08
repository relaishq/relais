package operatorgates

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestFinalizeBudgetAndStatusCharacterization(t *testing.T) {
	for _, mode := range []string{"normal", "exhausted", "cancelled", "late-pass", "late-failure", "failure", "refusal"} {
		t.Run(mode, func(t *testing.T) {
			s, c := fixtureSession(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "exhausted" {
				c.Advance(SessionMaximum)
			}
			if mode == "cancelled" {
				cancel()
			}
			originalEnd := s.Budget.end
			order := []string{}
			out := s.RunSequence(ctx, SequencePorts{
				Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
					p := ProcedureResult{Status: operator.Passed, Completed: true, Media: &MediaSummary{Status: operator.Passed}}
					if mode == "failure" {
						p.Status = operator.Failure
					}
					if mode == "refusal" {
						p.Status, p.Reason, p.Completed = operator.Inconclusive, "precondition-failed", false
					}
					return p
				},
				Finalize: func(_ context.Context, end time.Duration) (*MediaSummary, error) {
					order = append(order, "judge")
					require.Equal(t, min(originalEnd, c.Elapsed()+PoolReserve), end)
					status := operator.Passed
					if mode == "late-pass" || mode == "late-failure" {
						c.Advance(end - c.Elapsed())
					}
					if mode == "late-failure" {
						status = operator.Failure
					}
					return &MediaSummary{Status: status}, nil
				},
				FinalizeDiagnostics: func(context.Context, time.Duration) { order = append(order, "diagnostics") },
			})
			require.Equal(t, originalEnd, s.Budget.end)
			if mode == "exhausted" || mode == "cancelled" {
				require.Empty(t, order)
				require.Nil(t, out.Pool)
				return
			}
			require.Equal(t, []string{"judge", "diagnostics"}, order)
			switch mode {
			case "late-pass":
				require.Nil(t, out.Pool)
				require.Equal(t, operator.Inconclusive, out.Status)
			case "failure", "late-failure":
				require.Equal(t, operator.Failure, out.Status)
			case "refusal":
				require.Equal(t, operator.Inconclusive, out.Status)
				require.Equal(t, "precondition-failed", out.Gates[0].Reason)
			default:
				require.Equal(t, operator.Passed, out.Status)
			}
		})
	}
}

func TestDrainLatestCompletenessAndAttemptCap(t *testing.T) {
	s, _ := fixtureSession(t)
	s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}}
	s.retainClockAttempt(clocks.Evidence{Version: 2, Clocks: map[string]clocks.Sample{"shared": {TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000, LeapStatus: "N"}, {UnixSeconds: 1793930020, LeapStatus: "N"}}}}}})
	reads := 0
	read := func(context.Context, string, string) (string, error) {
		reads++
		return packedCoverageSample(t, 1793930023, false), nil
	}
	e := s.collectCoverage(context.Background(), clocks.FixedCoverageBounds(), []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010000000000", EndUnixNS: "1793930020000000000"}}, read, func(context.Context, time.Duration) error { return nil }, s.Budget.end)
	if e.DrainComplete || len(e.DrainAttempts) != 64 || reads != 64*4 {
		t.Fatal("latest completeness or drain cap changed", fmt.Sprint(e.DrainComplete), len(e.DrainAttempts), reads)
	}
}

func packedCoverageSample(t *testing.T, last int64, complete bool) string {
	t.Helper()
	sample := clocks.Sample{MeasuredUnixNS: "1793930030000000000", TrackingLog: &clocks.TrackingLog{ReadComplete: complete, Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000, LeapStatus: "N"}, {UnixSeconds: last, LeapStatus: "N"}}}}
	reply := hostReply(t, oracleJSON(sample))
	var output operator.HostOutput
	require.NoError(t, observe.Decode(reply.Payload, &output, observe.Contract{"stdout"}))
	return output.Stdout
}

func TestClosedCoverageFinalizationBytes(t *testing.T) {
	s, _ := fixtureSession(t)
	s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}}
	s.retainClockAttempt(clocks.Evidence{Version: 2, Clocks: map[string]clocks.Sample{"shared": {TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000, LeapStatus: "N"}, {UnixSeconds: 1793930030, LeapStatus: "N"}}}}}})
	windows := []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010000000000", EndUnixNS: "1793930020000000000"}}
	var wire []byte
	j := mediaJudgeFunc(func(_ context.Context, p operatorevidence.Pool) (operatorevidence.Summary, error) {
		var err error
		wire, err = operatorevidence.MarshalJudgeInput(p)
		return operatorevidence.Summary{Status: operator.Passed}, err
	})
	pool := &EvidenceRuntime{RunID: "run", Judge: j, Coverage: func([]observe.CoverageWindow) *clocks.CoverageEvidence {
		e := s.coverageSnapshot(clocks.FixedCoverageBounds())
		e.Windows = windows
		e.Evaluate()
		return e
	}}
	media, coverage, err := s.finalizePool(context.Background(), s.Budget.PoolEnd(), pool, clocks.FixedCoverageBounds(), func(context.Context, string, string) (string, error) { t.Fatal("closed tail read"); return "", nil }, func(context.Context, time.Duration) error { return nil })
	require.NoError(t, err)
	raw, err := json.Marshal(struct {
		Coverage *clocks.CoverageEvidence
		Input    json.RawMessage
		Report   ResourceResult
	}{coverage, wire, ResourceResult{SequenceResult: SequenceResult{Pool: media}, ClockCoverage: coverage}})
	require.NoError(t, err)
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != "2a646c819908dade4960a36cdf2240953db72dc686f9ccac844cc7719937d2eb" {
		t.Fatal("MUTATION: closed-finalization-bytes", got)
	}
}

type mediaJudgeFunc func(context.Context, operatorevidence.Pool) (operatorevidence.Summary, error)

func (f mediaJudgeFunc) Judge(ctx context.Context, p operatorevidence.Pool) (operatorevidence.Summary, error) {
	return f(ctx, p)
}
