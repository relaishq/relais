package relayjudge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/stretchr/testify/require"
)

func TestGeneratorAggregatesWorstViolation(t *testing.T) {
	for _, row := range []struct {
		rule                string
		first, second, want float64
	}{
		{"generator-cadence-ns-min", 900000000, 500000000, 500000000},
		{"generator-cpu-percent-max", 86, 99, 99},
	} {
		t.Run(row.rule, func(t *testing.T) {
			g := &GeneratorEvidence{}
			g.check(row.rule, row.first, 1, false)
			g.check(row.rule, row.second, 1, true)
			require.Equal(t, row.want, g.Checks[0].Observed)
			require.Equal(t, 2, g.Checks[0].Count)
			require.Equal(t, 1, g.Checks[0].EventOverlaps)
		})
	}
}

func TestGeneratorExplainsEventRefusal(t *testing.T) {
	in, calls := fixture(t)
	var sample loadgen.Message
	require.NoError(t, json.Unmarshal(in.Samples[11], &sample))
	sample.Stats.CPUPerGOMAXPROCS = 86
	in.Samples[11], _ = json.Marshal(sample)
	result := Judge(wire(t, in, calls))
	require.Equal(t, "inconclusive", result.Outcome)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	var saved struct {
		Generator struct {
			Checks []struct {
				Rule                 string
				Observed, Limit      float64
				Count, EventOverlaps int
			}
		}
	}
	require.NoError(t, json.Unmarshal(raw, &saved))
	require.NotEmpty(t, saved.Generator.Checks, "shared generator refusal must name the rule and values")
	check := saved.Generator.Checks[0]
	require.Equal(t, "generator-cpu-percent-max", check.Rule)
	require.Equal(t, float64(86), check.Observed)
	require.Equal(t, float64(85), check.Limit)
	require.Equal(t, 1, check.Count)
	require.Equal(t, 1, check.EventOverlaps)
}

func TestGeneratorExplainsPolicyRules(t *testing.T) {
	for _, row := range []struct {
		rule            string
		observed, limit float64
		mutate          func(int, *loadgen.Stats)
	}{
		{"generator-p99-ns-max", 21000000, 20000000, func(i int, s *loadgen.Stats) {
			if i == 11 {
				s.IntervalLateness.P99 = 21 * time.Millisecond
			}
		}},
		{"generator-max-ns-max", 501000000, 500000000, func(i int, s *loadgen.Stats) {
			if i == 11 {
				s.IntervalLateness.Max = 501 * time.Millisecond
			}
		}},
		{"generator-skipped-percent-max", 2, 1, func(i int, s *loadgen.Stats) {
			if i == 11 {
				s.IntervalPacing = loadgen.Pacing{SentFrames: 98, SkippedFrames: 2}
			}
		}},
		{"generator-interval-ns-max", 1026000000, 1025000000, func(i int, s *loadgen.Stats) {
			if i == 11 {
				s.Interval = 1026 * time.Millisecond
			}
			if i >= 11 {
				s.At = s.At.Add(26 * time.Millisecond)
			}
		}},
		{"generator-clock-delta-ns-max", 26000000, 25000000, func(i int, s *loadgen.Stats) {
			if i >= 11 {
				s.At = s.At.Add(26 * time.Millisecond)
			}
		}},
		{"generator-clock-excursion-ns-max", 26000000, 25000000, func(i int, s *loadgen.Stats) { s.At = s.At.Add(time.Duration(i) * time.Millisecond) }},
		{"generator-timestamps-ordered", 0, 1, func(i int, s *loadgen.Stats) {
			if i == 11 {
				s.At = s.At.Add(-time.Second)
			}
		}},
		{"generator-cadence-ns-min", 500000000, 975000000, func(i int, s *loadgen.Stats) {
			if i == 11 {
				s.Interval = 500 * time.Millisecond
			}
			if i >= 11 {
				s.At = s.At.Add(-500 * time.Millisecond)
			}
		}},
		{"generator-regular-samples-min", 0, 1, func(i int, s *loadgen.Stats) {
			s.Interval = 1100 * time.Millisecond
			s.At = s.At.Add(time.Duration(i+1) * 100 * time.Millisecond)
		}},
		{"generator-start-coverage-ns-max", 1100000000, 1000000000, func(_ int, s *loadgen.Stats) { s.At = s.At.Add(2100 * time.Millisecond) }},
		{"generator-end-coverage-ns-max", 100000000, 0, func(_ int, s *loadgen.Stats) { s.At = s.At.Add(-100 * time.Millisecond) }},
		{"generator-flagged-percent-max", 100, 1, func(_ int, s *loadgen.Stats) { s.CPUPerGOMAXPROCS = 86 }},
	} {
		t.Run(row.rule, func(t *testing.T) {
			in, calls := fixture(t)
			for i, raw := range in.Samples {
				var m loadgen.Message
				require.NoError(t, json.Unmarshal(raw, &m))
				row.mutate(i, m.Stats)
				in.Samples[i], _ = json.Marshal(m)
			}
			result := Judge(wire(t, in, calls))
			require.Equal(t, "inconclusive", result.Outcome)
			require.NotNil(t, result.Generator)
			var found bool
			for _, c := range result.Generator.Checks {
				require.True(t, GeneratorRuleAllowed(c.Rule))
				if c.Rule == row.rule {
					found = true
					require.Equal(t, row.observed, c.Observed)
					require.Equal(t, row.limit, c.Limit)
					require.Positive(t, c.Count)
				}
			}
			require.True(t, found, "missing rule %s: %+v", row.rule, result.Generator)
		})
	}
}

func TestGeneratorExplainsIndependentSampleJoin(t *testing.T) {
	in, calls := fixture(t)
	in = wire(t, in, calls)
	// Same sample count and timestamps, different measurements. Neither stream
	// may substitute for the other, even when both independently qualify.
	var m loadgen.Message
	require.NoError(t, json.Unmarshal(in.Samples[11], &m))
	m.Stats.CPUPerGOMAXPROCS = 1
	in.Samples[11], _ = json.Marshal(m)
	result := Judge(in)
	require.Equal(t, "inconclusive", result.Outcome)
	require.Equal(t, 30, result.Generator.Samples)
	require.Equal(t, 30, result.Generator.InlineSamples)
	require.Equal(t, []GeneratorCheck{{Rule: "generator-sample-join", Observed: 0, Limit: 1, Count: 1}}, result.Generator.Checks)
}

func TestGeneratorExplainsEventCoverage(t *testing.T) {
	for _, end := range []bool{false, true} {
		in, calls := fixture(t)
		want := GeneratorCheck{Rule: "generator-event-start-coverage-ns-max", Observed: 500000000, Count: 1}
		calls[0].Episodes[0].Last = ptr(500 * time.Millisecond)
		if end {
			calls[0].Episodes[0].Last = ptr(10 * time.Second)
			calls[0].Episodes[0].Video = ptr(30 * time.Second)
			want = GeneratorCheck{Rule: "generator-event-end-coverage-ns-max", Observed: 1000000000, Count: 1}
		}
		result := Judge(wire(t, in, calls))
		require.Contains(t, result.Generator.Checks, want)
		require.Contains(t, result.Causes, "relay-loss-generator")
	}
}

func TestGeneratorShortRRunWithSeededSampling(t *testing.T) {
	in, calls := fixture(t)
	o := loadgen.ChildOptions{RelayLoss: true, SampleEvery: time.Second, Desync: loadgen.Desync{Seed: 41}}
	ordinary := o
	ordinary.RelayLoss = false
	require.Equal(t, 1202626587*time.Nanosecond, ordinary.FirstSampleDelay(), "A/B/M sampling must stay unchanged")
	for i := range calls {
		calls[i].Call.FirstVideoSentAt = ptr(o.Desync.MediaStart(i))
		calls[i].Call.HungUpAt = ptr(60 * time.Second)
		for j := range calls[i].Call.Tracks {
			calls[i].Call.Tracks[j].LastArrival = ptr(60 * time.Second)
		}
	}
	in.Samples = nil
	at := *calls[0].Call.StartedAt
	for i := 0; i <= 60; i++ {
		interval := time.Second
		if i == 0 {
			interval = o.FirstSampleDelay()
		}
		at = at.Add(interval)
		s := loadgen.Stats{At: at, Interval: interval, GOMAXPROCS: 2, Lateness: loadgen.Lateness{Packets: 100}, IntervalPacing: loadgen.Pacing{SentFrames: 100}}
		raw, err := json.Marshal(loadgen.Message{Type: "stats", PID: in.PID, Stats: &s})
		require.NoError(t, err)
		in.Samples = append(in.Samples, raw)
	}
	result := Judge(wire(t, in, calls))
	require.Equal(t, "pass", result.Outcome, "healthy short R run rejected: %+v", result.Generator)
	require.Zero(t, result.Generator.FlaggedNS)
	require.Empty(t, result.Generator.Checks)
	require.Equal(t, 202626587*time.Nanosecond, o.FirstSampleDelay())
}
