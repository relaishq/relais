package operatorevidence

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
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
	judge, err := BuildPinned(ctx, root, strings.TrimSpace(string(head)), compiler, filepath.Join(t.TempDir(), "judge"))
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
			if tc.name == "duplicate" && legacy.DuplicatePackets != 1 {
				t.Fatal("legacy duplicate projection differs", legacy)
			}
			scenario, e := operator.BuildScenarioIdentity(operator.Scenario{SchemaVersion: 1, RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, Caller: operator.Identity{SourceCommit: judge.Commit}, Judge: operator.Identity{SourceCommit: judge.Commit, SHA256: judge.SHA256}}, "")
			if e != nil {
				t.Fatal(e)
			}
			in := relayjudge.Input{Version: 1, RunID: "parity", Scenario: scenario, PID: 42, Sessions: []string{"a", "b", "c", "d"}, TargetSlot: "relay", SurvivorEndpoint: "127.0.0.2:9000", Restore: "restored", Samples: samples}
			raw.Reset()
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
			got := relayjudge.Judge(in)
			if got.Outcome != tc.want {
				t.Fatal("R differs", got, legacy)
			}
		})
	}
}
