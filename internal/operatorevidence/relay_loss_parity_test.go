package operatorevidence

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

// Feed the same ordinary caller and generator receipts to the existing pinned
// calibration entry and R. Only R's two affected copies add restart evidence.
// No existing judge source, fixture or exception ledger is modified.
func TestRelayLossSharedGuardParity(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = root
	head, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	output := filepath.Join(t.TempDir(), "judge")
	judge, err := BuildPinned(ctx, root, strings.TrimSpace(string(head)), compiler, output)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		stats func(int, *loadgen.Stats)
		call  func(*callharness.Report)
		want  string
	}{
		{"clean", nil, nil, "pass"},
		{"cpu-boundary", func(_ int, s *loadgen.Stats) { s.CPUPerGOMAXPROCS = loadgen.DefaultLimits().CPUPercent }, nil, "pass"},
		{"cpu", func(_ int, s *loadgen.Stats) { s.CPUPerGOMAXPROCS = 86 }, nil, "inconclusive"},
		{"p99", func(_ int, s *loadgen.Stats) { s.IntervalLateness.P99 = 21 * time.Millisecond }, nil, "inconclusive"},
		{"max", func(_ int, s *loadgen.Stats) { s.IntervalLateness.Max = 501 * time.Millisecond }, nil, "inconclusive"},
		{"skips", func(_ int, s *loadgen.Stats) { s.IntervalPacing = loadgen.Pacing{SentFrames: 98, SkippedFrames: 2} }, nil, "inconclusive"},
		{"time-weighted-under-one-percent", func(i int, s *loadgen.Stats) {
			if i == 150 {
				s.CPUPerGOMAXPROCS = 86
			}
		}, nil, "pass"},
		{"reversed-interval-starts", func(i int, s *loadgen.Stats) {
			base := s.At.Add(-time.Duration(i) * time.Second)
			if i == 1 {
				s.At = base.Add(2 * time.Second)
				s.Interval = 10 * time.Millisecond
				s.CPUPerGOMAXPROCS = 100
			} else {
				s.At = base.Add(time.Duration(i)*time.Second + 975*time.Millisecond)
				if i == 2 {
					s.CPUPerGOMAXPROCS = 100
				}
			}
		}, func(c *callharness.Report) {
			c.FirstVideoSentAt = 1980 * time.Millisecond
			c.HungUpAt = 100980 * time.Millisecond
			for i := range c.Tracks {
				c.Tracks[i].LastArrival = c.HungUpAt
			}
		}, "inconclusive"},
		{"clock-step", func(i int, s *loadgen.Stats) {
			if i > 100 {
				s.At = s.At.Add(time.Second)
			}
		}, nil, "inconclusive"},
		{"cumulative-drift", func(i int, s *loadgen.Stats) { s.At = s.At.Add(time.Duration(i) * time.Millisecond) }, nil, "inconclusive"},
		{"interval-over-1025ms", func(i int, s *loadgen.Stats) {
			s.Interval = 1026 * time.Millisecond
			s.At = s.At.Add(time.Duration(i) * 26 * time.Millisecond)
		}, nil, "inconclusive"},
		{"cadence", func(i int, s *loadgen.Stats) {
			if i == 100 {
				s.Interval = 500 * time.Millisecond
			}
			if i >= 100 {
				s.At = s.At.Add(-500 * time.Millisecond)
			}
			if i == 200 {
				s.Interval += 500 * time.Millisecond
				s.At = s.At.Add(500 * time.Millisecond)
			}
		}, nil, "inconclusive"},
		{"start-coverage", func(_ int, s *loadgen.Stats) { s.At = s.At.Add(2100 * time.Millisecond) }, nil, "inconclusive"},
		{"no-regular-tick", func(i int, s *loadgen.Stats) {
			s.Interval = 1100 * time.Millisecond
			s.At = s.At.Add(time.Duration(i) * 100 * time.Millisecond)
		}, nil, "inconclusive"},
		{"missing-interval-sends", func(_ int, s *loadgen.Stats) { s.Lateness.Packets = 0 }, nil, "inconclusive"},
		{"decrypt", nil, func(c *callharness.Report) { c.DecryptionFailures.Replay = 1 }, "fail"},
		{"duplicate", nil, func(c *callharness.Report) { c.Tracks[0].DuplicatePackets = 1 }, "fail"},
		{"caller-loss", nil, func(c *callharness.Report) { c.Moves = []callharness.MoveReport{{Lost: true}} }, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Unix(1800000000, 0).UTC()
			c := callharness.Report{StartedAt: base, FirstVideoSentAt: time.Second, HungUpAt: 200 * time.Second, SentVideo: callharness.SentTrack{Frames: 6000}, Tracks: []callharness.TrackReport{{Kind: "audio", SSRC: 1, Packets: 1000, MediaGap: 20 * time.Millisecond, LastArrival: 200 * time.Second}, {Kind: "video", SSRC: 2, Packets: 1000, MediaGap: 33 * time.Millisecond, LastArrival: 200 * time.Second, Video: &callharness.VideoReport{}}}}
			if tc.call != nil {
				tc.call(&c)
			}
			var raw strings.Builder
			var samples []json.RawMessage
			appendJSON := func(v any) {
				b, e := json.Marshal(v)
				if e != nil {
					t.Fatal(e)
				}
				raw.Write(b)
				raw.WriteByte('\n')
			}
			for i := 1; i <= 200; i++ {
				s := loadgen.Stats{At: base.Add(time.Duration(i) * time.Second), Interval: time.Second, GOMAXPROCS: 2, Lateness: loadgen.Lateness{Packets: 100}, IntervalPacing: loadgen.Pacing{SentFrames: 100}}
				if tc.stats != nil {
					tc.stats(i, &s)
				}
				m := loadgen.Message{Type: "stats", PID: 42, Stats: &s}
				b, e := json.Marshal(m)
				if e != nil {
					t.Fatal(e)
				}
				samples = append(samples, b)
				appendJSON(m)
			}
			statsWire := raw.String()
			appendJSON(loadgen.Message{Type: "call", PID: 42, Call: &c})
			appendJSON(loadgen.Message{Type: "done", PID: 42})
			calibration, e := judge.Calibrate(ctx, raw.String())
			if e != nil {
				t.Fatal(e)
			}
			var legacy struct {
				Passed           bool     `json:"passed"`
				Causes           []string `json:"causes"`
				DuplicatePackets uint64   `json:"duplicatePackets"`
			}
			if e = json.Unmarshal(calibration, &legacy); e != nil {
				t.Fatal(e)
			}
			if legacy.Passed != (tc.want == "pass" || tc.name == "duplicate") {
				t.Fatal("legacy fixture differs", legacy)
			}
			rules := map[string]string{
				"interval-over-1025ms": "generator-interval-ns-max",
				"cadence":              "generator-cadence-ns-min",
				"start-coverage":       "generator-media-span-covered",
				"no-regular-tick":      "generator-regular-samples-min",
			}
			if rule := rules[tc.name]; rule != "" && !slices.Contains(legacy.Causes, rule) {
				t.Fatal("legacy fixture missed its intended rule", rule, legacy)
			}
			if tc.name == "duplicate" && legacy.DuplicatePackets != 1 {
				t.Fatal("legacy duplicate projection differs", legacy)
			}
			in := relayLossParityInput(t, judge, c, samples, statsWire)

			got := relayjudge.Judge(in)
			if got.Outcome != tc.want {
				t.Fatal("R differs", got, legacy)
			}
		})
	}
	testRelayLossEventWindowParity(t, ctx, judge, compiler, output)
}

func relayLossParityInput(t *testing.T, judge PinnedJudge, c callharness.Report, samples []json.RawMessage, statsWire string) relayjudge.Input {
	t.Helper()
	scenario, e := operator.BuildScenarioIdentity(operator.Scenario{SchemaVersion: 1, RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, Caller: operator.Identity{SourceCommit: judge.Commit}, Judge: operator.Identity{SourceCommit: judge.Commit, SHA256: judge.SHA256}}, "")
	if e != nil {
		t.Fatal(e)
	}
	in := relayjudge.Input{Version: 1, RunID: "parity", Scenario: scenario, PID: 42, Sessions: []string{"a", "b", "c", "d"}, TargetSlot: "relay", SurvivorEndpoint: "127.0.0.2:9000", Restore: "restored", Samples: samples}
	var raw strings.Builder
	appendJSON := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	raw.WriteString(statsWire)
	for i, session := range in.Sessions {
		slot := "relay-two"
		copyCall := c
		var episodes []callharness.RelayRestartEpisode
		if i < 2 {
			slot = "relay"
			copyCall.ICERestarts = 1
			episodes = []callharness.RelayRestartEpisode{{RestartStartNS: 12 * time.Second, AppliedNS: 13 * time.Second, Generation: 8, AnswerCandidate: in.SurvivorEndpoint, LastBeforeNS: 10 * time.Second, FirstVideoNS: 14 * time.Second, FirstAudioNS: 14 * time.Second, ArrivalsBeforeApplied: []callharness.RelayArrivalCount{{Kind: "audio", SSRC: 1}, {Kind: "video", SSRC: 2}}}}
		}
		in.Preflight = append(in.Preflight, relayjudge.Assignment{Session: session, Slot: slot, Generation: 7})
		appendJSON(struct {
			loadgen.Message
			Episodes []callharness.RelayRestartEpisode `json:"relayLossEpisodes,omitempty"`
		}{loadgen.Message{Type: "call", PID: 42, CallID: i, Session: session, Call: &copyCall}, episodes})
	}
	appendJSON(loadgen.Message{Type: "done", PID: 42})
	in.CallerJSONL = raw.String()
	return in
}

// Exercise unmodified legacy production helpers in BuildPinned's owned archive.
// The same sample messages, media span and raw event edges go through R's Judge.
func testRelayLossEventWindowParity(t *testing.T, ctx context.Context, judge PinnedJudge, compiler, output string) {
	t.Helper()
	type eventFixture struct {
		Name                    string
		Samples                 []json.RawMessage
		Start, End, First, Last time.Time
		Want                    bool
	}
	var fixtures []eventFixture
	for _, tc := range []struct {
		name  string
		flag  int
		drift time.Duration
		want  bool
	}{
		{"left-touch", 9, 0, true},
		{"right-touch", 16, 0, true},
		{"inside", 10, 0, false},
		{"outside", 150, 0, true},
		{"left-drift", 9, 25 * time.Millisecond, false},
		{"right-drift", 16, 25 * time.Millisecond, false},
		{"left-negative-drift", 9, -25 * time.Millisecond, false},
		{"right-negative-drift", 16, -25 * time.Millisecond, false},
	} {
		t.Run("event-"+tc.name, func(t *testing.T) {
			base := time.Unix(1800000000, 0).UTC()
			c := callharness.Report{StartedAt: base, FirstVideoSentAt: time.Second, HungUpAt: 200 * time.Second, SentVideo: callharness.SentTrack{Frames: 6000}, Tracks: []callharness.TrackReport{
				{Kind: "audio", SSRC: 1, Packets: 1000, MediaGap: 20 * time.Millisecond, LastArrival: 200 * time.Second},
				{Kind: "video", SSRC: 2, Packets: 1000, MediaGap: 33 * time.Millisecond, LastArrival: 200 * time.Second, Video: &callharness.VideoReport{}},
			}}
			var samples []json.RawMessage
			var wire strings.Builder
			for i := 1; i <= 200; i++ {
				s := loadgen.Stats{At: base.Add(time.Duration(i) * time.Second), Interval: time.Second, GOMAXPROCS: 2, Lateness: loadgen.Lateness{Packets: 100}, IntervalPacing: loadgen.Pacing{SentFrames: 100}}
				if i >= 50 && i < 100 {
					s.At = s.At.Add(tc.drift)
				}
				if i == tc.flag {
					s.CPUPerGOMAXPROCS = 100
				}
				b, err := json.Marshal(loadgen.Message{Type: "stats", PID: 42, Stats: &s})
				if err != nil {
					t.Fatal(err)
				}
				samples = append(samples, b)
				wire.Write(b)
				wire.WriteByte('\n')
			}
			in := relayLossParityInput(t, judge, c, samples, wire.String())
			got := relayjudge.Judge(in)
			if (got.Outcome == "pass") != tc.want || slices.Contains(got.Causes, "relay-loss-generator") == tc.want {
				t.Fatal(got)
			}
			fixtures = append(fixtures, eventFixture{tc.name, samples, base.Add(time.Second), base.Add(200 * time.Second), base.Add(9 * time.Second), base.Add(15 * time.Second), tc.want})
		})
	}
	archive := filepath.Join(output, "source", "cmd", "operator-media-judge")
	raw, err := json.Marshal(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(archive, "relay_loss_parity.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(archive, "relay_loss_parity_test.go"), []byte(legacyEventParityTest), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, compiler, "test", "-count=1", "-run", "^TestRelayLossEventParity$", "./cmd/operator-media-judge")
	cmd.Dir = filepath.Join(output, "source")
	cmd.Env = buildEnvironment()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("legacy event parity: %v\n%s", err, out)
	}
	t.Logf("legacy event-window parity: %d shared fixtures", len(fixtures))
}

const legacyEventParityTest = `package main
import (
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "testing"
 "time"
)
func TestRelayLossEventParity(t *testing.T) {
 raw, err := os.ReadFile("relay_loss_parity.json")
 if err != nil {t.Fatal(err)}
 var fixtures []struct {
  Name string
  Samples []json.RawMessage
  Start, End, First, Last time.Time
  Want bool
 }
 if err = json.Unmarshal(raw,&fixtures); err != nil {t.Fatal(err)}
 for _, tc := range fixtures {
  t.Run(tc.Name,func(t *testing.T) {
   dir:=t.TempDir()
   statsDir:=filepath.Join(dir,"stats")
   if err:=os.Mkdir(statsDir,0700); err!=nil {t.Fatal(err)}
   for i, raw:=range tc.Samples {
    if err:=os.WriteFile(filepath.Join(statsDir,fmt.Sprintf("%04d.json",i)),raw,0600);err!=nil {t.Fatal(err)}
   }
   samples,err:=loadSamples(dir,42,false)
   if err!=nil {t.Fatal(err)}
   if err=sampleSpan(samples,tc.Start,tc.End);err!=nil {t.Fatal(err)}
   budget:=saturationBudget("parity",samples,tc.Start,tc.End)
   // Match the event exclusion path, including measured excursion padding.
   excursion:=statsClockExcursion(samples)
   first,last:=tc.First.Add(-excursion),tc.Last.Add(excursion)
   pass:=budget.Outcome=="pass" && eventSpan(samples,first,last)
   for _, f:=range budget.Flagged {
    if f.Start.Before(last) && f.End.After(first) {pass=false}
   }
   if pass!=tc.Want {t.Fatalf("legacy event pass=%v want=%v",pass,tc.Want)}
  })
 }
}
`
