package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

func calibrationRows(mutate func(int, *loadgen.Stats)) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	at := time.Unix(1800000000, 0)
	for i := range 100 {
		s := loadgen.Stats{At: at.Add(time.Duration(i) * time.Second), Interval: time.Second, GOMAXPROCS: 2, CPUPerGOMAXPROCS: 10, Lateness: loadgen.Lateness{Packets: 30}, IntervalLateness: loadgen.Lateness{Packets: 30, P99: time.Millisecond, Max: time.Millisecond}, IntervalPacing: loadgen.Pacing{SentFrames: 30}}
		if mutate != nil {
			mutate(i, &s)
		}
		_ = e.Encode(loadgen.Message{Type: "stats", PID: 123, Stats: &s})
	}
	_ = e.Encode(loadgen.Message{Type: "call", PID: 123, Call: &callharness.Report{Tracks: []callharness.TrackReport{{Kind: "audio"}, {Kind: "video", Video: &callharness.VideoReport{}}}}})
	return b.String()
}

func TestCalibrationRequiresDecoderCounters(t *testing.T) {
	for _, field := range []string{"Errors", "Dropped"} {
		raw := strings.Replace(calibrationRows(nil), `"`+field+`":0,`, "", 1)
		if _, err := Calibrate(raw); err == nil {
			t.Fatal("missing decoder counter accepted as zero", field)
		}
	}
}
func TestCalibrationWeightsIntervalsAndKeepsPeak(t *testing.T) {
	c, e := Calibrate(calibrationRows(nil))
	if e != nil || !c.Passed || c.Samples != 100 || c.PeakP99NS != int64(time.Millisecond) {
		t.Fatal(c, e)
	}
	c, e = Calibrate(calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 50 {
			s.IntervalLateness.P99 = 21 * time.Millisecond
			s.IntervalLateness.Max = 21 * time.Millisecond
		}
	}))
	if e != nil || !c.Passed || c.FlaggedNS != int64(time.Second) || c.FlaggedPercent != 1 || c.PeakP99NS != int64(21*time.Millisecond) {
		t.Fatal(c, e)
	}
	c, e = Calibrate(calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 50 || i == 51 {
			s.CPUPerGOMAXPROCS = 86
		}
	}))
	if e != nil || c.Passed || c.FlaggedPercent != 2 {
		t.Fatal(c, e)
	}
}
func TestCalibrationMissingAndMalformedFail(t *testing.T) {
	for _, raw := range []string{"", `{"type":"stats","pid":123,"stats":{"gomaxprocs":2}}`, "not-json", calibrationRows(func(_ int, s *loadgen.Stats) { s.GOMAXPROCS = 1 }), calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 2 {
			s.At = s.At.Add(-2 * time.Second)
		}
	})} {
		c, e := Calibrate(raw)
		if e == nil && c.Passed {
			t.Fatal("missing evidence qualified")
		}
	}
}
func TestCalibrationLatenessAndStallsFail(t *testing.T) {
	for _, change := range []func(*loadgen.Stats){func(s *loadgen.Stats) {
		s.IntervalLateness.P99 = 21 * time.Millisecond
		s.IntervalLateness.Max = 21 * time.Millisecond
	}, func(s *loadgen.Stats) { s.IntervalLateness.Max = 501 * time.Millisecond }, func(s *loadgen.Stats) { s.IntervalPacing.SkippedFrames = 1 }} {
		c, e := Calibrate(calibrationRows(func(_ int, s *loadgen.Stats) { change(s) }))
		if e != nil || c.Passed {
			t.Fatal(c, e)
		}
	}
}

func TestCalibrationUsesTimeRatherThanTickPercentage(t *testing.T) {
	c, e := Calibrate(calibrationRows(func(i int, s *loadgen.Stats) {
		if i == 50 {
			s.Interval = 2 * time.Second
		}
		if i >= 50 {
			s.At = s.At.Add(time.Second)
		}
	}))
	if e != nil || c.Passed || c.FlaggedNS != int64(2*time.Second) || c.MediaNS != int64(101*time.Second) || c.FlaggedPercent < 1.98 {
		t.Fatal(c, e)
	}
}
