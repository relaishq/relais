package loadgen

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
)

func TestMeterCountsMissedScheduleAndResetsOnlyInterval(t *testing.T) {
	m := &Meter{}
	at := time.Now()
	for range 98 {
		m.Observe("audio", at, at.Add(time.Millisecond))
	}
	m.Observe("video", at, at.Add(25*time.Millisecond))
	m.Observe("video", at, at.Add(3*time.Second))
	all, interval := m.Sample()
	if all.Packets != 100 || interval.Packets != 100 || all.P50 != time.Millisecond || all.P99 != 25*time.Millisecond || all.Max != 3*time.Second {
		t.Fatalf("bad quantiles: %+v %+v", all, interval)
	}
	m.Observe("audio", at, at.Add(-time.Millisecond))
	all, interval = m.Sample()
	if all.Packets != 101 || interval.Packets != 1 || interval.P99 != 0 {
		t.Fatalf("wrong reset: %+v %+v", all, interval)
	}
}
func TestLimitsAndJudgeDoNotTurnSaturationIntoValid(t *testing.T) {
	limits := DefaultLimits()
	for _, tc := range []struct {
		name  string
		stats Stats
		want  string
	}{
		{"late", Stats{Lateness: Lateness{Packets: 100}, IntervalLateness: Lateness{P99: 21 * time.Millisecond}}, "send lateness"},
		{"cpu", Stats{Lateness: Lateness{Packets: 100}, CPUPerGOMAXPROCS: 86}, "CPU"},
		{"no media", Stats{}, "no successful media"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			causes := limits.Causes(tc.stats)
			if len(causes) != 1 || !strings.Contains(causes[0], tc.want) {
				t.Fatal(causes)
			}
			r := RunReport{Processes: []ProcessReport{{Complete: true, Saturated: true, Causes: causes}}}
			r.Judge()
			if r.Outcome != "inconclusive" {
				t.Fatal(r)
			}
		})
	}
	s := Stats{Lateness: Lateness{Packets: 1}, CPUPerGOMAXPROCS: 85, IntervalLateness: Lateness{P99: 20 * time.Millisecond}}
	if len(limits.Causes(s)) != 0 {
		t.Fatal("limits are inclusive")
	}
}
func TestReadProcessRejectsMissingDuplicateAndForeignEvidence(t *testing.T) {
	var data bytes.Buffer
	writer := NewWriter(&data)
	writer.Write(Message{Type: "stats", PID: 42, Stats: &Stats{Lateness: Lateness{Packets: 10}}})
	writer.Write(Message{Type: "call", PID: 42, CallID: 0, Call: &callharness.Report{}})
	complete := data.String()
	writer.Write(Message{Type: "done", PID: 42})
	p, err := ReadProcess(&data, 42, 1, DefaultLimits())
	if err != nil || !p.Complete || p.Saturated {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = ReadProcess(strings.NewReader(complete), 42, 1, DefaultLimits())
	if err != nil || p.Complete {
		t.Fatal("missing done must be incomplete")
	}
	var duplicate bytes.Buffer
	encoder := json.NewEncoder(&duplicate)
	for range 2 {
		_ = encoder.Encode(Message{Type: "call", PID: 42, CallID: 0, Call: &callharness.Report{}})
	}
	if _, err := ReadProcess(&duplicate, 42, 1, DefaultLimits()); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := ReadProcess(strings.NewReader(complete), 43, 1, DefaultLimits()); err == nil {
		t.Fatal("foreign PID accepted")
	}
	if _, err := ReadProcess(strings.NewReader("{broken"), 42, 1, DefaultLimits()); err == nil {
		t.Fatal("truncation accepted")
	}
}
func TestResourceSampler(t *testing.T) {
	s := &Sampler{}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	m := &Meter{}
	at := time.Now()
	m.Observe("audio", at, at)
	stats, err := s.Sample(m)
	if err != nil || stats.GOMAXPROCS < 1 || stats.RuntimeBytes == 0 || stats.PeakRSSBytes == 0 || stats.Lateness.Packets != 1 {
		t.Fatalf("%+v %v", stats, err)
	}
}

func TestYardstickCoverageAndFailuresRemainSeparateFromGeneratorValidity(t *testing.T) {
	events := make([]callharness.MoveReport, 5)
	for i := range events {
		events[i].Kind = "move"
		events[i].Measurement.AudioLoss.Trusted = i < 4
	}
	report := &callharness.Report{Moves: events}
	run := RunReport{Processes: []ProcessReport{{Complete: true, Calls: []Message{{Call: report}}}}}
	run.Judge()
	if run.Outcome != "valid" {
		t.Fatal(run)
	}
	find := func(metric string) MetricRow {
		for _, row := range run.Yardstick {
			if row.Metric == metric {
				return row
			}
		}
		t.Fatalf("missing %s", metric)
		return MetricRow{}
	}
	if row := find("audio_loss"); row.Outcome != "pass" || row.TrustedCoverage != .8 {
		t.Fatal(row)
	}
	events[3].Measurement.AudioLoss.Trusted = false
	run.Judge()
	if row := find("audio_loss"); row.Outcome != "inconclusive" {
		t.Fatal(row)
	}
	events[0].Measurement.AudioLoss.Failed = true
	run.Judge()
	if row := find("audio_loss"); row.Outcome != "fail" {
		t.Fatal(row)
	}
	run.Processes[0].Saturated = true
	run.Processes[0].Causes = []string{"send lateness"}
	run.Judge()
	if run.Outcome != "inconclusive" || find("audio_loss").Outcome != "fail" {
		t.Fatal(run)
	}
}
func TestReadProcessUsesActualSaturationLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		late   time.Duration
		cpu    float64
		causes int
	}{
		{"lateness", 21 * time.Millisecond, 10, 1}, {"cpu", time.Millisecond, 86, 1}, {"both", 21 * time.Millisecond, 86, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			w := NewWriter(&data)
			w.Write(Message{Type: "stats", PID: 42, Stats: &Stats{Lateness: Lateness{Packets: 100, P99: time.Millisecond}, IntervalLateness: Lateness{P99: tc.late}, CPUPerGOMAXPROCS: tc.cpu}})
			w.Write(Message{Type: "call", PID: 42, Call: &callharness.Report{}})
			w.Write(Message{Type: "done", PID: 42})
			p, err := ReadProcess(&data, 42, 1, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			r := RunReport{Processes: []ProcessReport{p}}
			r.Judge()
			if !p.Complete || !p.Saturated || len(p.Causes) != tc.causes || r.Outcome != "inconclusive" {
				t.Fatalf("%+v %+v", p, r)
			}
		})
	}
}
