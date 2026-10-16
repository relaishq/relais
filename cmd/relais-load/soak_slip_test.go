package main

import (
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

// #133: skipped frames get the lateness rule. One isolated excess within 2x
// the limit is a slip; persistence with any limit excess still excludes.
func TestSoakSkippedFramePersistence(t *testing.T) {
	at := time.Unix(1000, 0)
	skips := loadgen.Pacing{SentFrames: 158, SkippedFrames: 2}
	sample := func(second int) loadgen.Stats {
		return loadgen.Stats{At: at.Add(time.Duration(second) * time.Second), Interval: time.Second, Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: time.Millisecond, Max: time.Millisecond}}
	}
	for _, tc := range []struct {
		name   string
		mutate func([]loadgen.Stats)
		want   int
		cause  string
	}{
		{"isolated 1.25% skipped", func(s []loadgen.Stats) { s[0].IntervalPacing = skips }, 0, ""},
		{"consecutive skipped", func(s []loadgen.Stats) { s[0].IntervalPacing, s[1].IntervalPacing = skips, skips }, 2, "limit excess in consecutive samples"},
		{"skipped next to a lateness slip", func(s []loadgen.Stats) {
			s[0].IntervalPacing = skips
			s[1].IntervalLateness = loadgen.Lateness{P99: 25 * time.Millisecond, Max: 25 * time.Millisecond}
		}, 2, "limit excess in consecutive samples"},
		{"skipped next to CPU excess", func(s []loadgen.Stats) { s[0].IntervalPacing, s[1].CPUPerGOMAXPROCS = skips, 86 }, 2, "CPU 86.00%"},
		{"skipped above 2%", func(s []loadgen.Stats) { s[0].IntervalPacing = loadgen.Pacing{SentFrames: 155, SkippedFrames: 5} }, 1, "skipped frames 3.12% exceeds 1.00%"},
		{"slip beyond the tolerated stall", func(s []loadgen.Stats) {
			s[0].IntervalLateness = loadgen.Lateness{P99: 25 * time.Millisecond, Max: 41 * time.Millisecond}
		}, 1, "interval maximum send-lateness 41ms exceeds the 40ms tolerated stall"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			samples := []loadgen.Stats{sample(10), sample(11), sample(12)}
			tc.mutate(samples)
			r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{{PID: 42, Samples: samples}}}
			b := judgeSaturation(r)
			require.Equal(t, tc.want, b.Saturated)
			if tc.want > 0 {
				require.Contains(t, strings.Join(b.Excluded[0].Causes, "; ")+strings.Join(b.Excluded[len(b.Excluded)-1].Causes, "; "), tc.cause)
			}
		})
	}
}

// The stall bound must reach gaps that interval maximum lateness can hide;
// the recorder retains every gap such a stall can shorten from the ceiling.
func TestSoakToleratedStallFitsGapRetention(t *testing.T) {
	require.Equal(t, 2*loadgen.DefaultLimits().P99+soakAudioTick, callharness.MaxToleratedStall)
	require.Equal(t, callharness.MediaGapCeiling-callharness.MaxToleratedStall, callharness.MediaGapRetention)
}

// Review regression for the soak: an unscheduled near-ceiling gap overlapping
// a tolerated stall of this caller's generator is not a pass.
func TestSoakStalledNearCeilingGapIsInconclusive(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name      string
		pid       int
		second    int
		gap       time.Duration
		want      string
		failed    bool
		hasReason bool
	}{
		{"within this caller's stall bound", 42, 600, 140 * ms, "inconclusive", false, true},
		{"margin equals the bound", 42, 600, 100 * ms, "pass", false, false},
		{"measured failure stays failure", 42, 600, 200 * ms, "fail", true, true},
		{"stall away from the gap", 42, 590, 140 * ms, "pass", false, false},
		{"another generator's stall", 43, 600, 140 * ms, "pass", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGapsRecorded: true, MediaGaps: []callharness.MediaGap{{EndedAt: 600 * time.Second, Duration: tc.gap}}}}
			slip := loadgen.Stats{At: r.StartedAt.Add(time.Duration(tc.second) * time.Second), Interval: time.Second, Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: 25 * ms, Max: 30 * ms}}
			if tc.pid == 42 {
				r.Processes[0].Samples[tc.second] = slip
			} else {
				r.Processes = append(r.Processes, loadgen.ProcessReport{PID: 43, Complete: true, Samples: []loadgen.Stats{slip}})
			}
			b := judgeSaturation(r)
			require.Zero(t, b.Saturated, "the slip itself is tolerated")
			causes := soakContinuityCauses(r, r.Processes[0], r.Processes[0].Calls[0], b)
			require.Equal(t, tc.hasReason, len(causes) == 1, causes)
			if tc.hasReason {
				require.Equal(t, tc.failed, causes[0].Failed)
			}
			if tc.pid == 42 {
				require.Equal(t, tc.want, judgeSoak(c, r).Outcome)
			}
		})
	}
	// A full inventory may have dropped a near-ceiling gap.
	_, r := soakFixture()
	r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGapsRecorded: true, MediaGaps: make([]callharness.MediaGap, callharness.MaxRetainedMediaGaps)}}
	causes := soakContinuityCauses(r, r.Processes[0], r.Processes[0].Calls[0], judgeSaturation(r))
	require.Len(t, causes, 1)
	require.False(t, causes[0].Failed)
	require.Contains(t, causes[0].Reason, "inventory is full")
}
