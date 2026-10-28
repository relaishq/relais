package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestCalibrationFinalEmptySampleUsesJudgeWindow(t *testing.T) {
	raw := calibrationShape(60, 1202877166, 173312663, false)
	c, err := testCalibrate(t, raw)
	wantMedia := int64(60203492696 - 173312663)
	wantFlagged := int64(1202877166 - 173312663)
	want := 100 * float64(wantFlagged) / float64(wantMedia)
	if err != nil || c.Passed || c.Samples != 61 || c.CompletedCalls != 1 || c.MediaNS != wantMedia || c.FlaggedNS != wantFlagged || math.Abs(c.FlaggedPercent-want) > 1e-12 || !slices.Contains(c.Causes, "generator-saturation-budget") || slices.Contains(c.Causes, "sample-packets-min") {
		t.Fatal("calibration differs from clipped judge policy", c, err)
	}
}

// Product-emitted shape: phased first sample, completed call, zero-packet final
// sample and terminal done. The first-video timestamp controls budget clipping.
func calibrationShape(count int, first, mediaStart time.Duration, cpuFailure bool) string {
	var wire bytes.Buffer
	e := json.NewEncoder(&wire)
	base := time.Unix(1800000000, 0)
	at := base
	for i := range count {
		interval := time.Second
		if i == 0 {
			interval = first
		}
		if count == 60 && i == 59 {
			interval += 615530
		}
		at = at.Add(interval)
		stats := loadgen.Stats{At: at, Interval: interval, GOMAXPROCS: 2, CPUPerGOMAXPROCS: 1.913163368751658, Lateness: loadgen.Lateness{Packets: 30}, IntervalLateness: loadgen.Lateness{Packets: 30, P99: 4248361, Max: 4248361}, IntervalPacing: loadgen.Pacing{SentFrames: 30}}
		if cpuFailure {
			stats.CPUPerGOMAXPROCS = 86
		}
		_ = e.Encode(loadgen.Message{Type: "stats", PID: 123, Stats: &stats})
	}
	call := callharness.Report{StartedAt: base.Add(-time.Second), FirstVideoSentAt: time.Second + mediaStart, HungUpAt: time.Second + at.Sub(base), SentVideo: callharness.SentTrack{Frames: count * 30}, Tracks: []callharness.TrackReport{{Kind: "audio"}, {Kind: "video", Video: &callharness.VideoReport{}}}}
	_ = e.Encode(loadgen.Message{Type: "call", PID: 123, Call: &call})
	final := loadgen.Stats{At: at.Add(200 * time.Millisecond), Interval: 200 * time.Millisecond, GOMAXPROCS: 2, Lateness: loadgen.Lateness{Packets: 1800}}
	_ = e.Encode(loadgen.Message{Type: "stats", PID: 123, Stats: &final})
	_ = e.Encode(loadgen.Message{Type: "done", PID: 123})
	return wire.String()
}

func calibrationRows(mutate func(int, *loadgen.Stats)) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	at := time.Unix(1800000000, 0)
	for i := range 100 {
		s := loadgen.Stats{At: at.Add(time.Duration(i+1) * time.Second), Interval: time.Second, GOMAXPROCS: 2, CPUPerGOMAXPROCS: 10, Lateness: loadgen.Lateness{Packets: 30}, IntervalLateness: loadgen.Lateness{Packets: 30, P99: time.Millisecond, Max: time.Millisecond}, IntervalPacing: loadgen.Pacing{SentFrames: 30}}
		if mutate != nil {
			mutate(i, &s)
		}
		_ = e.Encode(loadgen.Message{Type: "stats", PID: 123, Stats: &s})
	}
	_ = e.Encode(loadgen.Message{Type: "call", PID: 123, Call: &callharness.Report{StartedAt: at.Add(-time.Second), FirstVideoSentAt: time.Second, HungUpAt: 101 * time.Second, SentVideo: callharness.SentTrack{Frames: 3000}, Tracks: []callharness.TrackReport{{Kind: "audio"}, {Kind: "video", Video: &callharness.VideoReport{}}}}})
	_ = e.Encode(loadgen.Message{Type: "done", PID: 123})
	return b.String()
}

func TestCalibrationRequiresDecoderCounters(t *testing.T) {
	for _, field := range []string{"Errors", "Dropped"} {
		raw := strings.Replace(calibrationRows(nil), `"`+field+`":0,`, "", 1)
		if c, err := testCalibrate(t, raw); err != nil || c.Passed {
			t.Fatal("missing decoder counter accepted as zero", field)
		}
	}
}
func TestCalibrationWeightsIntervalsAndKeepsPeak(t *testing.T) {
	c, e := testCalibrate(t, calibrationRows(nil))
	if e != nil || !c.Passed || c.Samples != 100 || c.PeakP99NS != int64(time.Millisecond) {
		t.Fatal(c, e)
	}
	c, e = testCalibrate(t, calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 50 {
			s.IntervalLateness.P99 = 21 * time.Millisecond
			s.IntervalLateness.Max = 21 * time.Millisecond
		}
	}))
	if e != nil || !c.Passed || c.FlaggedNS != int64(time.Second) || c.FlaggedPercent != 1 || c.PeakP99NS != int64(21*time.Millisecond) {
		t.Fatal(c, e)
	}
	c, e = testCalibrate(t, calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 50 || i == 51 {
			s.CPUPerGOMAXPROCS = 86
		}
	}))
	if e != nil || c.Passed || c.FlaggedPercent != 2 {
		t.Fatal(c, e)
	}
}
func TestCalibrationMissingAndMalformedFail(t *testing.T) {
	for _, raw := range []string{"", `{"type":"stats","pid":123,"stats":{"gomaxprocs":2}}`, "not-json", calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 2 {
			s.At = s.At.Add(-2 * time.Second)
		}
	})} {
		c, e := testCalibrate(t, raw)
		if e == nil && c.Passed {
			t.Fatal("missing evidence qualified")
		}
		if len(c.Causes) == 0 {
			t.Fatal("failed calibration has no named cause", c, e)
		}
	}
}
func TestCalibrationLatenessAndStallsFail(t *testing.T) {
	for _, change := range []func(*loadgen.Stats){func(s *loadgen.Stats) {
		s.IntervalLateness.P99 = 21 * time.Millisecond
		s.IntervalLateness.Max = 21 * time.Millisecond
	}, func(s *loadgen.Stats) { s.IntervalLateness.Max = 501 * time.Millisecond }, func(s *loadgen.Stats) { s.IntervalPacing.SkippedFrames = 1 }} {
		c, e := testCalibrate(t, calibrationRows(func(_ int, s *loadgen.Stats) { change(s) }))
		if e != nil || c.Passed {
			t.Fatal(c, e)
		}
	}
}

func TestCalibrationUsesTimeRatherThanTickPercentage(t *testing.T) {
	c, e := testCalibrate(t, calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 50 {
			s.Interval = 2 * time.Second
		}
		if i >= 50 {
			s.At = s.At.Add(time.Second)
		}
	}))
	if e != nil || c.Passed || c.FlaggedNS != int64(2*time.Second) || c.MediaNS != int64(100*time.Second) || c.FlaggedPercent != 2 {
		t.Fatal(c, e)
	}
}
