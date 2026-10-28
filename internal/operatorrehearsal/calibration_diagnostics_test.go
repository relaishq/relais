package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
)

func TestCalibrationThresholdsExplainValuesAndLimits(t *testing.T) {
	for _, row := range []struct {
		rule            string
		observed, limit float64
		mutate          func(int, *loadgen.Stats)
	}{
		{"generator-clock-delta-ns-max", float64(26 * time.Millisecond), float64(25 * time.Millisecond), func(i int, s *loadgen.Stats) {
			if i == 1 {
				s.At = s.At.Add(26 * time.Millisecond)
			}
		}},
		{"generator-cpu-percent-max", 86, 85, func(_ int, s *loadgen.Stats) { s.CPUPerGOMAXPROCS = 86 }},
		{"generator-p99-ns-max", float64(21 * time.Millisecond), float64(20 * time.Millisecond), func(_ int, s *loadgen.Stats) {
			s.IntervalLateness.P99 = 21 * time.Millisecond
			s.IntervalLateness.Max = 21 * time.Millisecond
		}},
		{"generator-max-ns-max", float64(501 * time.Millisecond), float64(500 * time.Millisecond), func(_ int, s *loadgen.Stats) { s.IntervalLateness.Max = 501 * time.Millisecond }},
		{"generator-skipped-percent-max", 100.0 / 31, 1, func(_ int, s *loadgen.Stats) { s.IntervalPacing.SkippedFrames = 1 }},
		{"generator-interval-ns-max", float64(2 * time.Second), float64(time.Second + 25*time.Millisecond), func(i int, s *loadgen.Stats) {
			s.Interval = 2 * time.Second
			s.At = s.At.Add(time.Duration(i) * time.Second)
		}},
	} {
		t.Run(row.rule, func(t *testing.T) {
			c, err := testCalibrate(t, calibrationRows(row.mutate))
			if c.Passed || !slices.Contains(c.Causes, row.rule) {
				t.Fatal("threshold failure has no cause", c, err)
			}
			want := CalibrationCheck{row.rule, row.observed, row.limit}
			if !slices.Contains(c.Checks, want) {
				t.Fatal("threshold lost its observed value or limit", want, c.Checks)
			}
			var log bytes.Buffer
			r := &rehearsal{report: &Report{CPU: CPUReport{Calibration: c}}, diagnostics: &log}
			r.recordCalibration("calibration-evidence-rejected", err)
			r.recordError("calibration-failed", err)
			line, _, _ := strings.Cut(log.String(), "\n")
			var logged Calibration
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "calibration-rejected ")), &logged) != nil || !slices.Contains(logged.Checks, want) || len(logged.Causes) == 0 || strings.Contains(log.String(), "unclassified") {
				t.Fatal("log lost rejection evidence", log.String())
			}
		})
	}
}

func TestCalibrationReportKeepsClippedMediaWindow(t *testing.T) {
	for _, row := range []struct {
		name string
		raw  string
		want CalibrationMediaWindow
	}{
		{"phased-first-empty-final", calibrationShape(60, 1202877166, 173312663, false), CalibrationMediaWindow{
			FirstVideoSendOffsetMS: 1173.312663, HangUpOffsetMS: 61203.492696,
			FlaggedIntervals: []CalibrationFlaggedInterval{{1173.312663, 2202.877166}},
			ClippedFlaggedMS: 1029.564503, MediaWindowMS: 60030.180033,
		}},
		{"adjacent-merged", calibrationRows(func(i int, s *loadgen.Stats) {
			if i == 50 || i == 51 {
				s.CPUPerGOMAXPROCS = 86
			}
		}), CalibrationMediaWindow{1000, 101000, []CalibrationFlaggedInterval{{51000, 53000}}, 2000, 100000}},
		{"unflagged", calibrationRows(nil), CalibrationMediaWindow{1000, 101000, []CalibrationFlaggedInterval{}, 0, 100000}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, err := testCalibrate(t, row.raw)
			if err != nil {
				t.Fatal(c, err)
			}
			r := passingReport()
			r.CPU.Calibration, r.CPU.Phases = c, []Calibration{c}
			var out bytes.Buffer
			if err := r.Write(&out); err != nil {
				t.Fatal(err)
			}
			var saved Report
			if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
				t.Fatal(err)
			}
			for _, actual := range []Calibration{saved.CPU.Calibration, saved.CPU.Phases[0]} {
				w := actual.MediaWindow
				if !reflect.DeepEqual(w.FlaggedIntervals, row.want.FlaggedIntervals) || w.FirstVideoSendOffsetMS != row.want.FirstVideoSendOffsetMS || w.HangUpOffsetMS != row.want.HangUpOffsetMS || math.Abs(w.ClippedFlaggedMS-row.want.ClippedFlaggedMS) > 1e-9 || math.Abs(w.MediaWindowMS-row.want.MediaWindowMS) > 1e-9 {
					t.Fatal("report lost judge window", w, row.want)
				}
				var total float64
				for _, f := range w.FlaggedIntervals {
					total += f.EndOffsetMS - f.StartOffsetMS
				}
				if math.Abs(total-w.ClippedFlaggedMS) > 1e-9 || math.Abs((w.HangUpOffsetMS-w.FirstVideoSendOffsetMS)-w.MediaWindowMS) > 1e-9 || math.Abs(100*total/w.MediaWindowMS-actual.FlaggedPercent) > 1e-12 {
					t.Fatal("cannot reconstruct budget from report", actual)
				}
				encoded, _ := json.Marshal(w)
				if strings.Contains(string(encoded), "2027-") || strings.Contains(string(encoded), "started_at") {
					t.Fatal("window contains wall-clock identity", string(encoded))
				}
			}
		})
	}
	// Both report locations sanitize non-finite offsets before JSON encoding.
	r := passingReport()
	bad := Calibration{MediaWindow: CalibrationMediaWindow{HangUpOffsetMS: math.Inf(1)}}
	r.CPU.Calibration, r.CPU.Phases = bad, []Calibration{bad}
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	var saved Report
	if json.Unmarshal(out.Bytes(), &saved) != nil || saved.CPU.Calibration.MediaWindow.HangUpOffsetMS != 0 || saved.CPU.Phases[0].MediaWindow.HangUpOffsetMS != 0 {
		t.Fatal("unsafe window retained", out.String())
	}
}

func TestCalibrationReportPreservesAndSanitizesChecks(t *testing.T) {
	c, err := testCalibrate(t, calibrationRows(func(_ int, s *loadgen.Stats) { s.CPUPerGOMAXPROCS = 86 }))
	if err != nil {
		t.Fatal(err)
	}
	r := passingReport()
	r.CPU.Calibration = c
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	var saved Report
	if json.Unmarshal(out.Bytes(), &saved) != nil || !slices.Equal(c.Checks, saved.CPU.Calibration.Checks) || !slices.Equal(c.Causes, saved.CPU.Calibration.Causes) {
		t.Fatal("report lost calibration diagnostics", out.String())
	}
	r.CPU.Calibration = Calibration{Checks: []CalibrationCheck{{"credential-secret", 1, 2}, {"flagged-percent-max", math.Inf(1), 1}}}
	out.Reset()
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "credential-secret") || json.Unmarshal(out.Bytes(), &saved) != nil || len(saved.CPU.Calibration.Causes) == 0 {
		t.Fatal("failed calibration was unnamed or disclosed free text", out.String())
	}
}
