package main

import (
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

// Calls with a late start offset send past start + duration (#85). The guards
// must see this caller's own saturation there exactly as they do before the
// boundary, while the 1% budget still counts only the run window.

const tailOffset = 800 * time.Millisecond

func tailRun(lateness time.Duration, call *callharness.Report) (loadgen.RunReport, time.Time) {
	at := time.Unix(1000, 0)
	end := at.Add(time.Hour)
	msg := loadgen.Message{Type: "call", PID: 42, Session: "late-call", Call: call, MediaOffset: tailOffset}
	// One sample spans [end - 100ms, end + 900ms].
	sample := loadgen.Stats{At: end.Add(900 * time.Millisecond), Interval: time.Second, Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: lateness, Max: lateness}}
	return loadgen.RunReport{StartedAt: at, Duration: time.Hour, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{{PID: 42, Calls: []loadgen.Message{msg}, Samples: []loadgen.Stats{sample}}}}, end
}

func TestSoakTailLossDuringOwnSaturationIsInconclusive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lostAt   time.Duration
		lateness time.Duration
		want     string
	}{
		{"before the run window ends", time.Hour - 200*time.Millisecond, 50 * time.Millisecond, "inconclusive"},
		{"in the delayed media tail", time.Hour + 100*time.Millisecond, 50 * time.Millisecond, "inconclusive"},
		{"in the tail without saturation", time.Hour + 100*time.Millisecond, time.Millisecond, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := &callharness.Report{HungUpAt: time.Hour + 1300*time.Millisecond}
			call.SentAudio.Frames = 1
			call.Recording.Delivery = &callharness.WholeCallDelivery{Audio: callharness.ContentDelivery{Sent: 1, Lost: 1, LostAt: []time.Duration{tc.lostAt}}}
			r, end := tailRun(tc.lateness, call)
			call.StartedAt = r.StartedAt
			b := judgeSaturation(r)
			d := judgeDelivery(r, b)
			require.Equal(t, tc.want, d.Outcome, d.Calls[0].Causes)
			if tc.want == "fail" {
				require.Empty(t, b.Excluded)
				return
			}
			require.Equal(t, 1, b.Saturated, "the budget counts only the run window's last second")
			require.Len(t, b.Excluded, 1)
			if tc.lostAt > time.Hour {
				require.Equal(t, end.Add(900*time.Millisecond), b.Excluded[0].End, "guards keep the whole saturated interval")
			}
		})
	}
}

func TestSoakTailGapDuringOwnSaturationIsNotAFailure(t *testing.T) {
	call := &callharness.Report{Tracks: []callharness.TrackReport{{Kind: "audio", MediaGapsRecorded: true, MediaGaps: []callharness.MediaGap{{EndedAt: time.Hour + 600*time.Millisecond, Duration: 400 * time.Millisecond}}}}}
	r, _ := tailRun(50*time.Millisecond, call)
	call.StartedAt = r.StartedAt
	causes := soakContinuityCauses(r, r.Processes[0], r.Processes[0].Calls[0], judgeSaturation(r))
	require.Len(t, causes, 1)
	require.False(t, causes[0].Failed, causes[0].Reason)
	require.Contains(t, causes[0].Reason, "overlaps this caller's generator saturation")
}

func TestSoakTailSamplesEstablishPersistence(t *testing.T) {
	at := time.Unix(1000, 0)
	end := at.Add(100 * time.Second)
	slip := func(ending time.Duration) loadgen.Stats {
		return loadgen.Stats{At: end.Add(ending), Interval: time.Second, Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: 25 * time.Millisecond, Max: 30 * time.Millisecond}}
	}
	for _, tc := range []struct {
		name     string
		offset   time.Duration
		excluded int
	}{
		// The second sample starts 300 ms after the window, inside this
		// process's media tail.
		{"media tail", tailOffset, 2},
		// Without offsets that sample is hangup-only, as before #85.
		{"hangup only", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := loadgen.ProcessReport{PID: 42, Calls: []loadgen.Message{{Type: "call", MediaOffset: tc.offset}}, Samples: []loadgen.Stats{slip(300 * time.Millisecond), slip(1300 * time.Millisecond)}}
			r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{p}}
			b := judgeSaturation(r)
			require.Len(t, b.Excluded, tc.excluded)
			require.Equal(t, min(tc.excluded, 1), b.Saturated, "only the run window's last second counts")
		})
	}
}

// With a fractional duration the budget's last interval is partial. A tail
// sample after the window must not count it.
func TestSoakTailSampleAfterFractionalWindowIsNotBudgeted(t *testing.T) {
	at := time.Unix(1000, 0)
	severe := loadgen.Stats{At: at.Add(31600 * time.Millisecond), Interval: time.Second, Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: 50 * time.Millisecond, Max: 50 * time.Millisecond}}
	p := loadgen.ProcessReport{PID: 42, Calls: []loadgen.Message{{Type: "call", MediaOffset: tailOffset}}, Samples: []loadgen.Stats{severe}}
	r := loadgen.RunReport{StartedAt: at, Duration: 30500 * time.Millisecond, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{p}}
	b := judgeSaturation(r)
	require.Len(t, b.Excluded, 1, "the tail sample stays visible to the guards")
	require.Equal(t, 31, b.Intervals)
	require.Zero(t, b.Saturated)
	require.Equal(t, "pass", b.Outcome)
}
