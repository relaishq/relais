package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/callharness"
)

func generatorShape(t *testing.T, firstVideo time.Duration, cpu bool) (string, loadgen.ProcessReport, []json.RawMessage) {
	t.Helper()
	var wire bytes.Buffer
	var samples []json.RawMessage
	appendMessage := func(m loadgen.Message) {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		wire.Write(raw)
		wire.WriteByte('\n')
		if m.Type == "stats" {
			samples = append(samples, raw)
		}
	}
	base := time.Unix(1800000000, 0)
	at := base
	for i := range 60 {
		interval := time.Second
		if i == 0 {
			interval = 1202877166
		}
		if i == 59 {
			interval += 615530
		}
		at = at.Add(interval)
		s := loadgen.Stats{At: at, Interval: interval, GOMAXPROCS: 2, CPUPerGOMAXPROCS: 1.913163368751658, Lateness: loadgen.Lateness{Packets: 1800}, IntervalLateness: loadgen.Lateness{Packets: 30, P99: 4248361, Max: 4248361}, IntervalPacing: loadgen.Pacing{SentFrames: 30}}
		if cpu {
			s.CPUPerGOMAXPROCS = 86
		}
		appendMessage(loadgen.Message{Type: "stats", PID: 42, Stats: &s})
	}
	c := callharness.Report{StartedAt: base.Add(-time.Second), FirstVideoSentAt: time.Second + firstVideo, HungUpAt: time.Second + at.Sub(base), SentVideo: callharness.SentTrack{Frames: 1800}, Tracks: []callharness.TrackReport{{Kind: "audio"}, {Kind: "video", Video: &callharness.VideoReport{}}}}
	appendMessage(loadgen.Message{Type: "call", PID: 42, Call: &c})
	tail := loadgen.Stats{At: at.Add(200 * time.Millisecond), Interval: 200 * time.Millisecond, GOMAXPROCS: 2, Lateness: loadgen.Lateness{Packets: 1800}}
	appendMessage(loadgen.Message{Type: "stats", PID: 42, Stats: &tail})
	appendMessage(loadgen.Message{Type: "done", PID: 42})
	p, err := loadgen.ReadSoakProcess(strings.NewReader(wire.String()), 42, 1, loadgen.DefaultLimits())
	if err != nil || !p.Complete {
		t.Fatal(p, err)
	}
	return wire.String(), p, samples
}

func TestCalibrationMatchesGeneratorPolicyAtBothPins(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, commit := range []string{"a0d9744ff1223a8d7978447c907041973e915b08", "85e10436eb6adbbf577b6538b0bbb799aab823b2"} {
		judge, err := operatorevidence.BuildPinned(ctx, root, commit, compiler, filepath.Join(t.TempDir(), "judge"))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range []struct {
			name      string
			first     time.Duration
			cpu, pass bool
		}{
			{"phased-first-and-empty-final", time.Second, false, true},
			{"seeded-media-start", 173312663, false, false},
			{"cpu-rejected", time.Second, true, false},
		} {
			t.Run(commit+"/"+row.name, func(t *testing.T) {
				raw, p, samples := generatorShape(t, row.first, row.cpu)
				start, end := callerWindow(p)
				budget := saturationBudget("caller-1", p.Samples, start, end)
				r := loadgen.RunReport{Processes: []loadgen.ProcessReport{p}}
				want := previewGeneratorGuards(&r, nil, samples)
				if want != row.pass {
					t.Fatal("normal judge fixture differs", want, budget)
				}
				got, err := operatorrehearsal.Calibrate(ctx, judge, raw)
				if err != nil || got.Passed != want || got.Samples != 61 || got.MediaNS != end.Sub(start).Nanoseconds() || math.Abs(got.FlaggedPercent-budget.Percent) > 1e-12 || got.FlaggedNS != int64(math.Round(budget.FlaggedSeconds*1e9)) || slices.Contains(got.Causes, "sample-packets-min") {
					t.Fatal("calibration differs from normal pinned generator path", got, budget, err)
				}
				t.Logf("passed=%t media_ns=%d flagged_ns=%d percent=%.9f", got.Passed, got.MediaNS, got.FlaggedNS, got.FlaggedPercent)
			})
		}
	}
}

func TestGeneratorFlagExtractionKeepsBoundaries(t *testing.T) {
	// Expected decisions are the existing policy's boundary fixtures. Diagnostic
	// extraction must not change its strict comparisons or add a packet minimum.
	for _, row := range []struct {
		s       loadgen.Stats
		flagged bool
	}{
		{loadgen.Stats{Interval: 1025 * time.Millisecond, CPUPerGOMAXPROCS: 85, IntervalLateness: loadgen.Lateness{P99: 20 * time.Millisecond, Max: 500 * time.Millisecond}}, false},
		{loadgen.Stats{Interval: 1025*time.Millisecond + 1}, true},
		{loadgen.Stats{CPUPerGOMAXPROCS: 85.000001}, true},
		{loadgen.Stats{IntervalLateness: loadgen.Lateness{P99: 20*time.Millisecond + 1}}, true},
		{loadgen.Stats{IntervalLateness: loadgen.Lateness{Max: 500*time.Millisecond + 1}}, true},
		{loadgen.Stats{IntervalPacing: loadgen.Pacing{SentFrames: 99, SkippedFrames: 1}}, false},
		{loadgen.Stats{IntervalPacing: loadgen.Pacing{SentFrames: 98, SkippedFrames: 2}}, true},
		{loadgen.Stats{Interval: 200 * time.Millisecond}, false},
	} {
		if got := sampleFlagged(row.s); got != row.flagged {
			t.Fatal("generator flag boundary changed", row, got)
		}
	}
}

func TestGeneratorBudgetRetainsClippedMergedIntervals(t *testing.T) {
	start := time.Unix(1800000000, 0)
	samples := []loadgen.Stats{
		{At: start.Add(time.Second), Interval: 2 * time.Second, CPUPerGOMAXPROCS: 86},
		{At: start.Add(2 * time.Second), Interval: 2 * time.Second, CPUPerGOMAXPROCS: 86},
		{At: start.Add(5 * time.Second), Interval: 2 * time.Second, CPUPerGOMAXPROCS: 86},
		{At: start.Add(7 * time.Second), Interval: time.Second, CPUPerGOMAXPROCS: 86},
	}
	b := saturationBudget("caller-1", samples, start, start.Add(4*time.Second))
	if len(b.clipped) != 2 || !b.clipped[0].Start.Equal(start) || !b.clipped[0].End.Equal(start.Add(2*time.Second)) || !b.clipped[1].Start.Equal(start.Add(3*time.Second)) || !b.clipped[1].End.Equal(start.Add(4*time.Second)) || b.FlaggedSeconds != 3 || b.MediaSeconds != 4 || b.Percent != 75 {
		t.Fatal("diagnostics differ from clipped budget", b, b.clipped)
	}
}
