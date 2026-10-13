package callharness

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Offline reproduction: retained 185s at 50Hz audio/30fps video (three
// packets/frame), production indexes, three callers sharing one scheduler P.
func costCalls(t *testing.T, events int) []*Call {
	t.Helper()
	return costCallsWithClock(t, events, 50*time.Millisecond, time.Millisecond)
}

func costCallsWithClock(t *testing.T, events int, rtt, bound time.Duration) []*Call {
	t.Helper()
	calls := make([]*Call, 3)
	for i := range calls {
		r, c := denseDistributedCall(t, true, rtt, bound)
		addDenseRepeatedReturns(r)
		for key, u := range r.sentAudioUnits {
			r.audioSends = append(r.audioSends, evidenceRef{at: u.at, audio: key})
			r.audioReturns = append(r.audioReturns, evidenceRef{at: u.returnedAt, audio: key})
		}
		for key, f := range r.sentVideoFrames {
			r.videoSends = append(r.videoSends, evidenceRef{at: f.at, video: key, generation: f.identity})
			r.videoReturns = append(r.videoReturns, evidenceRef{at: f.returnedAt, video: key, generation: f.identity})
		}
		for _, refs := range [][]evidenceRef{r.audioSends, r.audioReturns, r.videoSends, r.videoReturns} {
			slices.SortFunc(refs, func(a, b evidenceRef) int { return cmp.Compare(a.at, b.at) })
		}
		for j := 0; j < events; j++ {
			start := 5*time.Second + time.Duration(j)*5*time.Second
			kind := "move"
			end := start + 30*time.Millisecond
			if j%3 == 1 {
				kind = "takeover"
				end = start + 500*time.Millisecond
			}
			recordDenseEvent(t, r, c, fmt.Sprint("event-", j), kind, start, end)
		}
		calls[i] = c
	}
	return calls
}

func TestCallerCost(t *testing.T) {
	if os.Getenv("RELAIS_MEASURE_CALLER_COST") != "1" {
		t.Skip("opt-in offline timing; run without race instrumentation")
	}
	procs := 1
	if value := os.Getenv("RELAIS_CALLER_COST_PROCS"); value != "" {
		var err error
		procs, err = strconv.Atoi(value)
		if err != nil || procs < 1 || procs > 2 {
			t.Fatal("RELAIS_CALLER_COST_PROCS must be 1 or 2")
		}
	}
	prev := runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(prev)
	calls := costCalls(t, 30)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	began := time.Now()
	var build, encode time.Duration
	for _, c := range calls {
		stage := time.Now()
		rep := c.rec.report()
		build += time.Since(stage)
		stage = time.Now()
		if err := json.NewEncoder(io.Discard).Encode(rep); err != nil {
			t.Fatal(err)
		}
		encode += time.Since(stage)
	}
	report := time.Since(began)
	runtime.ReadMemStats(&after)
	t.Logf("report 3 calls/30 events: %s build=%s encode=%s allocated=%d bytes", report, build, encode, after.TotalAlloc-before.TotalAlloc)
	// A maintenance tick may run on the same P just as the operator arms or
	// delivers a receipt. Exercise it as well as the actual Call entry points.
	events, advance := 2, 5
	if os.Getenv("RELAIS_MEASURE_CALLER_COST_HEAVY") == "1" {
		// Delayed maintenance expires a full retained event prefix. Exercise
		// capped classifications with the media pacers still running.
		events, advance = 30, 150
	}
	calls = costCalls(t, events)
	done := make(chan struct{})
	samples := make(chan []time.Duration, 6)
	skips := make(chan uint64, 6)
	for _, c := range calls {
		for _, period := range []time.Duration{20 * time.Millisecond, time.Second / 30} {
			go func(c *Call, period time.Duration) {
				tick := time.NewTicker(period)
				defer tick.Stop()
				start := time.Now()
				p := framePacer{period: period, start: start, scheduled: start}
				var late []time.Duration
				for {
					select {
					case at := <-tick.C:
						p.next(at)
						c.rec.mu.Lock()
						c.rec.mu.Unlock()
						late = append(late, time.Since(p.scheduled))
						p.sentFrame()
					case <-done:
						samples <- late
						skips <- p.skipped
						return
					}
				}
			}(c, period)
		}
	}
	time.Sleep(100 * time.Millisecond)
	began = time.Now()
	for round := 0; round < 5; round++ {
		for _, c := range calls {
			id := OperationIdentity{RunID: "run", OperationID: fmt.Sprint("arm-", round), SessionID: "session", Worker: "0", Epoch: 1}
			if err := c.ArmDistributedEvent(id); err != nil {
				t.Fatal(err)
			}
			if err := c.RecordDistributedEvent(DistributedEvent{Identity: id, Kind: "move", Start: time.Now(), End: time.Now()}); err != nil {
				t.Fatal(err)
			}
			c.rec.compactRecording(densityTotal + time.Duration(advance+round)*time.Second)
		}
	}
	work := time.Since(began)
	var held time.Duration
	for _, c := range calls {
		held = max(held, c.rec.measurementLockTime)
	}
	time.Sleep(100 * time.Millisecond)
	close(done)
	var all []time.Duration
	var skipped uint64
	for range 6 {
		all = append(all, (<-samples)...)
		skipped += <-skips
	}
	slices.Sort(all)
	t.Logf("arm/receipt + maintenance (%d retained events): %s pacer p99=%s max=%s skipped=%d samples=%d last_prune_lock=%s", events, work, all[(99*len(all)+99)/100-1], all[len(all)-1], skipped, len(all), held)
	if !densityRaceDetector && os.Getenv("RELAIS_TEST_STRICT_TIMING") == "1" {
		if all[(99*len(all)+99)/100-1] >= 20*time.Millisecond {
			t.Fatal("pacer interval p99 reaches saturation threshold")
		}
		if skipped > 0 {
			t.Fatal("operator/maintenance work skipped media frames")
		}
		if report >= 10*time.Second {
			t.Fatal("final report exceeds offline 10s budget")
		}
	}
}

func TestCallerCostPreparedPointEvidenceEquivalent(t *testing.T) {
	r := costCalls(t, 2)[0].rec
	r.prepareMeasurementIndex()
	point := MoveReport{Kind: "move", Start: 5 * time.Second, End: 5030 * time.Millisecond}
	expected := r.legacyMeasureEvent(point, 10*time.Second, 0, false)
	actual := r.eventMeasurement(point, 10*time.Second, 0)
	t.Logf("post-event samples audio legacy=%d new=%d; video legacy=%d new=%d", expected.Audio.PostEventSamples, actual.Audio.PostEventSamples, expected.Video.PostEventSamples, actual.Video.PostEventSamples)
	require.Equal(t, expected, actual)
}

// Use the same retained recordings and production indexes as the #118 cost
// harness, with a measured clock width rather than the old WAN-sized bound.
func TestCallerEvaluationCounts(t *testing.T) {
	if os.Getenv("RELAIS_MEASURE_CALLER_COST") != "1" {
		t.Skip("opt-in retained-history evaluation counts")
	}
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	for _, tc := range []struct{ rtt, bound time.Duration }{{3 * time.Millisecond, 2 * time.Millisecond}, {4 * time.Millisecond, 2 * time.Millisecond}, {3 * time.Millisecond, 3 * time.Millisecond}, {4 * time.Millisecond, 3 * time.Millisecond}} {
		calls := costCallsWithClock(t, 2, tc.rtt, tc.bound)
		for _, c := range calls {
			r := c.rec
			reports := r.measureMoves(r.hungUpAt, len(r.moves))
			next, _, err := r.mappedMoveTimes(r.moves[1])
			if err != nil {
				t.Fatal(err)
			}
			moveCells := budgetCells(r, reports[0], next.Lower, DurationInterval{})
			killCells := budgetCells(r, reports[1], r.hungUpAt, reports[0].Distributed.BaselineFloor)
			if reports[0].Distributed.Evaluations != moveCells || reports[1].Distributed.Evaluations != killCells {
				t.Fatal("reported evaluations differ from cell cardinality")
			}
			if moveCells >= maxClassificationEvaluations || killCells >= maxClassificationEvaluations {
				t.Fatal("narrow clock interval exhausted the cell budget")
			}
			t.Logf("RTT=%s host_bound=±%s mapped_width=%s move=%d cells (%d first baselines), takeover=%d cells", tc.rtt, tc.bound, reports[0].Distributed.Start.Latest.Sub(reports[0].Distributed.Start.Earliest), moveCells, len(reports[0].Distributed.FirstBaselines), killCells)
		}
	}
}
