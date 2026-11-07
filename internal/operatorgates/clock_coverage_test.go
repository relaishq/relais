package operatorgates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestClockCoverageRetainsRowsAfterFailedBatch(t *testing.T) {
	r, p, _ := runtimeFixture(t)
	s := r.Session
	s.capabilities[operator.HostReadBatch] = true
	original := p.respond
	p.respond = func(req operator.Request) operator.Reply {
		if req.Action != "clock" {
			return original(req)
		}
		out := operator.HostReadBatchOutput{}
		for _, slot := range []string{"relay", "w1", "w2", "shared"} {
			raw := `{"errorBoundNs":1,"rateErrorPpm":0,"measuredUnixNs":"1793930000000000000","trackingLog":{"cursor":{"position":42,"anchor":"abc"},"rows":[{"unixSeconds":1793930000,"maxErrorSeconds":0.0001,"leapStatus":"N"}],"breaks":[],"readComplete":true}}`
			if slot == "w2" {
				raw = `{}`
			}
			child := hostReply(t, []byte(raw))
			child.ID = slot
			out.Replies = append(out.Replies, child)
		}
		return resourceTestReply(out)
	}
	require.Error(t, r.VerifyClocks(context.Background(), time.Minute))
	require.Empty(t, r.ClockSamples, "successful cache must remain unchanged")
	e := s.coverageSnapshot(*r.Bounds)
	require.Len(t, e.Attempts, 1)
	require.Len(t, e.Timelines()["relay"].Rows, 1)
	require.Contains(t, string(s.clockCursors["relay"]), `"position":42`)
	require.False(t, s.Latched())
}
func resourceTestReply(v any) operator.Reply {
	raw, _ := json.Marshal(v)
	zero := 0
	return operator.Reply{Transport: operator.Succeeded, ExitCode: &zero, Payload: raw}
}

func TestClockCoverageDrainConditionAndTimeout(t *testing.T) {
	for _, mode := range []string{"ready", "cut-short", "timeout", "read-error", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			s, c := fixtureSession(t)
			s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}, "w1": {"w1"}, "w2": {"w2"}}
			w := []observe.CoverageWindow{{Kind: "move", Worker: "w1", StartUnixNS: "1793930010250000000", EndUnixNS: "1793930020250000000"}}
			reads := 0
			reader := func(ctx context.Context, slot, script string) (string, error) {
				reads++
				require.NotContains(t, script, "kill")
				if mode == "read-error" {
					return "", fmt.Errorf("unavailable")
				}
				complete := mode != "cut-short" || reads > 4
				end := int64(1793930022)
				if mode == "timeout" {
					end = 1793930021
				}
				sample := clocks.Sample{ErrorBoundNS: 1, MeasuredUnixNS: "1793930035000000000", TrackingLog: &clocks.TrackingLog{ReadComplete: complete, Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000, LeapStatus: "N"}, {UnixSeconds: end, LeapStatus: "N"}}, Cursor: json.RawMessage(`{"position":42,"anchor":"abc"}`)}}
				raw, _ := json.Marshal(sample)
				reply := hostReply(t, raw)
				var out operator.HostOutput
				require.NoError(t, json.Unmarshal(reply.Payload, &out))
				return out.Stdout, nil
			}
			before := s.Final(nil)
			var read ClockCoverageReader = reader
			if mode == "unavailable" {
				read = nil
			}
			e := s.collectCoverage(context.Background(), clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000, DriftPPM: 100}, w, read, func(_ context.Context, d time.Duration) error { c.Advance(d); return nil })
			require.Equal(t, mode == "ready" || mode == "cut-short", e.DrainComplete)
			switch mode {
			case "ready":
				require.Equal(t, 4, reads)
			case "cut-short":
				require.Equal(t, 8, reads)
			case "unavailable":
				require.Zero(t, reads)
			default:
				require.Equal(t, 40, reads)
			}
			require.False(t, s.Latched())
			require.Equal(t, before, s.Final(nil), "diagnostics must not change latch, traces or final verdict")
			require.Empty(t, s.clockCursors, "diagnostic cursor must not acknowledge policy reads")
		})
	}
}

// This spy uses the exact projection used by PinnedJudge. Its verdict changes
// when media wire bytes change, so an input-insensitive fake cannot satisfy it.
type coverageJudge struct {
	expected []byte
	inputs   [][]byte
	order    *[]string
}

func (j *coverageJudge) Judge(_ context.Context, p operatorevidence.Pool) (operatorevidence.Summary, error) {
	if j.order != nil {
		*j.order = append(*j.order, "judge")
	}
	raw, err := operatorevidence.MarshalJudgeInput(p)
	if err != nil {
		return operatorevidence.Summary{}, err
	}
	j.inputs = append(j.inputs, raw)
	status := operator.Passed
	if !bytes.Equal(j.expected, raw) {
		status = operator.Failure
	}
	return operatorevidence.Summary{Status: status}, nil
}
func TestClockCoverageTransportsQualification(t *testing.T) {
	expected, err := operatorevidence.MarshalJudgeInput(operatorevidence.Pool{RunID: "run", Phases: []operatorevidence.MediaInput{}})
	require.NoError(t, err)
	j := &coverageJudge{expected: expected}
	p := &EvidenceRuntime{RunID: "run", Judge: j}
	before, err := p.Media(context.Background())
	require.NoError(t, err)
	require.Equal(t, operator.Passed, before.Status)
	p.Coverage = func([]observe.CoverageWindow) *clocks.CoverageEvidence {
		return &clocks.CoverageEvidence{Version: 2, Bounds: clocks.Bounds{DriftPPM: math.NaN()}}
	}
	after, err := p.Media(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	require.NotEqual(t, j.inputs[0], j.inputs[1])
	require.Contains(t, string(j.inputs[1]), `"clockCoverage":{"results":[],"falsified":false}`)
	p.RunID = "changed-media"
	changed, err := p.Media(context.Background())
	require.NoError(t, err)
	require.Equal(t, operator.Failure, changed.Status, "negative control: the spy must discriminate actual media changes")
}

func TestClockCoverageReadsOnlyAfterFinalJudgment(t *testing.T) {
	s, c := fixtureSession(t)
	s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}}
	windows := []observe.CoverageWindow{{Kind: "move", Worker: "w1", StartUnixNS: "1793930010250000000", EndUnixNS: "1793930020250000000"}}
	bounds := clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000, DriftPPM: 100}
	expected, err := operatorevidence.MarshalJudgeInput(operatorevidence.Pool{RunID: "run", Phases: []operatorevidence.MediaInput{}, ClockCoverage: &clocks.CoverageEvidence{Bounds: bounds, Windows: windows}})
	require.NoError(t, err)
	order := []string{}
	j := &coverageJudge{expected: expected, order: &order}
	result := ResourceResult{}
	pool := &EvidenceRuntime{RunID: "run", Judge: j, Coverage: func([]observe.CoverageWindow) *clocks.CoverageEvidence {
		e := s.coverageSnapshot(bounds)
		e.Windows = windows
		e.Evaluate()
		result.ClockCoverage = e
		return e
	}}
	for i := 0; i < 2; i++ {
		m, err := pool.Media(context.Background())
		require.NoError(t, err)
		require.Equal(t, operator.Passed, m.Status)
	}
	require.Equal(t, []string{"judge", "judge"}, order)
	read := func(context.Context, string, string) (string, error) {
		order = append(order, "read")
		c.Advance(time.Second)
		sample := clocks.Sample{MeasuredUnixNS: "1793930030000000000", TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000, LeapStatus: "N"}, {UnixSeconds: 1793930023, LeapStatus: "N"}}}}
		raw, err := json.Marshal(sample)
		require.NoError(t, err)
		reply := hostReply(t, raw)
		var output operator.HostOutput
		require.NoError(t, json.Unmarshal(reply.Payload, &output))
		return output.Stdout, nil
	}
	m, err := pool.Media(context.Background())
	result.ClockCoverage = s.collectCoverage(context.Background(), bounds, result.ClockCoverage.Windows, read, func(_ context.Context, d time.Duration) error { c.Advance(d); return nil })
	require.NoError(t, err)
	require.Equal(t, operator.Passed, m.Status)
	require.Equal(t, []string{"judge", "judge", "judge", "read", "read", "read", "read"}, order)
	require.True(t, result.ClockCoverage.DrainComplete)
	require.Len(t, result.ClockCoverage.DrainAttempts, 1)
	require.Empty(t, result.ClockCoverage.Attempts)
}

func TestClockCoverageOverflowKeepsPrefixAndSeparateBudgets(t *testing.T) {
	s, _ := fixtureSession(t)
	s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}}
	sample := func(a, b int64) clocks.Evidence {
		return clocks.Evidence{Version: 2, Clocks: map[string]clocks.Sample{"shared": {TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: []clocks.TrackingRow{{UnixSeconds: 1793930000 + a, LeapStatus: "N", MaxErrorSeconds: .0001}, {UnixSeconds: 1793930000 + b, LeapStatus: "N", MaxErrorSeconds: .0001}}}}}}
	}
	for i := 0; i < clocks.CoverageAttemptLimit; i++ {
		s.retainClockAttempt(sample(0, 30))
	}
	s.retainClockAttempt(sample(0, 30))
	require.Empty(t, s.clockMissingIntervals, "dropping an exact replay loses no interval")
	s.retainClockAttempt(sample(31, 32))
	s.retainDrainClockAttempt(sample(33, 40))
	e := s.coverageSnapshot(clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000, DriftPPM: 100})
	e.Windows = []observe.CoverageWindow{{Event: 0, Kind: "move", StartUnixNS: "1793930010000000000", EndUnixNS: "1793930020000000000"}, {Event: 1, Kind: "move", StartUnixNS: "1793930031000000000", EndUnixNS: "1793930032000000000"}}
	e.Evaluate()
	require.Empty(t, e.Results[0].Causes)
	require.Contains(t, e.Results[1].Causes, "clock-coverage-missing")
	require.Equal(t, 2, e.OmittedAttempts)
	require.Len(t, e.DrainAttempts, 1)
	require.Zero(t, e.OmittedDrainAttempts)
	s.clockBrackets = make([]observe.CoverageBracket, 4096)
	s.retainClockBracket("w1", "1793930010000000000", "1793930010000000100")
	after := s.coverageSnapshot(e.Bounds)
	require.Equal(t, 1, after.OmittedBrackets)
	require.Equal(t, 2, after.OmittedAttempts)
	for len(s.clockDrainAttempts) < clocks.CoverageDrainAttemptLimit {
		s.retainDrainClockAttempt(sample(33, 40))
	}
	s.retainDrainClockAttempt(sample(41, 42))
	after = s.coverageSnapshot(e.Bounds)
	after.Windows = e.Windows
	after.Evaluate()
	require.Equal(t, 1, after.OmittedDrainAttempts)
	require.Equal(t, 2, after.OmittedAttempts)
	require.Equal(t, 1, after.OmittedBrackets)
	require.Empty(t, after.Results[0].Causes, "drain overflow erased the early prefix")
}

func TestFinalCoverageLatencyCannotChangePoolDeadlineVerdict(t *testing.T) {
	s, c := fixtureSession(t)
	order := []string{}
	result := s.RunSequence(context.Background(), SequencePorts{
		Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
			return ProcedureResult{Status: operator.Passed, Completed: true, Media: &MediaSummary{Status: operator.Passed}}
		},
		Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
			order = append(order, "judge")
			return &MediaSummary{Status: operator.Passed}, nil
		},
		FinalizeDiagnostics: func(_ context.Context, end time.Duration) {
			order = append(order, "drain")
			c.Advance(end - c.Elapsed())
		},
	})
	require.Equal(t, []string{"judge", "drain"}, order)
	require.NotNil(t, result.Pool)
	require.Equal(t, operator.Passed, result.Pool.Status)
	require.Equal(t, operator.Passed, result.Status)
	require.False(t, s.Latched())
}

func TestClockCoverageFalsificationLatchIsSticky(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprint(overflow), func(t *testing.T) {
			s, _ := fixtureSession(t)
			clean := clocks.Evidence{Version: 2}
			if overflow {
				for range clocks.CoverageAttemptLimit {
					s.retainClockAttempt(clean)
				}
			}
			bad := clocks.Evidence{Version: 2, Falsification: &clocks.Falsification{Records: []clocks.ProbeRecord{{Status: "falsified"}}}}
			s.retainClockAttempt(bad)
			s.retainClockAttempt(clean)
			if !s.coverageSnapshot(clocks.Bounds{}).Falsified {
				t.Fatal("MUTATION: clock-coverage-sticky-falsification")
			}
			s.retainDrainClockAttempt(clean)
			if !s.coverageSnapshot(clocks.Bounds{}).Qualification().Falsified {
				t.Fatal("MUTATION: clock-coverage-sticky-falsification")
			}
		})
	}
	s, _ := fixtureSession(t)
	s.retainClockAttempt(clocks.Evidence{Version: 2})
	if s.coverageSnapshot(clocks.Bounds{}).Falsified {
		t.Fatal("disabled prober falsified")
	}
}

func TestClockCoverageFalsificationUsesCumulativeCounts(t *testing.T) {
	s, _ := fixtureSession(t)
	s.retainClockAttempt(clocks.Evidence{Version: 2, Falsification: &clocks.Falsification{CountsByStatus: map[string]int{"falsified": 1}}})
	if !s.coverageSnapshot(clocks.Bounds{}).Falsified {
		t.Fatal("cumulative falsification lost")
	}
}

type finalCoverageJudge struct{}

func (finalCoverageJudge) Judge(_ context.Context, pool operatorevidence.Pool) (operatorevidence.Summary, error) {
	q := pool.ClockCoverage.Qualification()
	causes := q.WindowCauses(0, 0, false)
	out := operatorevidence.Summary{Status: operator.Passed}
	if len(causes) != 0 {
		out.Status = operator.Inconclusive
		out.UndecidedMetrics = []operatorevidence.UndecidedMetric{{Phase: 0, Event: 0, Metric: "distributed_window", Causes: causes}}
	}
	return out, nil
}
func TestFinalJudgmentRefreshesClockCoverage(t *testing.T) {
	s, _ := fixtureSession(t)
	closed := true
	pool := &EvidenceRuntime{RunID: "run", Judge: finalCoverageJudge{}, Coverage: func([]observe.CoverageWindow) *clocks.CoverageEvidence {
		last := int64(1793930030)
		if !closed {
			last = 1793930020
		}
		return &clocks.CoverageEvidence{Bounds: clocks.FixedCoverageBounds(), FrontendSlot: "shared", ControlSlot: "shared",
			Windows:  []observe.CoverageWindow{{Kind: "move", StartUnixNS: "1793930010000000000", EndUnixNS: "1793930020000000000"}},
			Attempts: []clocks.CoverageAttempt{{Version: 2, Clocks: map[string]observe.CoverageTimeline{"shared": {Rows: []observe.CoverageRow{{UnixSeconds: 1793930000, LeapStatus: "N"}, {UnixSeconds: last, LeapStatus: "N"}}}}}}}
	}}
	before, err := pool.Media(context.Background())
	require.NoError(t, err)
	if before.Status != operator.Passed {
		t.Fatal("final coverage control", before)
	}
	closed = false
	result := s.RunSequence(context.Background(), SequencePorts{
		Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
			return ProcedureResult{Status: operator.Passed, Completed: true, Media: before}
		},
		Finalize: func(ctx context.Context, _ time.Duration) (*MediaSummary, error) { return pool.Media(ctx) },
	})
	if result.Pool == nil || result.Pool.Status != operator.Inconclusive || len(result.Pool.UndecidedMetrics) != 1 || !slices.Contains(result.Pool.UndecidedMetrics[0].Causes, "clock-coverage-missing") {
		t.Fatal("MUTATION: final-coverage-snapshot", result.Pool)
	}
}
