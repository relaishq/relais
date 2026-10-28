package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestGeneratorCoverageUsesEventHorizon(t *testing.T) {
	t.Parallel()
	c := previewSample().Processes[0].Calls[0].Call
	c.FirstVideoSentAt = 92410 * time.Millisecond
	c.HungUpAt = 1112910 * time.Millisecond
	m := c.Moves[0]
	m.Start, m.End = 120*time.Second, 120*time.Second+time.Millisecond
	m.Distributed.Start = callharness.TimeInterval{Earliest: c.StartedAt.Add(m.Start), Latest: c.StartedAt.Add(m.Start)}
	m.Distributed.End = callharness.TimeInterval{Earliest: c.StartedAt.Add(m.End), Latest: c.StartedAt.Add(m.End)}
	v := &m.Measurement
	v.MediaResumedAt = m.Start + 20*time.Millisecond
	v.SettledAt = 126 * time.Second
	v.Audio.RecoveryLimit, v.Audio.StabilityWindow, v.Audio.EvaluationEnd = 2*time.Second, 5*time.Second, v.SettledAt
	v.Video.RecoveryLimit, v.Video.StabilityWindow, v.Video.EvaluationEnd = 2*time.Second, 5*time.Second, v.SettledAt
	v.FirstNewFrame, v.FirstLiveFrame = 40*time.Millisecond, 60*time.Millisecond
	v.FirstContent, v.FirstNewContent = "fresh", "fresh"
	v.ContentLoss.Baselines = []callharness.ContentLossWindow{{Start: 118 * time.Second, End: 119 * time.Second}}
	v.ObservedUntil = c.HungUpAt // Compaction does not extend a complete measurement.
	c.Moves = []callharness.MoveReport{m}
	r := receipt{Identity: *m.Identity, Start: c.StartedAt.Add(m.Start), End: c.StartedAt.Add(m.End)}
	first, last, err := callerEventWindow(r, c, m)
	if err != nil || !first.Equal(c.StartedAt.Add(118*time.Second)) || !last.Equal(c.StartedAt.Add(129*time.Second)) {
		t.Fatal("MUTATION: event-measurement-horizon", first.Sub(c.StartedAt), last.Sub(c.StartedAt), err)
	}
	// Recorded caller shape: pre-media probes take about 92s; the last row is
	// a partial zero-packet interval ending just after hangup, not hangup + 1s.
	var raw []json.RawMessage
	for i := 0; i <= 1020; i++ {
		s := loadgen.Stats{At: c.StartedAt.Add(93440*time.Millisecond + time.Duration(i)*time.Second), Interval: time.Second}
		if i == 0 {
			s.Interval = 1200 * time.Millisecond
		}
		if i == 1020 {
			s.At = c.StartedAt.Add(c.HungUpAt + time.Millisecond)
			s.Interval = 471 * time.Millisecond
		}
		b, e := json.Marshal(loadgen.Message{Type: "stats", PID: 42, Stats: &s})
		if e != nil {
			t.Fatal(e)
		}
		raw = append(raw, b)
	}
	dir, err := collectGeneratorSamples(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := loadgen.ProcessReport{PID: 42, Calls: []loadgen.Message{{Call: c}}}
	for _, b := range raw {
		var msg loadgen.Message
		if err = json.Unmarshal(b, &msg); err != nil {
			t.Fatal(err)
		}
		p.Samples = append(p.Samples, *msg.Stats)
	}
	report := loadgen.RunReport{Processes: []loadgen.ProcessReport{p}}
	budgets, excluded := applySaturation(dir, &report, []receipt{r}, []string{"caller-1"})
	if len(budgets) != 1 || budgets[0].Outcome != "pass" || len(excluded) != 0 || len(report.Processes[0].Calls[0].Call.Moves) != 1 {
		t.Fatal("event coverage rejected normal pre-media delay or final partial sample", budgets, excluded)
	}
	samples, err := loadSamples(dir+"/caller-1", 42, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range []struct{ first, last time.Time }{
		{samples[0].At.Add(-samples[0].Interval - time.Nanosecond), last},
		{first, samples[len(samples)-1].At.Add(time.Nanosecond)},
	} {
		if eventSpan(samples, edge.first, edge.last) {
			t.Fatal("uncovered event edge accepted")
		}
	}
	// Incomplete measurements still require the conservative observation horizon.
	m.Measurement.Audio.EvaluationEnd = 0
	_, end, err := callerEventWindow(r, c, m)
	if err != nil || !end.Equal(c.StartedAt.Add(c.HungUpAt+time.Second)) || eventSpan(samples, first, end) {
		t.Fatal("incomplete horizon accepted", end, err)
	}
}
