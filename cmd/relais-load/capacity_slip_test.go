package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

var capacitySlipStart = time.Unix(1000, 0)

// Thirty one-second media samples plus the hangup sample, all within limits.
func capacitySlipStats() []loadgen.Stats {
	var out []loadgen.Stats
	for i := 1; i <= 31; i++ {
		out = append(out, loadgen.Stats{At: capacitySlipStart.Add(time.Duration(i) * time.Second), Interval: time.Second, CPUPerGOMAXPROCS: 5,
			Lateness:         loadgen.Lateness{Packets: uint64(160 * i), P99: time.Millisecond, Max: 2 * time.Millisecond},
			IntervalLateness: loadgen.Lateness{Packets: 160, P99: time.Millisecond, Max: 2 * time.Millisecond},
			IntervalPacing:   loadgen.Pacing{SentFrames: 160}})
	}
	return out
}

func capacitySlipSamples() [2][]loadgen.Stats {
	return [2][]loadgen.Stats{capacitySlipStats(), capacitySlipStats()}
}

func capacitySlip(s *loadgen.Stats, p99, peak time.Duration) {
	s.IntervalLateness.P99, s.IntervalLateness.Max = p99, peak
}

// capacitySlipRun builds a two-generator step through loadgen.ReadProcess and
// RunReport.Judge, as execute does, so the strict generator causes match
// production. Generator 0's single call is affected by the event at 6 s.
func capacitySlipRun(t *testing.T, event string, samples [2][]loadgen.Stats, tracks ...callharness.MoveTrackReport) loadgen.RunReport {
	t.Helper()
	at := capacitySlipStart
	r := capacityFixture()
	r.StartedAt, r.Duration, r.Limits, r.Callers, r.Yardstick = at, 30*time.Second, loadgen.DefaultLimits(), 2, nil
	move := callharness.MoveReport{Kind: "move", To: 1, Start: 6 * time.Second, End: 6100 * time.Millisecond, Measurement: callharness.EventMeasurement{MediaResumedAt: 6050 * time.Millisecond}}
	move.Measurement.AudioLoss.Trusted, move.Measurement.Audio.Verdict.Trusted, move.Measurement.ResumeVerdict.Trusted = true, true, true
	move.Tracks = tracks
	if len(tracks) == 0 {
		move.Tracks = []callharness.MoveTrackReport{{Kind: "audio", PacketsAfter: 100, Gap: 40 * time.Millisecond, BaselineGapSamples: 50, BaselineGapP95: 30 * time.Millisecond}}
	}
	e := loadgen.EventResult{Event: loadgen.Event{After: 6 * time.Second, Kind: event, Count: 1, Worker: 1}, At: at.Add(6 * time.Second), FinishedAt: at.Add(6200 * time.Millisecond), Affected: 1, Sessions: []string{"s42"}}
	if event == "kill" {
		move.Kind, move.From, e.Event.Worker = "takeover", 0, 0
		r.Telemetry.Processes[1].Last["relais_control_takeovers_total"] = 1
	}
	r.Events = []loadgen.EventResult{e}
	r.Processes = nil
	for i, pid := range []int{42, 43} {
		call := &callharness.Report{StartedAt: at}
		if i == 0 {
			call.Moves = []callharness.MoveReport{move}
		}
		var lines bytes.Buffer
		w := loadgen.NewWriter(&lines)
		w.Write(loadgen.Message{Type: "call", PID: pid, Session: fmt.Sprintf("s%d", pid), Call: call})
		for _, s := range samples[i] {
			w.Write(loadgen.Message{Type: "stats", PID: pid, Stats: &s})
		}
		w.Write(loadgen.Message{Type: "done", PID: pid})
		require.NoError(t, w.Err())
		p, err := loadgen.ReadProcess(&lines, pid, 1, r.Limits)
		require.NoError(t, err)
		r.Processes = append(r.Processes, p)
	}
	r.Judge()
	return r
}

// #133: the #85 rerun's 50-call steps were inconclusive only because single
// 1 s samples had send-lateness p99 of 25.3 and 25.9 ms in two generators.
func TestCapacityIsolatedSlipIsNotInconclusive(t *testing.T) {
	samples := capacitySlipSamples()
	r := capacitySlipRun(t, "move", samples)
	require.Equal(t, "valid", r.Outcome)
	require.Equal(t, "pass", judgeCapacity("relay", r, 100, 128<<10).Verdict)

	capacitySlip(&samples[0][15], 25300*time.Microsecond, 25300*time.Microsecond)
	capacitySlip(&samples[1][22], 25900*time.Microsecond, 26*time.Millisecond)
	r = capacitySlipRun(t, "move", samples)
	require.Equal(t, "inconclusive", r.Outcome, "the strict loadgen judge is unchanged")
	step := judgeCapacity("relay", r, 100, 128<<10)
	require.Equal(t, "pass", step.Verdict, step.Causes)
	require.Equal(t, "valid", step.GeneratorOutcome)
	require.Len(t, step.ToleratedSlips, 2)
	require.Contains(t, step.ToleratedSlips[0], "process 42: isolated interval send-lateness p99 25.3ms exceeds 20ms")
	// Raw evidence keeps the strict single-sample gate.
	require.Equal(t, "inconclusive", step.Run.Outcome)
	require.True(t, step.Run.Processes[0].Saturated)
	require.True(t, step.Run.Processes[1].Saturated)
	require.Contains(t, strings.Join(step.Run.Causes, ";"), "interval send-lateness p99 peak 25.3ms exceeds 20ms")
	require.Equal(t, r, step.Run)
}

func TestCapacityOverloadStaysInconclusive(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name   string
		mutate func(*[2][]loadgen.Stats)
		after  func(*loadgen.RunReport)
		want   string
		cause  string
	}{
		{"exactly twice the limit is a slip", func(s *[2][]loadgen.Stats) { capacitySlip(&s[0][15], 40*ms, 40*ms) }, nil, "pass", ""},
		{"consecutive slips", func(s *[2][]loadgen.Stats) {
			capacitySlip(&s[0][15], 21*ms, 21*ms)
			capacitySlip(&s[0][16], 21*ms, 21*ms)
		}, nil, "inconclusive", "interval send-lateness p99 peak 21ms"},
		{"above twice the limit", func(s *[2][]loadgen.Stats) { capacitySlip(&s[0][15], 40100*time.Microsecond, 41*ms) }, nil, "inconclusive", "interval send-lateness p99 peak 40.1ms"},
		{"isolated skipped frames are a slip", func(s *[2][]loadgen.Stats) {
			s[1][15].IntervalPacing = loadgen.Pacing{SentFrames: 158, SkippedFrames: 2}
		}, nil, "pass", ""},
		{"skipped frames with a slip in one sample", func(s *[2][]loadgen.Stats) {
			capacitySlip(&s[0][15], 25*ms, 25*ms)
			s[0][15].IntervalPacing = loadgen.Pacing{SentFrames: 158, SkippedFrames: 2}
		}, nil, "pass", ""},
		{"exactly twice the skipped limit is a slip", func(s *[2][]loadgen.Stats) {
			s[1][15].IntervalPacing = loadgen.Pacing{SentFrames: 98, SkippedFrames: 2}
		}, nil, "pass", ""},
		{"skipped frames above twice the limit", func(s *[2][]loadgen.Stats) {
			s[1][15].IntervalPacing = loadgen.Pacing{SentFrames: 155, SkippedFrames: 5}
		}, nil, "inconclusive", "skipped frames 3.12% exceeds 1.00%"},
		{"consecutive skipped frames", func(s *[2][]loadgen.Stats) {
			s[1][15].IntervalPacing = loadgen.Pacing{SentFrames: 158, SkippedFrames: 2}
			s[1][16].IntervalPacing = loadgen.Pacing{SentFrames: 158, SkippedFrames: 2}
		}, nil, "inconclusive", "skipped frames 1.25% exceeds 1.00%"},
		{"skipped frames next to a lateness slip", func(s *[2][]loadgen.Stats) {
			capacitySlip(&s[0][15], 25*ms, 25*ms)
			s[0][16].IntervalPacing = loadgen.Pacing{SentFrames: 158, SkippedFrames: 2}
		}, nil, "inconclusive", "skipped frames 1.25% exceeds 1.00%"},
		{"maximum within the tolerated stall", func(s *[2][]loadgen.Stats) { capacitySlip(&s[0][15], 25*ms, 40*ms) }, nil, "pass", ""},
		{"maximum beyond the tolerated stall", func(s *[2][]loadgen.Stats) { capacitySlip(&s[0][15], 25*ms, 40*ms+time.Microsecond) }, nil, "inconclusive", "interval send-lateness p99 peak 25ms"},
		{"CPU excess", func(s *[2][]loadgen.Stats) {
			capacitySlip(&s[0][15], 25*ms, 25*ms)
			s[0][20].CPUPerGOMAXPROCS = 86
		}, nil, "inconclusive", "CPU 86.00% per GOMAXPROCS exceeds 85.00%"},
		{"dropped samples cannot prove isolation", func(s *[2][]loadgen.Stats) { capacitySlip(&s[0][15], 25*ms, 25*ms) }, func(r *loadgen.RunReport) {
			r.Processes[0].SamplesDropped = 1
		}, "inconclusive", "interval send-lateness p99 peak 25ms"},
		{"execution error is retained", func(s *[2][]loadgen.Stats) { capacitySlip(&s[0][15], 25*ms, 25*ms) }, func(r *loadgen.RunReport) {
			r.Causes = append(r.Causes, "context deadline exceeded")
		}, "inconclusive", "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := capacitySlipSamples()
			tc.mutate(&samples)
			r := capacitySlipRun(t, "move", samples)
			if tc.after != nil {
				tc.after(&r)
			}
			step := judgeCapacity("relay", r, 100, 128<<10)
			require.Equal(t, tc.want, step.Verdict, step.Causes)
			if tc.want == "inconclusive" {
				require.Equal(t, "inconclusive", step.GeneratorOutcome)
				require.Empty(t, step.ToleratedSlips)
				require.Contains(t, strings.Join(step.Causes, ";"), "generator invalid: ")
				require.Contains(t, strings.Join(step.Causes, ";"), tc.cause)
			}
		})
	}
}

// An isolated slip that no longer invalidates the step can stamp the last
// packet before an outage late and shorten the measured gap. The bound is the
// generator's interval maximum lateness plus one 20 ms audio tick.
func TestCapacityStallBoundGuardsNearThresholdGaps(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name          string
		event         string
		generator     int
		sample        int
		p99, peak     time.Duration
		gap, baseline time.Duration
		want          string
	}{
		// A 30 ms maximum gives a 50 ms bound. Sample 5 covers [5 s, 6 s].
		{"move excess within bound", "move", 0, 5, 25 * ms, 30 * ms, 40 * ms, 30 * ms, "inconclusive"},
		{"move ceiling within bound", "move", 0, 5, 25 * ms, 30 * ms, 120 * ms, 125 * ms, "inconclusive"},
		{"move excess margin equals bound", "move", 0, 5, 25 * ms, 30 * ms, 40 * ms, 40 * ms, "pass"},
		{"bound adds one audio tick", "move", 0, 5, 25 * ms, 30 * ms, 40 * ms, 39 * ms, "inconclusive"},
		{"move failure stays failure", "move", 0, 5, 25 * ms, 30 * ms, 151 * ms, 140 * ms, "fail"},
		{"stall until media resumes", "move", 0, 6, 25 * ms, 30 * ms, 40 * ms, 30 * ms, "inconclusive"},
		{"maximum above limit with p99 within it", "move", 0, 5, 15 * ms, 30 * ms, 40 * ms, 30 * ms, "inconclusive"},
		{"maximum within limit is the existing envelope", "move", 0, 5, 20 * ms, 20 * ms, 40 * ms, 30 * ms, "pass"},
		{"ends one second before the move", "move", 0, 4, 25 * ms, 39 * ms, 40 * ms, 30 * ms, "pass"},
		{"starts after media resumes", "move", 0, 7, 25 * ms, 39 * ms, 40 * ms, 30 * ms, "pass"},
		{"another generator cannot shorten this gap", "move", 1, 5, 25 * ms, 39 * ms, 40 * ms, 30 * ms, "pass"},
		{"takeover within bound", "kill", 0, 5, 25 * ms, 30 * ms, 1960 * ms, 0, "inconclusive"},
		{"takeover margin equals bound", "kill", 0, 5, 25 * ms, 30 * ms, 1950 * ms, 0, "inconclusive"},
		{"takeover margin exceeds bound", "kill", 0, 5, 25 * ms, 30 * ms, 1949 * ms, 0, "pass"},
		{"takeover failure stays failure", "kill", 0, 5, 25 * ms, 30 * ms, 2 * time.Second, 0, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := capacitySlipSamples()
			capacitySlip(&samples[tc.generator][tc.sample], tc.p99, tc.peak)
			r := capacitySlipRun(t, tc.event, samples, callharness.MoveTrackReport{Kind: "audio", PacketsAfter: 100, Gap: tc.gap, BaselineGapSamples: 50, BaselineGapP95: tc.baseline})
			step := judgeCapacity("n-plus-one", r, 100, 128<<10)
			require.Equal(t, tc.want, step.Verdict, step.Causes)
			if tc.want == "inconclusive" {
				require.Equal(t, "valid", step.GeneratorOutcome)
				require.Contains(t, strings.Join(step.Causes, ";"), "caller stall bound")
			}
		})
	}
}

// A stall touching every track must not turn a passing p95 into a failure.
func TestCapacityStallBoundKeepsP95Denominator(t *testing.T) {
	var tracks []callharness.MoveTrackReport
	for i := range 20 {
		gap := 40 * time.Millisecond
		switch i {
		case 18:
			gap = 140 * time.Millisecond
		case 19:
			gap = 151 * time.Millisecond
		}
		tracks = append(tracks, callharness.MoveTrackReport{Kind: "audio", PacketsAfter: 100, Gap: gap, BaselineGapSamples: 50, BaselineGapP95: gap - 10*time.Millisecond})
	}
	samples := capacitySlipSamples()
	require.Equal(t, "pass", judgeCapacity("relay", capacitySlipRun(t, "move", samples, tracks...), 100, 128<<10).Verdict)
	capacitySlip(&samples[0][5], 35*time.Millisecond, 35*time.Millisecond)
	step := judgeCapacity("relay", capacitySlipRun(t, "move", samples, tracks...), 100, 128<<10)
	require.Equal(t, "inconclusive", step.Verdict, step.Causes)
}

// The soak judges caller stalls per event (soakGapRows); its shared capacity
// checks keep their measured-gap verdicts.
func TestCapacitySoakPathKeepsMeasuredGapVerdict(t *testing.T) {
	samples := capacitySlipSamples()
	capacitySlip(&samples[0][5], 15*time.Millisecond, 30*time.Millisecond)
	r := capacitySlipRun(t, "move", samples)
	require.Equal(t, "valid", r.Outcome)
	require.Equal(t, "inconclusive", judgeCapacity("relay", r, 100, 128<<10).Verdict)
	require.Equal(t, "pass", judgeCapacityExcept("soak", r, 100, 128<<10, map[int]bool{}).Verdict)
}

// A worker above the proven lower bound is a known failure when the only
// generator cause was a tolerated slip.
func TestCapacityPeakOverrideUsesToleratedGenerator(t *testing.T) {
	samples := capacitySlipSamples()
	capacitySlip(&samples[1][20], 25*time.Millisecond, 25*time.Millisecond)
	for _, tc := range []struct{ kind, want string }{{"n-plus-one", "fail"}, {"drain", "fail"}, {"relay", "inconclusive"}} {
		t.Run(tc.kind, func(t *testing.T) {
			r := capacitySlipRun(t, "kill", samples)
			r.WorkerPeaks = map[string]int{"worker-1": 9}
			dir := t.TempDir()
			step, err := recordCapacityStep(dir, filepath.Join(dir, "top.json"), &capacityReport{WorkerCapacityLowerBound: 8}, judgeCapacity(tc.kind, r, 100, 128<<10))
			require.NoError(t, err)
			require.Equal(t, tc.want, step.Verdict, step.Causes)
		})
	}
}

func TestCapacityRequiresOneSecondSamples(t *testing.T) {
	c := config{Capacity: true, Callers: 2, Processes: 1, Workers: 3, GOMAXPROCS: 1, Duration: 20 * time.Second, SampleEvery: time.Second, Limits: loadgen.DefaultLimits(), CapacityStart: 2, CapacityMax: 2, CallersPerProcess: 2, StoreOpsBudget: 100, StoreBytesBudget: 128 << 10}
	require.NoError(t, c.validate())
	for _, every := range []time.Duration{100 * time.Millisecond, 2 * time.Second} {
		c.SampleEvery = every
		require.Error(t, c.validate())
	}
}

// Review regression: a tolerated stall can stamp the packet before a real
// interruption late, so an unscheduled 140 ms gap may truly exceed 150 ms.
func TestCapacityUnscheduledGapCannotHideBehindStall(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name      string
		generator int
		sample    int
		gap       time.Duration
		want      string
		cause     string
	}{
		// Sample 14 covers [14 s, 15 s]; its 30 ms maximum gives a 50 ms bound.
		{"near-ceiling gap within the bound", 0, 14, 140 * ms, "inconclusive", "within this caller's stall bound 50ms of 150ms"},
		{"margin equals the bound", 0, 14, 100 * ms, "pass", ""},
		{"measured failure stays failure", 0, 14, 200 * ms, "fail", "whole-call media gap outside scheduled events exceeds 150ms"},
		{"stall a second before the gap", 0, 13, 140 * ms, "inconclusive", "stall bound"},
		{"stall ended before that second", 0, 12, 140 * ms, "pass", ""},
		{"stall after the gap", 0, 15, 140 * ms, "pass", ""},
		{"another generator cannot shorten this gap", 1, 14, 140 * ms, "pass", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := capacitySlipSamples()
			capacitySlip(&samples[tc.generator][tc.sample], 25*ms, 30*ms)
			for i := tc.sample; i < len(samples[tc.generator]); i++ {
				samples[tc.generator][i].Lateness.Max = 30 * ms
			}
			r := capacitySlipRun(t, "move", samples)
			end := 15 * time.Second
			r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGap: tc.gap, MediaGapEndedAt: end, MediaGapsRecorded: true, Arrivals: []time.Duration{end - tc.gap, end}}}
			if tc.gap > callharness.MediaGapRetention {
				r.Processes[0].Calls[0].Call.Tracks[0].MediaGaps = []callharness.MediaGap{{EndedAt: end, Duration: tc.gap}}
			}
			step := judgeCapacity("relay", r, 100, 128<<10)
			require.Equal(t, tc.want, step.Verdict, step.Causes)
			require.Contains(t, strings.Join(step.Causes, ";"), tc.cause)
		})
	}
}

// The retained inventory covers gaps that are neither the longest nor in the
// rolling arrival history, and a full inventory cannot prove the rest.
func TestCapacityJudgesEveryRetainedGap(t *testing.T) {
	ms := time.Millisecond
	samples := capacitySlipSamples()
	r := capacitySlipRun(t, "move", samples)
	// The longest gap belongs to the move at 6 s.
	track := callharness.TrackReport{Kind: "audio", MediaGap: 300 * ms, MediaGapEndedAt: 6200 * ms, MediaGapsRecorded: true, Arrivals: []time.Duration{29 * time.Second, 29*time.Second + 20*ms}}
	track.MediaGaps = []callharness.MediaGap{{EndedAt: 6200 * ms, Duration: 300 * ms}}
	r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{track}
	require.Equal(t, "pass", judgeCapacity("relay", r, 100, 128<<10).Verdict)
	track.MediaGaps = append(track.MediaGaps, callharness.MediaGap{EndedAt: 20 * time.Second, Duration: 200 * ms})
	r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{track}
	step := judgeCapacity("relay", r, 100, 128<<10)
	require.Equal(t, "fail", step.Verdict, step.Causes)
	require.Contains(t, strings.Join(step.Causes, ";"), "retained media gap outside scheduled events exceeds 150ms")

	track.MediaGaps = make([]callharness.MediaGap, callharness.MaxRetainedMediaGaps)
	for i := range track.MediaGaps {
		track.MediaGaps[i] = callharness.MediaGap{EndedAt: 6200 * ms, Duration: 300 * ms}
	}
	r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{track}
	step = judgeCapacity("relay", r, 100, 128<<10)
	require.Equal(t, "inconclusive", step.Verdict, step.Causes)
	require.Contains(t, strings.Join(step.Causes, ";"), "gap inventory is full")
}
