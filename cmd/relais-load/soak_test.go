package main

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/internal/capacity"
	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/loadgen"
	"github.com/relais/internal/processrss"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

func memorySamples(at time.Time, duration time.Duration, rate float64) []capacity.MemorySample {
	var samples []capacity.MemorySample
	for t := time.Duration(0); t <= duration; t += time.Second {
		value := uint64(float64(100<<20) + rate*t.Seconds())
		samples = append(samples, capacity.MemorySample{At: at.Add(t), RSSBytes: value, PhysicalFootprintBytes: value, HeapInUseBytes: value, HeapLiveBytes: value})
	}
	return samples
}
func TestSoakMemoryRegression(t *testing.T) {
	at := time.Unix(1000, 0)
	for _, metric := range []string{"rss", "heap-in-use", "heap-live"} {
		for _, tc := range []struct {
			name        string
			duration    time.Duration
			rate        float64
			incarnation bool
			want        string
		}{
			{"flat", time.Hour, 0, false, "pass"},
			{"growth", time.Hour, 10 << 10, false, "fail"},
			{"short service", 3 * time.Minute, 10 << 10, false, "insufficient-data"},
			{"short worker", 9 * time.Minute, 10 << 10, true, "insufficient-data"},
			{"ten minute worker", 10 * time.Minute, 0, true, "pass"},
		} {
			t.Run(metric+"/"+tc.name, func(t *testing.T) {
				v := judgeMemory("worker", 42, metric, memorySamples(at, tc.duration, tc.rate), at, time.Hour, tc.incarnation)
				require.Equal(t, tc.want, v.Outcome)
				if v.Outcome != "insufficient-data" {
					require.InDelta(t, tc.rate, v.SlopeBytesPerSecond, .001)
				}
			})
		}
	}
	// A setup spike must not poison the slope after the warm-up cutoff.
	samples := memorySamples(at, time.Hour, 0)
	for i := 0; i < 300; i++ {
		samples[i].RSSBytes = 500 << 20
	}
	require.Equal(t, "pass", judgeMemory("relay", 42, "rss", samples, at, time.Hour, false).Outcome)
	samples = memorySamples(at, 20*time.Minute, 0)
	samples[0].RSSBytes = 0
	require.Equal(t, "pass", judgeMemory("replacement", 42, "rss", samples, at.Add(-30*time.Minute), time.Hour, true).Outcome)
}
func TestSoakSeedReproducesPlanAndMix(t *testing.T) {
	first := soakSchedule(time.Hour, 30*time.Second, 41, 3)
	require.Equal(t, first, soakSchedule(time.Hour, 30*time.Second, 41, 3))
	require.False(t, reflect.DeepEqual(first, soakSchedule(time.Hour, 30*time.Second, 42, 3)))
	require.Len(t, first, 119)
	require.Len(t, soakSchedule(10*time.Minute+10*time.Second, 30*time.Second, 41, 3), 20, "nightly has one-event headroom under exact 95% coverage")
	seen := map[string]bool{}
	for _, e := range first[:3] {
		seen[e.Kind] = true
	}
	require.Len(t, seen, 3)
}
func TestSoakSaturationBudgetAndOverlap(t *testing.T) {
	at := time.Unix(1000, 0)
	r := loadgen.RunReport{StartedAt: at, Duration: 100 * time.Second, Limits: loadgen.DefaultLimits()}
	sample := loadgen.Stats{At: at.Add(10 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: 41 * time.Millisecond}}
	r.Processes = []loadgen.ProcessReport{{PID: 42, Samples: []loadgen.Stats{sample}}, {PID: 43, Samples: []loadgen.Stats{sample}}}
	b := judgeSaturation(r)
	require.Equal(t, "pass", b.Outcome)
	require.Equal(t, 1, b.Saturated)
	require.Len(t, b.Excluded, 2)
	require.Equal(t, 1.0, b.Percent)
	event := loadgen.EventResult{At: at.Add(10 * time.Second), FinishedAt: at.Add(11 * time.Second)}
	require.True(t, eventSaturated(r, event, b))
	event.At, event.FinishedAt = at.Add(30*time.Second), at.Add(31*time.Second)
	require.False(t, eventSaturated(r, event, b))
	r.Processes[0].Samples = append(r.Processes[0].Samples, loadgen.Stats{At: at.Add(20 * time.Second), Interval: time.Second, CPUPerGOMAXPROCS: 86})
	require.Equal(t, "inconclusive", judgeSaturation(r).Outcome)
	// Exactly the existing limits remains valid; tail beyond media is excluded.
	r.Processes = []loadgen.ProcessReport{{Samples: []loadgen.Stats{{At: at.Add(time.Second), Interval: time.Second, CPUPerGOMAXPROCS: 85, IntervalLateness: loadgen.Lateness{P99: 20 * time.Millisecond}, IntervalPacing: loadgen.Pacing{SentFrames: 99, SkippedFrames: 1}}, {At: at.Add(110 * time.Second), Interval: time.Second, CPUPerGOMAXPROCS: 100}}}}
	require.Zero(t, judgeSaturation(r).Saturated)
}

func soakFixture() (config, loadgen.RunReport) { return soakFixtureEvents(3, 60*time.Second) }
func soakFixtureEvents(count int, interval time.Duration) (config, loadgen.RunReport) {
	at := time.Unix(1000, 0)
	c := config{Soak: true, Duration: 20 * time.Minute, Callers: 2, Processes: 1, Workers: 3, WorkerFloor: 8, RelayFloor: 8, StoreOpsBudget: 100, StoreBytesBudget: 128 << 10}
	r := capacityFixture()
	r.StartedAt, r.Duration, r.Limits = at, c.Duration, loadgen.DefaultLimits()
	r.SentinelWorker = "worker-0"
	template := r.Processes[0].Calls[0].Call.Moves[0]
	template.Measurement.AudioLoss.Trusted = true
	template.Measurement.Audio.Verdict.Trusted = true
	template.Measurement.ResumeVerdict.Trusted = true
	var moves []callharness.MoveReport
	takeovers := 0
	for i := range count {
		after := 30*time.Second + time.Duration(i)*interval
		kind := []string{"move", "drain", "kill"}[i%3]
		event := loadgen.Event{After: after, Kind: kind, Worker: 1, Count: 1}
		move := template
		move.Tracks = append([]callharness.MoveTrackReport(nil), template.Tracks...)
		move.From, move.To, move.Start, move.End = 0, 1, after, after+time.Second
		if kind == "drain" {
			move.From, move.To = 1, 0
		}
		if kind == "kill" {
			event.Worker = 1
			move.From, move.To = 1, 0
			move.Kind = "takeover"
			takeovers++
		}
		moves = append(moves, move)
		c.Events = append(c.Events, event)
		r.Events = append(r.Events, loadgen.EventResult{Event: event, At: at.Add(after), FinishedAt: at.Add(after + time.Second), Affected: 1, Sessions: []string{"a"}})
	}
	r.Telemetry.Processes[1].Last["relais_control_takeovers_total"] = float64(takeovers)
	r.Processes = []loadgen.ProcessReport{{PID: 42, Complete: true, ExpectedCalls: 2, Lateness: loadgen.Lateness{Packets: 1}, Calls: []loadgen.Message{{Session: "a", Call: &callharness.Report{StartedAt: at, HungUpAt: c.Duration, Moves: moves}}, {Session: "b", Call: &callharness.Report{StartedAt: at, HungUpAt: c.Duration}}}}}
	for _, sample := range memorySamples(at, c.Duration, 0) {
		r.Processes[0].Samples = append(r.Processes[0].Samples, loadgen.Stats{At: sample.At, Interval: time.Second, RSSBytes: sample.RSSBytes, PhysicalFootprintBytes: sample.PhysicalFootprintBytes, HeapInUseBytes: sample.HeapInUseBytes, RuntimeMemory: &loadgen.RuntimeMemory{HeapLiveBytes: sample.HeapLiveBytes}})
	}
	r.Telemetry.Processes = append(r.Telemetry.Processes, capacity.Process{Name: "store", PID: 45}, capacity.Process{Name: "worker-0", PID: 46})
	for i := range r.Telemetry.Processes {
		r.Telemetry.Processes[i].Memory = memorySamples(at, c.Duration, 0)
	}
	r.Leaks = loadgen.LeakReport{Checked: true, ScanControls: map[string]bool{"session_epoch": true, "relay-fencing-hash": true}}
	r.Judge()
	return c, r
}
func TestSoakVerdictRetainsIndependentFailures(t *testing.T) {
	c, r := soakFixture()
	require.Equal(t, "pass", judgeSoak(c, r).Outcome)
	r.Processes[0].Samples[10].IntervalLateness.P99 = 41 * time.Millisecond
	r.Processes[0].Saturated = true
	r.Judge()
	v := judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome, "an isolated excluded interval away from chaos is within budget")
	r.Processes[0].Samples[31].IntervalLateness.P99 = 41 * time.Millisecond
	v = judgeSoak(c, r)
	require.Equal(t, "inconclusive", v.Events[0].Outcome)
	require.True(t, v.Events[0].Saturated)
	require.Equal(t, "inconclusive", v.Outcome)
	r.Telemetry.Processes[0].UnexpectedExit = true
	require.Equal(t, "fail", judgeSoak(c, r).Outcome, "confirmed crash must survive saturation")
	r.Telemetry.Processes[0].UnexpectedExit = false
	r.Processes[0].Calls[0].Call.DecryptionFailures.AuthTag = 1
	require.Equal(t, "fail", judgeSoak(c, r).Outcome, "caller decryption is independently observed")
}
func TestSoakRejectsSeededLeakInventory(t *testing.T) {
	for _, tc := range []struct {
		name string
		leak loadgen.LeakReport
	}{
		{"key", loadgen.LeakReport{Checked: true, StoreKeys: []string{"run:seeded-leftover"}, Keys: []loadgen.RetainedKey{{Key: "run:seeded-leftover", Class: "unknown", TTLMS: 60000}}}},
		{"session", loadgen.LeakReport{Checked: true, Sessions: map[string]int{"worker-1": 1}}},
		{"process", loadgen.LeakReport{Checked: true, Processes: []int{42}}},
		{"listener", loadgen.LeakReport{Checked: true, Listeners: []string{"tcp:127.0.0.1:12345"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			r.Leaks = tc.leak
			require.Equal(t, "fail", judgeSoak(c, r).Outcome)
		})
	}
	c, r := soakFixture()
	r.Leaks.Checked = false
	require.Equal(t, "inconclusive", judgeSoak(c, r).Outcome)
}
func TestSoakRetainsEveryGeneratorInterval(t *testing.T) {
	var data bytes.Buffer
	writer := loadgen.NewWriter(&data)
	for i := range 600 {
		writer.Write(loadgen.Message{Type: "stats", PID: 42, Stats: &loadgen.Stats{At: time.Unix(int64(i), 0), Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: 21 * time.Millisecond}}})
	}
	writer.Write(loadgen.Message{Type: "call", PID: 42, Call: &callharness.Report{}})
	writer.Write(loadgen.Message{Type: "done", PID: 42})
	p, err := loadgen.ReadSoakProcess(&data, 42, 1, loadgen.DefaultLimits())
	require.NoError(t, err)
	require.Len(t, p.Samples, 600)
	require.Zero(t, p.SamplesDropped)
	require.True(t, p.Saturated)
	require.Empty(t, p.Causes)
}
func TestSoakRedisScanFindsSeededLeftover(t *testing.T) {
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not supplied")
	}
	require.NoError(t, processrun.ValidateRedis(addr))
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	defer func() { require.NoError(t, client.Close()) }()
	prefix := "relais:soak-test:" + strings.ReplaceAll(t.Name(), "/", "-") + ":" + strconvID() + ":"
	key := prefix + "seeded-leftover"
	require.NoError(t, client.Set(context.Background(), key, "leftover", time.Minute).Err())
	defer func() { require.NoError(t, client.Del(context.Background(), key).Err()) }()
	keys, err := scanSoakKeys(context.Background(), client, prefix)
	require.NoError(t, err)
	require.Equal(t, []string{key}, keys)
	c, r := soakFixture()
	observation, err := observeSoakKey(context.Background(), client, prefix, key)
	require.NoError(t, err)
	require.Equal(t, "unknown", observation.Class)
	require.Positive(t, observation.TTLMS)
	r.Leaks = loadgen.LeakReport{Checked: true, StoreKeys: keys, Keys: []loadgen.RetainedKey{observation}}
	require.Equal(t, "fail", judgeSoak(c, r).Outcome)
	// Exercise real Redis PTTL sentinels and the persistent relay hash schema.
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want bool
	}{
		{"bounded", time.Minute, true}, {"no TTL", 0, false}, {"overlong", soakSessionLimit + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := prefix + "epoch:{sess:ttl-test}"
			require.NoError(t, client.Set(context.Background(), key, "epoch", tc.ttl).Err())
			defer func() { require.NoError(t, client.Del(context.Background(), key).Err()) }()
			k, err := observeSoakKey(context.Background(), client, prefix, key)
			require.NoError(t, err)
			require.Equal(t, tc.want, boundedSoakKey(k))
		})
	}

	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want bool
	}{{"bounded", 30 * time.Second, true}, {"missing TTL", 0, false}, {"overlong", 31 * time.Second, false}} {
		t.Run("frames/"+tc.name, func(t *testing.T) {
			key := prefix + "frames:{sess:frame-test}"
			require.NoError(t, client.Set(context.Background(), key, "frame", tc.ttl).Err())
			defer func() { require.NoError(t, client.Del(context.Background(), key).Err()) }()
			observation, err := observeSoakKey(context.Background(), client, prefix, key)
			require.NoError(t, err)
			require.Equal(t, "frame-cache", observation.Class)
			require.EqualValues(t, 30000, observation.LimitMS)
			require.Equal(t, tc.want, boundedSoakKey(observation))
		})
	}
	relayKey := prefix + "relay:{relay:ZGVmYXVsdA}"
	require.NoError(t, client.HSet(context.Background(), relayKey, "holder", "", "expires", 0, "forwarder", "", "previous", "").Err())
	defer func() { require.NoError(t, client.Del(context.Background(), relayKey).Err()) }()
	k, err := observeSoakKey(context.Background(), client, prefix, relayKey)
	require.NoError(t, err)
	require.EqualValues(t, -1, k.TTLMS)
	require.True(t, k.Released)
	require.True(t, boundedSoakKey(k))

	counter := prefix + "session_epoch"
	require.NoError(t, client.Set(context.Background(), counter, 1, 0).Err())
	defer func() { require.NoError(t, client.Del(context.Background(), counter).Err()) }()
	scanned, err := scanSoakKeys(context.Background(), client, prefix)
	require.NoError(t, err)
	controls := soakScanControls(scanned, prefix)
	require.True(t, controls["session_epoch"])
	require.True(t, controls["relay-fencing-hash"])
	wrong, err := scanSoakKeys(context.Background(), client, prefix+"wrong:")
	require.NoError(t, err)
	require.Empty(t, wrong)
	for _, found := range soakScanControls(wrong, prefix) {
		require.False(t, found)
	}
	now, err := client.Time(context.Background()).Result()
	require.NoError(t, err)
	require.NoError(t, client.HSet(context.Background(), relayKey, "holder", "held", "expires", now.Add(time.Minute).UnixMilli()).Err())
	k, err = observeSoakKey(context.Background(), client, prefix, relayKey)
	require.NoError(t, err)
	require.False(t, boundedSoakKey(k))
}
func strconvID() string { return time.Now().Format("150405.000000000") }

func TestSoakIndependentWorkerFailureAndCallerLifetime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*loadgen.RunReport)
	}{
		{"worker decryption", func(r *loadgen.RunReport) {
			r.Telemetry.Processes = append(r.Telemetry.Processes, capacity.Process{Name: "worker-0", Last: map[string]float64{"relais_worker_decryption_failures_total": 1}})
		}},
		{"worker overload", func(r *loadgen.RunReport) {
			r.Telemetry.Processes = append(r.Telemetry.Processes, capacity.Process{Name: "worker-0", Peaks: map[string]float64{"relais_worker_active_sessions": 9}})
		}},
		{"lost call", func(r *loadgen.RunReport) { r.Telemetry.Processes[1].Last["relais_control_losses_total"] = 1 }},
		{"expired ownership loss", func(r *loadgen.RunReport) {
			r.Telemetry.Processes[1].Last[`relais_control_losses_total{cause="ownership_expired"}`] = 1
		}},
		{"checkpoint loss", func(r *loadgen.RunReport) {
			r.Telemetry.Processes[1].Last[`relais_control_losses_total{cause="checkpoint_unsafe"}`] = 1
		}},
		{"early hangup", func(r *loadgen.RunReport) { r.Processes[0].Calls[0].Call.HungUpAt = r.Duration - time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			r.Processes[0].Samples[31].CPUPerGOMAXPROCS = 86
			tc.mutate(&r)
			require.Equal(t, "fail", judgeSoak(c, r).Outcome)
		})
	}
}

func TestSoakDetectsOwnedLiveProcessAndReapedProcess(t *testing.T) {
	if err := processrss.Check(); err != nil {
		t.Skipf("current memory sampling unavailable in this build: %v", err)
	}
	child, err := clusterprocess.Start(t.TempDir(), "seeded-process", os.Environ(), "sleep", "2")
	require.NoError(t, err)
	defer child.Stop()
	rss, err := processrss.Current(child.PID())
	require.NoError(t, err)
	require.Positive(t, rss)
	remaining, err := soakProcesses([]*clusterprocess.Child{child})
	require.NoError(t, err)
	require.Contains(t, remaining, child.PID())
	c, r := soakFixture()
	r.Leaks = loadgen.LeakReport{Checked: true, Processes: remaining}
	require.Equal(t, "fail", judgeSoak(c, r).Outcome)
	child.Stop()
	remaining, err = soakProcesses([]*clusterprocess.Child{child})
	require.NoError(t, err)
	require.Empty(t, remaining)
}

func TestSoakEventGapFailureCannotHideInGlobalP95(t *testing.T) {
	c, r := soakFixture()
	r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].Gap = 151 * time.Millisecond
	v := judgeSoak(c, r)
	require.Equal(t, "fail", v.Events[0].Outcome)
	require.Equal(t, "fail", v.Outcome)
	r.Processes[0].Samples[31].CPUPerGOMAXPROCS = 86
	v = judgeSoak(c, r)
	require.Equal(t, "inconclusive", v.Events[0].Outcome)
	require.Equal(t, "inconclusive", v.Outcome, "saturated interruption is ambiguous")
}

func TestSoakEventOverlapIncludesSavedLossObservationHorizon(t *testing.T) {
	c, r := soakFixture()
	r.Processes[0].Calls[0].Call.Moves[0].Measurement.ObservedUntil = 60 * time.Second
	r.Processes[0].Samples[55].CPUPerGOMAXPROCS = 86
	v := judgeSoak(c, r)
	require.True(t, v.Events[0].Saturated, "loss is judged through the saved observation horizon")
	require.Equal(t, "inconclusive", v.Events[0].Outcome)
}

func TestSoakBoundedRetention(t *testing.T) {
	remaining := int64(600)
	for _, tc := range []struct {
		name   string
		keys   []loadgen.RetainedKey
		routes int
		want   string
	}{
		{"bounded", []loadgen.RetainedKey{{Key: "epoch:{sess:a}", Class: "session", TTLMS: 299000}, {Key: "session_epoch", Class: "session-counter", TTLMS: -1}, {Key: "relay", Class: "relay-lease", TTLMS: -1, LeaseRemainingMS: &remaining}}, 0, "pass"},
		{"no TTL", []loadgen.RetainedKey{{Key: "epoch:{sess:a}", Class: "session", TTLMS: -1}}, 0, "fail"},
		{"overlong TTL", []loadgen.RetainedKey{{Key: "epoch:{sess:a}", Class: "session", TTLMS: soakSessionLimit.Milliseconds() + 1}}, 0, "fail"},
		{"route after wait", nil, 1, "fail"},
		{"missing relay expiry", []loadgen.RetainedKey{{Key: "relay", Class: "relay-lease", TTLMS: -1}}, 0, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			r.Leaks = loadgen.LeakReport{Checked: true, ScanControls: map[string]bool{"session_epoch": true, "relay-fencing-hash": true}, Keys: tc.keys, RelayWait: soakRelayWait, RelayObservedAt: time.Now(), Sessions: map[string]int{"relay-routes": tc.routes}}
			for _, k := range tc.keys {
				r.Leaks.StoreKeys = append(r.Leaks.StoreKeys, k.Key)
			}
			require.Equal(t, tc.want, judgeSoak(c, r).Outcome)
		})
	}
	c, r := soakFixture()
	r.Leaks.StoreKeys = []string{"old-key-without-TTL-observation"}
	r.Leaks.Sessions = map[string]int{"relay-routes": 4}
	require.Equal(t, "inconclusive", judgeSoak(c, r).Outcome, "legacy immediate observations do not prove bounded retention or a leak")
	r.Leaks = loadgen.LeakReport{Checked: false, StoreKeys: []string{"unobserved"}, Keys: []loadgen.RetainedKey{{Key: "unobserved", Error: "Redis unavailable"}}, Errors: []string{"Redis unavailable"}}
	require.Equal(t, "inconclusive", judgeSoak(c, r).Outcome)
	remaining++
	require.False(t, boundedSoakKey(loadgen.RetainedKey{Class: "relay-lease", LeaseRemainingMS: &remaining}))
}

func TestSoakMemoryNeedsFiveMinutesAfterWarmup(t *testing.T) {
	at := time.Unix(1000, 0)
	for _, metric := range []string{"rss", "heap-in-use"} {
		require.Equal(t, "insufficient-data", judgeMemory("service", 1, metric, memorySamples(at, 329*time.Second, 1<<20), at, 5*time.Minute, false).Outcome)
		// A one-hour run excludes five minutes; exactly another five minutes is sufficient.
		require.Equal(t, "pass", judgeMemory("service", 1, metric, memorySamples(at, 10*time.Minute, 0), at, time.Hour, false).Outcome)
		require.Equal(t, "insufficient-data", judgeMemory("service", 1, metric, memorySamples(at, 10*time.Minute-time.Second, 1<<20), at, time.Hour, false).Outcome)
	}
}

func TestSoakEventCoveragePolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		excluded []int
		fail     bool
		want     string
	}{
		{"one among nineteen passes", []int{0}, false, "pass"},
		{"ten percent inconclusive", []int{0, 1}, false, "inconclusive"},
		{"any failed event", nil, true, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixtureEvents(20, 30*time.Second)
			for _, index := range tc.excluded {
				r.Processes[0].Samples[31+index*30].IntervalLateness.P99 = 41 * time.Millisecond
			}
			if tc.fail {
				r.Processes[0].Calls[0].Call.Moves[0].Tracks[0].Gap = 151 * time.Millisecond
			}
			r.Judge()
			v := judgeSoak(c, r)
			require.Equal(t, tc.want, v.Outcome)
			require.Equal(t, 20-len(tc.excluded), v.ConclusiveEvents)
			for _, index := range tc.excluded {
				require.Equal(t, "inconclusive", v.Events[index].Outcome)
				require.NotEmpty(t, v.Events[index].Causes)
			}
		})
	}
	c, r := soakFixtureEvents(19, 30*time.Second)
	r.Processes[0].Samples[31].IntervalLateness.P99 = 41 * time.Millisecond
	r.Judge()
	boundary := judgeSoak(c, r)
	require.Equal(t, "inconclusive", boundary.Outcome, "18/19 is below the exact 95% threshold")
	require.InDelta(t, 100.0*18/19, boundary.ConclusiveEventPercent, .000001)
	c, r = soakFixtureEvents(20, 30*time.Second)
	// Keep only one scheduled kill, then exclude it. Aggregate coverage is 95%,
	// but that cannot establish kill recovery.
	for i := range r.Events {
		if r.Events[i].Event.Kind == "kill" && i != 2 {
			r.Events[i].Event.Kind = "move"
			c.Events[i].Kind = "move"
			r.Events[i].Event.Worker = 1
			c.Events[i].Worker = 1
			r.Processes[0].Calls[0].Call.Moves[i].Kind = "move"
			r.Processes[0].Calls[0].Call.Moves[i].From = 0
			r.Processes[0].Calls[0].Call.Moves[i].To = 1
		}
	}
	r.Telemetry.Processes[1].Last["relais_control_takeovers_total"] = 1
	r.Processes[0].Samples[91].IntervalLateness.P99 = 41 * time.Millisecond
	r.Judge()
	v := judgeSoak(c, r)
	require.Equal(t, 95.0, v.ConclusiveEventPercent)
	require.Equal(t, "inconclusive", v.Outcome)
	require.Contains(t, v.Causes, "no conclusive kill pass")
}

func TestSoakAcceptanceRequiresFullScaleAndMemory(t *testing.T) {
	c, r := soakFixture()
	v := judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome)
	require.False(t, v.Acceptance)
	require.Contains(t, v.AcceptanceReasons, "requires at least 50 concurrent calls for at least one hour")
	// Scale is configured separately from the verdict fixture's call inventory.
	// All observed long-lived roles have conclusive memory in this fixture.
	c.Callers, c.Duration = 50, time.Hour
	v = judgeSoak(c, r)
	require.True(t, v.FullScale)
	require.True(t, v.Acceptance)
	v.Outcome = "fail"
	v.judgeAcceptance()
	require.False(t, v.Acceptance)
	require.Contains(t, v.AcceptanceReasons, "run outcome is fail")
	r.Telemetry.Processes[0].Memory = memorySamples(r.StartedAt, 3*time.Minute, 0)
	v = judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome)
	require.False(t, v.Acceptance)
	require.Contains(t, v.AcceptanceReasons, "relay pid=0 rss: insufficient-data")
	// A worker observed for ten minutes still requires memory proof even if its
	// memory sampler returned only a short tail.
	r.Telemetry.Processes[3].ObservedSeconds = 600
	r.Telemetry.Processes[3].Memory = memorySamples(r.StartedAt, 3*time.Minute, 0)
	v = judgeSoak(c, r)
	require.False(t, v.Acceptance)
	require.Contains(t, v.AcceptanceReasons, "worker-0 pid=46 rss: insufficient-data")
	// Post-judgment execution errors also revoke acceptance.
	v.Outcome = "inconclusive"
	v.judgeAcceptance()
	require.False(t, v.Acceptance)
	require.Contains(t, v.AcceptanceReasons, "run outcome is inconclusive")
}

func TestSoakShortRunCanPassWithoutMemoryEvidence(t *testing.T) {
	c, r := soakFixture()
	for i := range r.Telemetry.Processes {
		r.Telemetry.Processes[i].Memory = nil
	}
	for i := range r.Processes[0].Samples {
		r.Processes[0].Samples[i].RSSBytes, r.Processes[0].Samples[i].HeapInUseBytes = 0, 0
		r.Processes[0].Samples[i].PhysicalFootprintBytes = 0
		r.Processes[0].Samples[i].RuntimeMemory = nil
	}
	v := judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome)
	require.False(t, v.Acceptance)
	for _, m := range v.Memory {
		require.Equal(t, "insufficient-data", m.Outcome)
	}
}

func TestSoakEventCoverageDoesNotHideIncompleteGenerators(t *testing.T) {
	c, r := soakFixtureEvents(20, 30*time.Second)
	// One missing event record is local to event coverage, not a missing caller.
	r.Processes[0].Calls[0].Call.Moves = r.Processes[0].Calls[0].Call.Moves[1:]
	r.Judge()
	v := judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome)
	require.Equal(t, "inconclusive", v.Events[0].Outcome)
	require.NotEmpty(t, v.Events[0].Causes)
	// A genuinely incomplete generator remains an independent unknown.
	r.Processes[0].Complete = false
	r.Judge()
	require.Equal(t, "inconclusive", judgeSoak(c, r).Outcome)
}

func TestSoakAcceptanceCannotIgnoreAllWorkerMemory(t *testing.T) {
	c, r := soakFixture()
	c.Callers, c.Duration = 50, time.Hour
	for i := range r.Telemetry.Processes {
		if strings.HasPrefix(r.Telemetry.Processes[i].Name, "worker-") {
			r.Telemetry.Processes[i].Memory = memorySamples(r.StartedAt, 3*time.Minute, 180*float64(1<<20)/3600)
		}
	}
	v := judgeSoak(c, r)
	require.False(t, v.Acceptance, "short recycled workers cannot prove worker memory")
	require.Contains(t, v.AcceptanceReasons, "worker memory unproven")
}

func TestSoakConfirmedEventFailureSurvivesSaturation(t *testing.T) {
	c, r := soakFixture()
	r.Events[0].Failed = true
	r.Processes[0].Saturated = true
	r.Processes[0].Samples[31].IntervalLateness.P99 = 41 * time.Millisecond
	r.Judge()
	v := judgeSoak(c, r)
	require.True(t, v.Events[0].Saturated)
	require.Equal(t, "fail", v.Events[0].Outcome)
	require.Equal(t, "fail", v.Outcome)
}
