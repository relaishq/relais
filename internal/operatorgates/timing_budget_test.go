package operatorgates

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

var slowTiming = ClockTiming{PollSeconds: 8, HostReadLatencyMS: 4000}

func TestTimingBudgetDerivation(t *testing.T) {
	reference, err := ClockTiming{}.budget(8)
	require.NoError(t, err)
	stated, err := ClockTiming{PollSeconds: 1, HostReadLatencyMS: 400}.budget(8)
	require.NoError(t, err)
	require.Equal(t, reference, stated)
	slow, err := slowTiming.budget(2)
	require.NoError(t, err)
	for _, tc := range []struct {
		name            string
		got, want, slow time.Duration
	}{
		{"lease-attempt", reference.leaseAttempt(), leaseReadAttempt, 17600 * time.Millisecond},
		{"lease-read-minimum", reference.leaseReadMinimum(), leaseReadMinimum, 4150 * time.Millisecond},
		{"lease-batch-minimum", reference.leaseBatchMinimum(), leaseReadBatchMinimum, 16 * time.Second},
		{"drain-limit", reference.drainLimit(), 10 * time.Second, 32 * time.Second},
		{"judge-reserve", reference.judgeReserve(), 110 * time.Second, 88 * time.Second},
	} {
		if tc.got != tc.want {
			t.Fatalf("MUTATION: timing-reference-%s: got %s want %s", tc.name, tc.got, tc.want)
		}
	}
	for name, d := range map[string][2]time.Duration{
		"lease-attempt":       {slow.leaseAttempt(), 17600 * time.Millisecond},
		"lease-read-minimum":  {slow.leaseReadMinimum(), 4150 * time.Millisecond},
		"lease-batch-minimum": {slow.leaseBatchMinimum(), 16 * time.Second},
		"drain-limit":         {slow.drainLimit(), 32 * time.Second},
		"judge-reserve":       {slow.judgeReserve(), 88 * time.Second},
	} {
		if d[0] != d[1] {
			t.Fatalf("MUTATION: timing-slow-%s: got %s want %s", name, d[0], d[1])
		}
	}
	// The ticket's floor: the drain has at least two polls plus one read.
	require.GreaterOrEqual(t, slow.drainLimit(), 2*slow.poll+slow.read)
}

func TestTimingBudgetRefusesUnfitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		timing ClockTiming
		reads  int
		cause  string
	}{
		{"reference", ClockTiming{}, 8, ""},
		{"slow-batch", slowTiming, 2, ""},
		{"no-poll", ClockTiming{HostReadLatencyMS: 400}, 2, "clock-timing-invalid"},
		{"no-latency", ClockTiming{PollSeconds: 8}, 2, "clock-timing-invalid"},
		{"negative", ClockTiming{PollSeconds: -1, HostReadLatencyMS: 400}, 2, "clock-timing-invalid"},
		{"overflow", ClockTiming{PollSeconds: 1 << 62, HostReadLatencyMS: 1 << 62}, 2, "clock-timing-invalid"},
		{"validity-edge", ClockTiming{PollSeconds: 4, HostReadLatencyMS: 4000}, 2, ""},
		{"validity-short", ClockTiming{PollSeconds: 4, HostReadLatencyMS: 4001}, 2, "clock-timing-lease-validity"},
		{"judge-edge", ClockTiming{PollSeconds: 10, HostReadLatencyMS: 10000}, 2, ""},
		{"judge-short", ClockTiming{PollSeconds: 11, HostReadLatencyMS: 10000}, 2, "clock-timing-judge-reserve"},
		{"restore-edge", ClockTiming{PollSeconds: 4, HostReadLatencyMS: 3625}, 8, ""},
		{"restore-short", ClockTiming{PollSeconds: 4, HostReadLatencyMS: 3626}, 8, "clock-timing-restore-reserve"},
		{"slow-serial", slowTiming, 8, "clock-timing-restore-reserve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.timing.budget(tc.reads)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.cause {
				t.Fatalf("MUTATION: timing-fit: got %q want %q", got, tc.cause)
			}
		})
	}
}

// An unfit configuration stops every gate before the first host request.
func TestUnfitTimingRefusedBeforeAnyRequest(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			r, p, _ := runtimeFixture(t)
			r.Session.capabilities[operator.HostReadBatch] = batch
			r.Session.capabilities[operator.HostLeaseReadBatch] = batch
			timing, cause := slowTiming, "clock-timing-restore-reserve"
			if batch {
				timing, cause = ClockTiming{PollSeconds: 3, HostReadLatencyMS: 4000}, "clock-timing-lease-validity"
			}
			got := (ResourceRunner{Config: ResourceConfig{ClockTiming: timing}}).Run(context.Background(), r.Session, p)
			final := r.Session.Final(got)
			if len(final.Gates) != 1 || final.Gates[0].Status != operator.NotRun || final.Gates[0].Reason != cause || p.sent != 0 || r.Session.Latched() {
				t.Fatal("MUTATION: timing-refused-before-session", final.Gates, p.sent, r.Session.Latched())
			}
		})
	}
	require.EqualError(t, ResourceConfig{ClockTiming: ClockTiming{PollSeconds: 3, HostReadLatencyMS: 4000}}.CheckClockTiming(), "clock-timing-lease-validity")
	require.NoError(t, ResourceConfig{}.CheckClockTiming())
	// Before the session the adapter's capabilities are unknown: batch reads are assumed.
	require.NoError(t, ResourceConfig{ClockTiming: slowTiming}.CheckClockTiming())
}

const slowHostEpoch = 1793930000 // A multiple of the eight-second poll.

// pollRows returns the tracking rows a host logs in (after, now], one per poll.
func pollRows(after, now time.Time, poll time.Duration) []clocks.TrackingRow {
	rows := []clocks.TrackingRow{}
	for at := after.Truncate(poll).Add(poll); !at.After(now); at = at.Add(poll) {
		rows = append(rows, clocks.TrackingRow{UnixSeconds: at.Unix(), MaxErrorSeconds: 1e-6, LeapStatus: "N"})
	}
	return rows
}

func packedTracking(t *testing.T, rows []clocks.TrackingRow) string {
	t.Helper()
	reply := hostReply(t, oracleJSON(clocks.Sample{MeasuredUnixNS: "1793930000000000000", TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: rows}}))
	var out operator.HostOutput
	require.NoError(t, json.Unmarshal(reply.Payload, &out))
	return out.Stdout
}

// slowWorld makes every host action take four seconds and every host clock
// update once per eight seconds, on the round-trip world's real procedures.
func slowWorld(t *testing.T) (*roundTripWorld, ProcedurePorts, *[]time.Time) {
	t.Helper()
	w, p := newRoundTripWorld(t, true)
	s := w.runtime.Session
	var err error
	s.timing, err = slowTiming.budget(s.restoreReads())
	require.NoError(t, err)
	poll := time.Duration(slowTiming.PollSeconds) * time.Second
	latency := time.Duration(slowTiming.HostReadLatencyMS) * time.Millisecond
	peer := w.runtime.Peer.(*observationPeer)
	base := peer.respond
	moves := &[]time.Time{}
	logged := map[string]time.Time{}
	peer.respond = func(q operator.Request) operator.Reply {
		w.clock.Advance(latency)
		now := time.Unix(slowHostEpoch, 0).Add(w.clock.Elapsed())
		if q.Operation != operator.HostLeaseReadBatch {
			if q.Action == "move" {
				*moves = append(*moves, now)
			}
			return base(q)
		}
		w.counts[q.Action]++
		var args operator.HostLeaseReadArgs
		require.NoError(t, json.Unmarshal(q.Args, &args))
		reference := now.Truncate(poll)
		o := bindingtest.Observation(now)
		o.PollSeconds = fmt.Sprint(slowTiming.PollSeconds)
		o.ReferenceUnixNS, o.ReferenceAgeNS = fmt.Sprint(reference.UnixNano()), int64(now.Sub(reference))
		out := operator.HostReadBatchOutput{}
		for _, read := range args.Reads {
			if logged[read.Target].IsZero() {
				logged[read.Target] = now.Add(-2 * poll)
			}
			rows := pollRows(logged[read.Target], now, poll)
			logged[read.Target] = now
			child := hostReply(t, oracleJSON(clocks.Sample{MeasuredUnixNS: o.MeasuredUnixNS, Lease: &o, TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: rows}}))
			child.ID = read.Target
			out.Replies = append(out.Replies, child)
		}
		return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Payload: oracleJSON(out)}
	}
	return w, p, moves
}

// With an eight-second poll and four seconds per host action, a rollout with
// its moves and kills completes without any clock-lease or clock-coverage cause.
func TestSlowHostReadsGateSequence(t *testing.T) {
	w, p, moves := slowWorld(t)
	s := w.runtime.Session
	out := RunProcessGate(context.Background(), operator.Rollout, w.runtime.Config, p, GateMaximum)
	if !out.Completed || out.Status != operator.Passed || out.Reason != "" || s.Latched() || len(s.uncertainties) != 0 {
		t.Fatalf("MUTATION: slow-host-read-gate: %+v latched=%t uncertainties=%+v counts=%v", out, s.Latched(), s.uncertainties, w.counts)
	}
	if w.counts["move"] == 0 || w.counts["worker-down"] == 0 {
		t.Fatal("the sequence needs a move and a kill", w.counts)
	}
	for _, d := range s.Final([]operator.GateResult{out.GateResult(operator.Rollout)}).Gates {
		if strings.HasPrefix(d.Reason, "clock-") {
			t.Fatal("clock cause published", d.Reason)
		}
	}
	// Every move and every kill of the sequence gets a coverage window.
	windows := []observe.CoverageWindow{}
	window := func(kind, worker string, start int64) {
		windows = append(windows, observe.CoverageWindow{Event: len(windows), Kind: kind, Worker: worker, StartUnixNS: fmt.Sprintf("%019d", start), EndUnixNS: fmt.Sprintf("%019d", start+int64(time.Second))})
	}
	for _, at := range *moves {
		window("move", "", at.UnixNano())
	}
	for _, b := range s.clockBrackets {
		start, ok := observe.CoverageNS(b.MidpointUnixNS)
		require.True(t, ok)
		window("takeover", b.Worker, start)
	}
	require.Len(t, windows, w.counts["move"]+w.counts["worker-down"])
	// The gate's own lease reads retain the rows that close every window; the
	// trailing drain has its own test (TestProductionDrainCoversLastTakeover).
	e := s.collectCoverage(context.Background(), clocks.Bounds{}, windows, nil, p.Sleep, s.Budget.end)
	require.True(t, e.DrainComplete)
	require.Len(t, e.Results, len(windows))
	for _, r := range e.Results {
		if len(r.Causes) != 0 {
			t.Fatalf("MUTATION: slow-host-read-coverage: event %d has %v", r.Event, r.Causes)
		}
	}
	t.Logf("elapsed=%s counts=%v windows=%d", w.clock.Elapsed(), w.counts, len(windows))
}

// The same sequence at the reference timing latches on its first slow lease
// read. This is the failure the budget removes; it also keeps the latch.
func TestSlowHostReadsLatchUnderReferenceTiming(t *testing.T) {
	w, p, _ := slowWorld(t)
	w.runtime.Session.timing = referenceTiming
	out := RunProcessGate(context.Background(), operator.Rollout, w.runtime.Config, p, GateMaximum)
	if out.Completed || !w.runtime.Session.Latched() {
		t.Fatalf("MUTATION: slow-read-latch-kept: %+v", out)
	}
}

// A configured latency raises the send minimums: a lease read that cannot end
// inside its budget is refused locally instead of becoming a late, unknown reply.
func TestConfiguredLeaseSendMinimum(t *testing.T) {
	for _, tc := range []struct {
		op      operator.Operation
		target  string
		args    string
		minimum time.Duration
	}{
		{operator.HostLeaseRead, "w1", `{"reads":[{"target":"w1"}]}`, 4150 * time.Millisecond},
		{operator.HostLeaseReadBatch, "hosts", `{"reads":[{"target":"relay"},{"target":"w1"},{"target":"w2"},{"target":"shared"}]}`, 16 * time.Second},
	} {
		for _, short := range []bool{true, false} {
			s, c := fixtureSession(t)
			var err error
			s.timing, err = slowTiming.budget(2)
			require.NoError(t, err)
			s.capabilities[tc.op] = true
			p := &observationPeer{respond: func(q operator.Request) operator.Reply { return reply(q, operator.Succeeded) }}
			remaining := tc.minimum + 50*time.Millisecond
			if short {
				remaining = tc.minimum - 10*time.Millisecond
			}
			_, err = s.Exchange(context.Background(), p, tc.op, tc.target, "clock", json.RawMessage(tc.args), nil, c.Elapsed()+remaining, func(operator.Reply) operator.Certainty { return operator.Succeeded })
			if short != (err != nil && err.Error() == "clock-lease-expired" && p.sent == 0) || !short && (err != nil || p.sent != 1) || s.Latched() {
				t.Fatal("MUTATION: configured-lease-send-minimum", tc.op, short, err, p.sent, s.Latched())
			}
		}
	}
}

// The drain takes its configured limit from the pool reserve; the judge keeps the rest.
func TestConfiguredDrainKeepsJudgeReserve(t *testing.T) {
	for budget, drained := range map[time.Duration]time.Duration{PoolReserve: 32 * time.Second, 100 * time.Second: 12 * time.Second, 88 * time.Second: 0} {
		s, c, pool := finalDrainFixture(t)
		var err error
		s.timing, err = slowTiming.budget(2)
		require.NoError(t, err)
		s.gates = nil
		s.Budget.end = budget
		judgedAt := time.Duration(-1)
		pool.Judge = mediaJudgeFunc(func(context.Context, operatorevidence.Pool) (operatorevidence.Summary, error) {
			judgedAt = c.Elapsed()
			return operatorevidence.Summary{Status: operator.Passed}, nil
		})
		out := s.RunSequence(context.Background(), SequencePorts{Finalize: func(ctx context.Context, end time.Duration) (*MediaSummary, error) {
			// This tail never closes: the drain runs to its limit.
			summary, _, err := s.finalizePool(ctx, end, pool, clocks.FixedCoverageBounds(), func(context.Context, string, string) (string, error) {
				return packedCoverageSample(t, 1793930021, true), nil
			}, func(_ context.Context, d time.Duration) error { c.Advance(d); return nil })
			return summary, err
		}})
		if judgedAt != drained || budget-judgedAt < min(budget, 88*time.Second) || out.Pool == nil {
			t.Fatal("MUTATION: configured-judge-reserve", budget, judgedAt, out)
		}
	}
}
