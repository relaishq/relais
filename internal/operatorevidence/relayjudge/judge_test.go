package relayjudge

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/relayloss"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

func ptr[T any](v T) *T { return &v }
func fixture(t *testing.T) (Input, []message) {
	t.Helper()
	s, err := operator.BuildScenarioIdentity(operator.Scenario{SchemaVersion: 1, RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, Caller: operator.Identity{SourceCommit: strings.Repeat("a", 40)}, Judge: operator.Identity{SourceCommit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	in := Input{Version: 1, RunID: "run-R", Scenario: s, PID: 42, TargetSlot: "relay", SurvivorEndpoint: "127.0.0.2:9000", Restore: "restored"}
	base := time.Unix(1800000000, 0).UTC()
	var ms []message
	for i := range 4 {
		session := fmt.Sprint("s", i)
		in.Sessions = append(in.Sessions, session)
		slot := "relay"
		if i >= 2 {
			slot = "relay-two"
		}
		in.Preflight = append(in.Preflight, Assignment{session, slot, 7})
		r := &report{StartedAt: ptr(base), HungUpAt: ptr(30 * time.Second), FirstVideoSentAt: ptr(time.Second), SentVideo: &struct{ Frames *int }{ptr(900)}, ICERestarts: ptr(0), DecryptionFailures: &struct{ AuthTag, Replay, Other *int }{ptr(0), ptr(0), ptr(0)}}
		for j, kind := range []string{"audio", "video"} {
			r.Tracks = append(r.Tracks, track{Kind: kind, SSRC: ptr(uint32(j + 1)), Packets: ptr(100), DuplicatePackets: ptr(0), MediaGap: ptr(20 * time.Millisecond), LastArrival: ptr(30 * time.Second)})
		}
		r.Tracks[1].Video = &struct{ OnlineDecode *struct{ Errors *int } }{&struct{ Errors *int }{ptr(0)}}
		m := message{Type: "call", PID: 42, CallID: i, Session: session, Call: r}
		if i < 2 {
			r.ICERestarts = ptr(1)
			m.Episodes = []episode{{Start: ptr(12 * time.Second), Applied: ptr(13 * time.Second), Generation: ptr(uint64(8)), Candidate: ptr(in.SurvivorEndpoint), Last: ptr(10 * time.Second), Video: ptr(14 * time.Second), Audio: ptr(14 * time.Second), Counts: []arrival{{"audio", ptr(uint32(1)), ptr(0)}, {"video", ptr(uint32(2)), ptr(0)}}}}
		}
		ms = append(ms, m)
	}
	for i := 1; i <= 30; i++ {
		s := loadgen.Stats{At: base.Add(time.Duration(i) * time.Second), Interval: time.Second, GOMAXPROCS: 2, Lateness: loadgen.Lateness{Packets: 100}, IntervalPacing: loadgen.Pacing{SentFrames: 100}}
		raw, _ := json.Marshal(loadgen.Message{Type: "stats", PID: 42, Stats: &s})
		in.Samples = append(in.Samples, raw)
	}
	return in, ms
}
func wire(t *testing.T, in Input, ms []message) Input {
	t.Helper()
	var b strings.Builder
	for _, s := range in.Samples {
		b.Write(s)
		b.WriteByte('\n')
	}
	for _, m := range ms {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	b.WriteString(`{"type":"done","pid":42}` + "\n")
	in.CallerJSONL = b.String()
	return in
}
func TestRelayLossRules(t *testing.T) {
	for _, row := range []struct {
		name, outcome, cause string
		change               func(*Input, []message)
	}{
		{"pass", "pass", "", func(*Input, []message) {}},
		{"strict-boundary", "pass", "", func(_ *Input, m []message) { m[0].Episodes[0].Video = ptr(15 * time.Second) }},
		{"slow", "fail", "relay-loss-slow", func(_ *Input, m []message) { m[0].Episodes[0].Video = ptr(15*time.Second + time.Nanosecond) }},
		{"slow-with-missing", "fail", "relay-loss-slow", func(_ *Input, m []message) {
			m[0].Call.Tracks[0].MediaGap = ptr(relayloss.Budget + 1)
			m[0].Episodes = nil
		}},
		{"missing-record", "inconclusive", "relay-loss-episode-count", func(_ *Input, m []message) { m[0].Episodes = nil }},
		{"extra-restart", "inconclusive", "relay-loss-episode-count", func(_ *Input, m []message) { m[0].Episodes = append(m[0].Episodes, m[0].Episodes[0]) }},
		{"wrong-generation", "inconclusive", "relay-loss-generation-mismatch", func(_ *Input, m []message) { m[0].Episodes[0].Generation = ptr(uint64(9)) }},
		{"wrong-candidate", "inconclusive", "relay-loss-candidate-mismatch", func(_ *Input, m []message) { m[0].Episodes[0].Candidate = ptr("127.0.0.3:9000") }},
		{"bad-order", "inconclusive", "relay-loss-anchor-order", func(_ *Input, m []message) { m[0].Episodes[0].Last = m[0].Episodes[0].Start }},
		{"early-media", "inconclusive", "relay-loss-media-order-violated", func(_ *Input, m []message) { m[0].Episodes[0].Counts[0].Count = ptr(1) }},
		{"missing-track-count", "inconclusive", "relay-loss-missing", func(_ *Input, m []message) { m[0].Episodes[0].Counts = m[0].Episodes[0].Counts[:1] }},
		{"unrecovered", "fail", "relay-loss-unrecovered", func(_ *Input, m []message) {
			m[0].Episodes[0].Video = ptr(time.Duration(0))
			m[0].Episodes[0].Audio = ptr(time.Duration(0))
		}},
		{"unobserved", "inconclusive", "relay-loss-missing", func(_ *Input, m []message) {
			m[0].Episodes[0].Video = ptr(time.Duration(0))
			m[0].Episodes[0].Audio = ptr(time.Duration(0))
			m[0].Call.HungUpAt = ptr(19 * time.Second)
			for i := range m[0].Call.Tracks {
				m[0].Call.Tracks[i].LastArrival = ptr(19 * time.Second)
			}
		}},
		{"unaffected-restart", "inconclusive", "relay-loss-unaffected-restart", func(_ *Input, m []message) { m[2].Episodes = m[0].Episodes; m[2].Call.ICERestarts = ptr(1) }},
		{"unaffected-slow", "fail", "relay-loss-slow", func(_ *Input, m []message) { m[2].Call.Tracks[0].MediaGap = ptr(6 * time.Second) }},
		{"trailing-slow", "fail", "relay-loss-slow", func(_ *Input, m []message) { m[2].Call.Tracks[0].LastArrival = ptr(24 * time.Second) }},
		{"missing-gap", "inconclusive", "relay-loss-missing", func(_ *Input, m []message) { m[0].Call.Tracks[0].MediaGap = nil }},
		{"overflow-gap", "inconclusive", "relay-loss-missing", func(_ *Input, m []message) { m[0].Call.Tracks[0].MediaGap = ptr(time.Duration(1<<63 - 1)) }},
		{"restore", "fail", "relay-loss-restore-failed", func(in *Input, _ []message) { in.Restore = "failed" }},
		{"target-insufficient", "inconclusive", "relay-loss-target-insufficient", func(in *Input, m []message) {
			in.TargetInsufficient = true
			for i := range m {
				m[i].Episodes = nil
				m[i].Call.ICERestarts = ptr(0)
			}
		}},
		{"decrypt", "fail", "relay-loss-decryption", func(_ *Input, m []message) { m[0].Call.DecryptionFailures.AuthTag = ptr(1) }},
		{"duplicate", "fail", "relay-loss-duplicate", func(_ *Input, m []message) { m[0].Call.Tracks[0].DuplicatePackets = ptr(1) }},
		{"lost", "fail", "relay-loss-call-lost", func(in *Input, _ []message) { in.LostSessions = []string{"s0"} }},
		{"caller-lost", "fail", "relay-loss-call-lost", func(_ *Input, m []message) { m[0].Call.Moves = []callharness.MoveReport{{Lost: true}} }},
		{"generator", "inconclusive", "relay-loss-generator", func(in *Input, _ []message) {
			var m loadgen.Message
			_ = json.Unmarshal(in.Samples[12], &m)
			m.Stats.CPUPerGOMAXPROCS = 100
			in.Samples[12], _ = json.Marshal(m)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			in, ms := fixture(t)
			row.change(&in, ms)
			got := Judge(wire(t, in, ms))
			if got.Outcome != row.outcome || len(got.Calls) != 4 {
				t.Fatalf("%+v", got)
			}
			if row.cause != "" && !slices.Contains(got.Causes, row.cause) {
				t.Fatalf("missing %s: %+v", row.cause, got)
			}
		})
	}
}

func TestHostileRawRecords(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mutate         func(string) string
		outcome, cause string
	}{
		{"omitted-gap", func(s string) string { return strings.Replace(s, `"MediaGap":20000000,`, "", 1) }, "inconclusive", "relay-loss-missing"},
		{"integer-overflow", func(s string) string {
			return strings.Replace(s, `"MediaGap":20000000`, `"MediaGap":9223372036854775808`, 1)
		}, "inconclusive", "relay-loss-missing"},
		{"slow-recovery-with-overflowed-snapshot", func(s string) string {
			s = strings.Replace(s, `"firstVideoNs":14000000000`, `"firstVideoNs":16000000000`, 1)
			return strings.Replace(s, `"MediaGap":20000000`, `"MediaGap":9223372036854775808`, 1)
		}, "fail", "relay-loss-slow"},
		{"slow-recovery-with-malformed-count", func(s string) string {
			s = strings.Replace(s, `"firstVideoNs":14000000000`, `"firstVideoNs":16000000000`, 1)
			return strings.Replace(s, `"count":0`, `"count":"bad"`, 1)
		}, "inconclusive", "relay-loss-missing"},
		{"duplicate-key", func(s string) string {
			return strings.Replace(s, `"MediaGap":20000000`, `"MediaGap":20000000,"MediaGap":0`, 1)
		}, "inconclusive", "relay-loss-missing"},
		{"alias", func(s string) string { return strings.Replace(s, `"MediaGap":20000000`, `"mediagap":20000000`, 1) }, "inconclusive", "relay-loss-missing"},
		{"missing-done", func(s string) string { return strings.Replace(s, `{"type":"done","pid":42}`, "", 1) }, "inconclusive", "relay-loss-missing"},
		{"slow-and-missing", func(s string) string {
			s = strings.Replace(s, `"MediaGap":20000000`, `"MediaGap":6000000000`, 1)
			return strings.Replace(s, `"LastArrival":30000000000,`, "", 1)
		}, "fail", "relay-loss-slow"},
		{"slow-and-malformed-sibling", func(s string) string {
			s = strings.Replace(s, `"MediaGap":20000000`, `"MediaGap":6000000000`, 1)
			return strings.Replace(s, `"LastArrival":30000000000`, `"LastArrival":"bad"`, 1)
		}, "fail", "relay-loss-slow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, m := fixture(t)
			in = wire(t, in, m)
			original := in.CallerJSONL
			in.CallerJSONL = tc.mutate(original)
			if in.CallerJSONL == original {
				t.Fatal("mutation did not match")
			}
			got := Judge(in)
			if got.Outcome != tc.outcome || !slices.Contains(got.Causes, tc.cause) || len(got.Calls) != 4 {
				t.Fatal(got)
			}
		})
	}
}
func TestRelayLossTrustBoundary(t *testing.T) {
	t.Run("no-answer-without-counts-is-missing", func(t *testing.T) {
		in, m := fixture(t)
		e := &m[0].Episodes[0]
		e.Applied = ptr(time.Duration(0))
		e.Generation = ptr(uint64(0))
		e.Candidate = ptr("")
		e.Video = ptr(time.Duration(0))
		e.Audio = ptr(time.Duration(0))
		e.Counts = nil
		got := Judge(wire(t, in, m))
		if got.Outcome != "inconclusive" || !slices.Contains(got.Calls[0].Causes, "relay-loss-missing") {
			t.Fatal(got)
		}
	})
	t.Run("partial-is-missing", func(t *testing.T) {
		in, m := fixture(t)
		m[0].Episodes[0].Audio = nil
		got := Judge(wire(t, in, m))
		if got.Outcome != "inconclusive" {
			t.Fatal(got)
		}
	})
	t.Run("counts-bind-track", func(t *testing.T) {
		in, m := fixture(t)
		m[0].Episodes[0].Counts[0].SSRC = ptr(uint32(9))
		got := Judge(wire(t, in, m))
		if got.Outcome != "inconclusive" {
			t.Fatal(got)
		}
	})
	t.Run("independent-slow-survives-media-order", func(t *testing.T) {
		in, m := fixture(t)
		m[0].Episodes[0].Counts[0].Count = ptr(1)
		m[0].Call.Tracks[0].MediaGap = ptr(6 * time.Second)
		got := Judge(wire(t, in, m))
		if got.Outcome != "fail" || !slices.Contains(got.Causes, "relay-loss-media-order-violated") {
			t.Fatal(got)
		}
	})
	t.Run("bound-gap-value", func(t *testing.T) {
		in, m := fixture(t)
		got := Judge(wire(t, in, m))
		if got.Calls[0].Gap != 4*time.Second || got.Calls[2].Gap != 20*time.Millisecond || got.Calls[2].Role != "unaffected" {
			t.Fatal(got)
		}
	})
	t.Run("same-generator-receipts", func(t *testing.T) {
		in, m := fixture(t)
		in = wire(t, in, m)
		in.Samples = in.Samples[1:]
		got := Judge(in)
		if got.Outcome != "inconclusive" || !slices.Contains(got.Causes, "relay-loss-generator") {
			t.Fatal(got)
		}
	})
	t.Run("early-arrival-does-not-qualify-slow-recovery", func(t *testing.T) {
		in, m := fixture(t)
		m[0].Episodes[0].Counts[0].Count = ptr(1)
		m[0].Episodes[0].Video = ptr(16 * time.Second)
		got := Judge(wire(t, in, m))
		if got.Outcome != "inconclusive" {
			t.Fatal(got)
		}
	})
}

func TestDuplicateRecordsCannotEraseFailure(t *testing.T) {
	in, m := fixture(t)
	in = wire(t, in, m)
	m[0].Call.Tracks[0].MediaGap = ptr(6 * time.Second)
	extra, err := json.Marshal(m[0])
	if err != nil {
		t.Fatal(err)
	}
	// The existing judge retains failure facts even when the population or
	// terminal sequence is invalid. Ambiguous JSON members stay quarantined.
	in.CallerJSONL += string(extra) + "\n"
	got := Judge(in)
	if got.Outcome != "fail" || !slices.Contains(got.Causes, "relay-loss-slow") {
		t.Fatal(got)
	}
}

func TestProcedureInputsCannotQualifyForeignPopulation(t *testing.T) {
	for _, change := range []func(*Input){
		func(in *Input) { in.Preflight[0].Session = "foreign" },
		func(in *Input) { in.Preflight[0].Generation = 1 << 53 },
		func(in *Input) { in.Sessions[1] = in.Sessions[0] },
		func(in *Input) { in.LostSessions = []string{"foreign"} },
		func(in *Input) { in.SurvivorEndpoint = "127.0.0.2:0" },
		func(in *Input) { in.Scenario.Gates = []operator.Gate{operator.RelayDeploy} },
		func(in *Input) { in.Scenario.Digest = "changed" },
	} {
		in, m := fixture(t)
		change(&in)
		got := Judge(wire(t, in, m))
		if got.Outcome != "inconclusive" || len(got.Calls) != 4 {
			t.Fatal(got)
		}
	}
}
func TestGeneratorWindowRejectsLocalStallBelowGlobalBudget(t *testing.T) {
	base := time.Unix(1800000000, 0).UTC()
	for _, tc := range []struct {
		flag int
		max  time.Duration
		pass bool
	}{{0, 0, true}, {40, 0, true}, {19, 0, false}, {21, 2 * time.Second, false}} {
		var samples []loadgen.Stats
		for i := 1; i <= 200; i++ {
			s := loadgen.Stats{At: base.Add(time.Duration(i) * time.Second), Interval: time.Second}
			if i == tc.flag {
				s.CPUPerGOMAXPROCS = 100
				s.IntervalLateness.Max = tc.max
			}
			samples = append(samples, s)
		}
		got := generatorWindow(samples, base.Add(time.Second), base.Add(200*time.Second), []wallWindow{{base.Add(10 * time.Second), base.Add(20 * time.Second)}})
		if got != tc.pass {
			t.Fatal(tc, got)
		}
	}
}

// Initial placement has generation zero (controlplane/relay_status_test.go:39).
// Missing is distinct from this explicit valid value at the raw boundary.
func TestInitialRelayGenerationIsZero(t *testing.T) {
	in, m := fixture(t)
	for i := range in.Preflight {
		in.Preflight[i].Generation = 0
	}
	for i := 0; i < 2; i++ {
		m[i].Episodes[0].Generation = ptr(uint64(1))
	}
	in = wire(t, in, m)
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := DecodeInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := Judge(checked)
	if got.Outcome != "pass" {
		t.Fatal(got)
	}
	missing := strings.Replace(string(raw), `,"generation":0`, "", 1)
	if missing == string(raw) {
		t.Fatal("generation mutation did not match")
	}
	if _, err := DecodeInput([]byte(missing)); err == nil {
		t.Fatal("missing preflight generation accepted as zero")
	}
}

func TestArrivalCountsMustCoverEachTrack(t *testing.T) {
	for _, counts := range [][]arrival{nil, {}, {{"audio", ptr(uint32(1)), ptr(0)}}, {{"audio", ptr(uint32(1)), ptr(0)}, {"audio", ptr(uint32(1)), ptr(0)}}} {
		in, m := fixture(t)
		m[0].Episodes[0].Counts = counts
		got := Judge(wire(t, in, m))
		if got.Outcome != "inconclusive" {
			t.Fatal(got)
		}
	}
}

func TestOmittedArrivalCountsAreMissing(t *testing.T) {
	in, m := fixture(t)
	in = wire(t, in, m)
	original := in.CallerJSONL
	in.CallerJSONL = strings.Replace(in.CallerJSONL, `,"arrivalsBeforeApplied":[{"kind":"audio","ssrc":1,"count":0},{"kind":"video","ssrc":2,"count":0}]`, "", 1)
	if original == in.CallerJSONL {
		t.Fatal("omission did not match")
	}
	got := Judge(in)
	if got.Outcome != "inconclusive" || !slices.Contains(got.Calls[0].Causes, "relay-loss-missing") {
		t.Fatal(got)
	}
}

func TestSpecificEpisodeCausesDoNotAddMissing(t *testing.T) {
	for _, tc := range []struct {
		name, cause string
		change      func(*message)
	}{
		{"absent", "relay-loss-episode-count", func(m *message) { m.Episodes = nil }},
		{"extra", "relay-loss-episode-count", func(m *message) { m.Episodes = append(m.Episodes, m.Episodes[0]) }},
		{"restart-count", "relay-loss-ice-restart-missing", func(m *message) { *m.Call.ICERestarts = 0 }},
		{"early-media", "relay-loss-media-order-violated", func(m *message) { *m.Episodes[0].Counts[0].Count = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, ms := fixture(t)
			tc.change(&ms[0])
			got := Judge(wire(t, in, ms))
			if got.Outcome != "inconclusive" || !slices.Equal(got.Calls[0].Causes, []string{tc.cause}) {
				t.Fatal(got)
			}
			ms[0].Call.Tracks[0].MediaGap = nil
			got = Judge(wire(t, in, ms))
			if !slices.Contains(got.Calls[0].Causes, "relay-loss-missing") {
				t.Fatal("independent missing gap hidden", got)
			}
		})
	}
}

func TestGeneratorWindowPadsLargestClockExcursion(t *testing.T) {
	base := time.Unix(1800000000, 0).UTC()
	for _, drift := range []time.Duration{0, 25 * time.Millisecond, -25 * time.Millisecond} {
		var samples []loadgen.Stats
		for i := 1; i <= 200; i++ {
			s := loadgen.Stats{At: base.Add(time.Duration(i) * time.Second), Interval: time.Second}
			if i >= 50 && i < 100 {
				s.At = s.At.Add(drift)
			}
			if i == 9 {
				s.CPUPerGOMAXPROCS = 100
			}
			samples = append(samples, s)
		}
		// The unpadded left edge is 10ms after the flagged interval ends.
		// A later excursion (even one reversed before hangup) pads both edges.
		got := generatorWindow(samples, base.Add(time.Second), base.Add(200*time.Second), []wallWindow{{base.Add(9*time.Second + 10*time.Millisecond), base.Add(20 * time.Second)}})
		if got != (drift == 0) {
			t.Fatal(drift, got)
		}
	}
}
