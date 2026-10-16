package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestSoakEndedKeyRetention(t *testing.T) {
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not supplied")
	}
	require.NoError(t, redisendpoint.ValidateDev(addr))
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer func() { require.NoError(t, client.Close()) }()
	prefix := "relais:soak-ended-test:" + strconvID() + ":"
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want bool
	}{
		{"observed VM retention", 265 * time.Second, true},
		{"store retention", sessionstore.EndedRetention, true},
		{"immortal", 0, false},
		{"overlong", sessionstore.EndedRetention + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := prefix + "ended:{sess:bounded}"
			require.NoError(t, client.Set(context.Background(), key, "ended", tc.ttl).Err())
			defer func() { require.NoError(t, client.Del(context.Background(), key).Err()) }()
			k, err := observeSoakKey(context.Background(), client, prefix, key)
			require.NoError(t, err)
			require.Equal(t, "session-ended", k.Class)
			require.Equal(t, sessionstore.EndedRetention.Milliseconds(), k.LimitMS)
			require.Equal(t, tc.want, boundedSoakKey(k))
			causes := leakCauses(loadgen.LeakReport{Checked: true, Keys: []loadgen.RetainedKey{k}})
			require.Equal(t, !tc.want, len(causes) > 0)
		})
	}
}

func TestSoakEndedKeyBound(t *testing.T) {
	limit := sessionstore.EndedRetention.Milliseconds()
	for _, ttl := range []int64{-2, -1, 0, 1, 264907, 264920, limit, limit + 1} {
		k := loadgen.RetainedKey{Key: "run:ended:{sess:bounded}", Class: "session-ended", TTLMS: ttl}
		require.Equal(t, ttl > 0 && ttl <= limit, boundedSoakKey(k), "TTL=%d", ttl)
	}
}

func TestSoakIsolatedTickSlip(t *testing.T) {
	at := time.Unix(1000, 0)
	r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits()}
	r.Processes = []loadgen.ProcessReport{{PID: 42, Samples: []loadgen.Stats{
		{At: at.Add(9 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: time.Millisecond}},
		{At: at.Add(10 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: 20100 * time.Microsecond}},
		{At: at.Add(11 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: time.Millisecond}},
	}}}
	b := judgeSaturation(r)
	require.Zero(t, b.Saturated)
	require.Empty(t, b.Excluded)
	require.False(t, eventSaturated(r, loadgen.EventResult{At: at.Add(10 * time.Second), FinishedAt: at.Add(11 * time.Second)}, b))
}

func TestSoakSaturationPersistenceAndImmediateSignals(t *testing.T) {
	at := time.Unix(1000, 0)
	sample := func(second int, late time.Duration) loadgen.Stats {
		return loadgen.Stats{At: at.Add(time.Duration(second) * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: late}}
	}
	for _, tc := range []struct {
		name    string
		samples []loadgen.Stats
		want    int
	}{
		{"isolated VM maximum", []loadgen.Stats{sample(10, 26*time.Millisecond)}, 0},
		{"sustained Mac overload", []loadgen.Stats{sample(10, 23*time.Millisecond), sample(11, 35*time.Millisecond), sample(12, 52*time.Millisecond)}, 3},
		{"exact lateness limits", []loadgen.Stats{sample(10, 20*time.Millisecond), sample(11, 20*time.Millisecond)}, 0},
		{"severe isolated delay", []loadgen.Stats{sample(10, 52*time.Millisecond)}, 1},
		{"missing second is not persistence", []loadgen.Stats{sample(10, 21*time.Millisecond), sample(12, 21*time.Millisecond)}, 0},
		{"recovered between slips", []loadgen.Stats{sample(10, 21*time.Millisecond), sample(11, time.Millisecond), sample(12, 21*time.Millisecond)}, 0},
		{"startup does not establish persistence", []loadgen.Stats{sample(0, 21*time.Millisecond), sample(1, 21*time.Millisecond)}, 0},
		{"hangup does not establish persistence", []loadgen.Stats{sample(100, 21*time.Millisecond), sample(101, 21*time.Millisecond)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{{PID: 42, Samples: tc.samples}}}
			b := judgeSaturation(r)
			require.Equal(t, tc.want, b.Saturated)
			require.Len(t, b.Excluded, tc.want)
			if tc.want > 0 {
				e := loadgen.EventResult{At: at.Add(10 * time.Second), FinishedAt: at.Add(11 * time.Second)}
				require.True(t, eventSaturated(r, e, b), "exclude the first second of sustained overload too")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		pacing loadgen.Pacing
		cpu    float64
		second int
		want   int
	}{
		{"isolated skipped frames", loadgen.Pacing{SentFrames: 157, SkippedFrames: 2}, 0, 10, 0},
		{"exactly twice the skipped limit", loadgen.Pacing{SentFrames: 98, SkippedFrames: 2}, 0, 10, 0},
		{"skipped frames above twice the limit", loadgen.Pacing{SentFrames: 155, SkippedFrames: 5}, 0, 10, 1},
		{"live zero-send stall", loadgen.Pacing{SkippedFrames: 50}, 0, 10, 1},
		{"exact skipped limit", loadgen.Pacing{SentFrames: 99, SkippedFrames: 1}, 0, 10, 0},
		{"CPU excess", loadgen.Pacing{}, 86, 10, 1},
		{"exact CPU limit", loadgen.Pacing{}, 85, 10, 0},
		{"finish accounting after media", loadgen.Pacing{SkippedFrames: 1}, 0, 101, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sample(tc.second, time.Millisecond)
			s.IntervalPacing, s.CPUPerGOMAXPROCS = tc.pacing, tc.cpu
			r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{{PID: 42, Samples: []loadgen.Stats{s}}}}
			require.Equal(t, tc.want, judgeSaturation(r).Saturated)
		})
	}
	// Two generators slipping once each are not sustained overload in either.
	r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{
		{PID: 42, Samples: []loadgen.Stats{sample(10, 21*time.Millisecond)}},
		{PID: 43, Samples: []loadgen.Stats{sample(11, 21*time.Millisecond)}},
	}}
	require.Zero(t, judgeSaturation(r).Saturated)
}

func soakSlip(r *loadgen.RunReport, process, sample int, p99, peak time.Duration) {
	late := &r.Processes[process].Samples[sample].IntervalLateness
	late.P99, late.Max = p99, peak
}

func TestSoakIsolatedSlipKeepsMediaVerdicts(t *testing.T) {
	c, r := soakFixture()
	soakSlip(&r, 0, 31, 21*time.Millisecond, 25*time.Millisecond)
	r.Processes[0].Saturated = true // The unchanged raw load gate still reports this.
	// Both gate margins exceed the 45 ms stall bound: 110 ms to the ceiling, 48 ms to excess.
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].BaselineGapP95 = 38 * time.Millisecond
	r.Judge()
	v := judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome)
	require.Equal(t, "pass", v.Events[0].Outcome)
	require.False(t, v.Events[0].Saturated)
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].Gap = 151 * time.Millisecond
	v = judgeSoak(c, r)
	require.Equal(t, "fail", v.Outcome)
	require.Equal(t, "fail", v.Events[0].Outcome)
}

func soakGapRow(t *testing.T, v soakReport, event int) loadgen.MetricRow {
	t.Helper()
	for _, row := range v.Events[event].Rows {
		if row.Metric == "audio_media_gap" {
			return row
		}
	}
	t.Fatalf("event %d has no audio_media_gap row: %+v", event, v.Events[event].Rows)
	return loadgen.MetricRow{}
}

// A caller stall at the outage start can shorten a measured gap by up to the
// interval maximum lateness plus one audio tick. A pass that this bound could
// turn into a failure is inconclusive, never a pass.
func TestSoakEventStallUntrustsNearGatePass(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name          string
		event, sample int
		p99, peak     time.Duration
		gap, baseline time.Duration
		resumed       time.Duration
		want          string
		trusted       int
	}{
		// A 30 ms maximum gives a 50 ms bound.
		{"move excess within bound", 0, 31, 25 * ms, 30 * ms, 40 * ms, 30 * ms, 0, "inconclusive", 1},
		{"move ceiling within bound", 0, 31, 25 * ms, 30 * ms, 140 * ms, 140 * ms, 0, "inconclusive", 1},
		{"move excess margin equals bound", 0, 31, 25 * ms, 30 * ms, 40 * ms, 40 * ms, 0, "pass", 1},
		{"bound adds one audio tick", 0, 31, 25 * ms, 30 * ms, 40 * ms, 39 * ms, 0, "inconclusive", 1},
		{"move failure stays failure", 0, 31, 25 * ms, 30 * ms, 151 * ms, 140 * ms, 0, "fail", 1},
		{"stall before media resumes", 0, 33, 25 * ms, 30 * ms, 40 * ms, 30 * ms, 33 * time.Second, "inconclusive", 1},
		{"maximum above limit with p99 within it", 0, 31, 15 * ms, 30 * ms, 40 * ms, 30 * ms, 0, "inconclusive", 1},
		{"maximum within limit is the existing envelope", 0, 31, 20 * ms, 20 * ms, 40 * ms, 30 * ms, 0, "pass", 1},
		{"takeover within bound", 2, 151, 25 * ms, 30 * ms, 1960 * ms, 20 * ms, 0, "inconclusive", 0},
		{"takeover margin equals bound", 2, 151, 25 * ms, 30 * ms, 1950 * ms, 20 * ms, 0, "inconclusive", 0},
		{"takeover margin exceeds bound", 2, 151, 25 * ms, 30 * ms, 1949 * ms, 20 * ms, 0, "pass", 1},
		{"takeover failure stays failure", 2, 151, 25 * ms, 30 * ms, 2 * time.Second, 20 * ms, 0, "fail", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			soakSlip(&r, 0, tc.sample, tc.p99, tc.peak)
			move := &r.Processes[0].Calls[0].Call.Moves[tc.event]
			move.Tracks[0].Gap, move.Tracks[0].BaselineGapP95 = tc.gap, tc.baseline
			move.Measurement.MediaResumedAt = tc.resumed
			r.Judge()
			v := judgeSoak(c, r)
			require.False(t, v.Events[tc.event].Saturated, "an isolated slip does not exclude the event")
			require.Equal(t, tc.want, v.Events[tc.event].Outcome)
			row := soakGapRow(t, v, tc.event)
			require.Equal(t, tc.want, row.Outcome)
			require.Equal(t, tc.trusted, row.Trusted)
		})
	}
}

// Review counterexample: p99 does not bound the stamping error. A 39 ms stall
// on the last pre-outage packet turns a true 74 ms gap into 35 ms.
func TestSoakStallMaximumBoundsGap(t *testing.T) {
	c, r := soakFixture()
	meter := &loadgen.Meter{}
	at := r.StartedAt.Add(30 * time.Second)
	for range 197 {
		meter.Observe("audio", at, at.Add(time.Millisecond))
	}
	for range 2 {
		meter.Observe("audio", at, at.Add(35*time.Millisecond))
	}
	meter.Observe("audio", at, at.Add(39*time.Millisecond))
	_, sample := meter.Sample()
	require.Equal(t, 35*time.Millisecond, sample.P99)
	require.Equal(t, 39*time.Millisecond, sample.Max)
	r.Processes[0].Samples[31].IntervalLateness = sample
	track := &r.Processes[0].Calls[0].Call.Moves[0].Tracks[0]
	track.Gap, track.BaselineGapP95 = 35*time.Millisecond, 20*time.Millisecond
	r.Judge()
	v := judgeSoak(c, r)
	require.NotEqual(t, "pass", v.Outcome, "a 74 ms true gap exceeds the 20 ms baseline + 50 ms")
	require.Equal(t, "inconclusive", v.Events[0].Outcome)
	row := soakGapRow(t, v, 0)
	require.Equal(t, "inconclusive", row.Outcome)
	require.Equal(t, 1, row.Trusted)
}

// Review counterexample: 18 gaps of 40 ms, one of 140 ms and one of 151 ms
// have a passing p95 of 140 ms. Stall evidence must not shrink the denominator.
func soakP95Fixture() loadgen.RunReport {
	_, r := soakFixture()
	template := *r.Processes[0].Calls[0].Call
	r.Events[0].Affected = 20
	r.Processes[0].Calls = nil
	for i := range 20 {
		report := template
		report.Moves = append([]callharness.MoveReport(nil), template.Moves...)
		report.Moves[0].Tracks = append([]callharness.MoveTrackReport(nil), template.Moves[0].Tracks...)
		track := &report.Moves[0].Tracks[0]
		track.Gap, track.BaselineGapP95 = 40*time.Millisecond, 30*time.Millisecond
		if i == 18 {
			track.Gap, track.BaselineGapP95 = 140*time.Millisecond, 130*time.Millisecond
		}
		if i == 19 {
			track.Gap, track.BaselineGapP95 = 151*time.Millisecond, 130*time.Millisecond
		}
		r.Processes[0].Calls = append(r.Processes[0].Calls, loadgen.Message{Call: &report})
	}
	return r
}

func TestSoakStallBoundKeepsP95Denominator(t *testing.T) {
	r := soakP95Fixture()
	rows := soakGapRows(r, r.Events[0], 0)
	require.Equal(t, "pass", rows[0].Outcome)
	require.Equal(t, 20, rows[0].Trusted)
	// The stall touches every call, so the 140 ms track could truly be 195 ms.
	soakSlip(&r, 0, 31, 35*time.Millisecond, 35*time.Millisecond)
	rows = soakGapRows(r, r.Events[0], 0)
	require.Equal(t, "inconclusive", rows[0].Outcome, "uncertainty must not become a conclusive failure")
	require.Equal(t, 20, rows[0].Trusted)
	require.Zero(t, rows[0].Failed)
	// A stall in only the 151 ms call's generator cannot move the p95.
	r = soakP95Fixture()
	outlier := r.Processes[0].Calls[19]
	r.Processes[0].Calls = r.Processes[0].Calls[:19]
	r.Processes = append(r.Processes, loadgen.ProcessReport{PID: 43, Calls: []loadgen.Message{outlier}, Samples: []loadgen.Stats{{At: r.StartedAt.Add(31 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: 35 * time.Millisecond, Max: 35 * time.Millisecond}}}})
	rows = soakGapRows(r, r.Events[0], 0)
	require.Equal(t, "pass", rows[0].Outcome)
	require.Equal(t, 20, rows[0].Trusted)
}

func TestSoakStallAwayFromEventKeepsGapVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sample int
	}{
		{"ends one second before the move", 29},
		{"starts when the move ends", 32},
		{"far from the event", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			soakSlip(&r, 0, tc.sample, 35*time.Millisecond, 39*time.Millisecond)
			r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].Gap = 50 * time.Millisecond // 30 ms excess
			r.Judge()
			v := judgeSoak(c, r)
			require.Equal(t, "pass", v.Events[0].Outcome)
			row := soakGapRow(t, v, 0)
			require.Equal(t, "pass", row.Outcome)
			require.Equal(t, 1, row.Trusted)
		})
	}
	// Another generator's slip cannot shorten this call's gap.
	_, r := soakFixture()
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].Gap = 50 * time.Millisecond
	r.Processes = append(r.Processes, loadgen.ProcessReport{PID: 43, Samples: []loadgen.Stats{{At: r.StartedAt.Add(31 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: 35 * time.Millisecond, Max: 39 * time.Millisecond}}}})
	rows := soakGapRows(r, r.Events[0], 0)
	require.Len(t, rows, 1)
	require.Equal(t, "pass", rows[0].Outcome)
	require.Equal(t, 1, rows[0].Trusted)
}

func TestSoakExclusionNamesItsCauses(t *testing.T) {
	at := time.Unix(1000, 0)
	for _, tc := range []struct {
		name     string
		late     time.Duration
		lateness bool
	}{
		{"isolated slip with skipped frames", 25 * time.Millisecond, false},
		{"severe lateness with skipped frames", 45 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := loadgen.Stats{At: at.Add(10 * time.Second), Interval: time.Second, Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: tc.late}, IntervalPacing: loadgen.Pacing{SentFrames: 97, SkippedFrames: 3}}
			r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits(), Processes: []loadgen.ProcessReport{{PID: 42, Samples: []loadgen.Stats{s}}}}
			b := judgeSaturation(r)
			require.Len(t, b.Excluded, 1)
			causes := strings.Join(b.Excluded[0].Causes, "; ")
			require.Contains(t, causes, "skipped frames")
			require.Equal(t, tc.lateness, strings.Contains(causes, "send-lateness"), causes)
		})
	}
}
