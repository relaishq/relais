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
