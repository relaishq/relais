package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/stretchr/testify/require"
)

// Decode the caller metric's JSON field so judgment cannot silently substitute
// heap objects or heap-in-use for the GC live set.
func setCallerLiveHeap(t *testing.T, s *loadgen.Stats, bytes uint64) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"heap_live_bytes": bytes})
	require.NoError(t, err)
	s.RuntimeMemory = &loadgen.RuntimeMemory{}
	require.NoError(t, json.Unmarshal(data, s.RuntimeMemory))
}

func TestSoakCallerMemoryUsesLiveHeap(t *testing.T) {
	for _, tc := range []struct {
		name             string
		callerRSSGrowth  bool
		callerLiveGrowth bool
		productRSSGrowth bool
		want             string
	}{
		{"flat live heap with growing diagnostics", true, false, false, "pass"},
		{"growing live heap with flat RSS", false, true, false, "fail"},
		{"product RSS growth", false, false, true, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			c.Callers, c.Duration = 50, time.Hour
			for i := range r.Processes[0].Samples {
				s := &r.Processes[0].Samples[i]
				growth := uint64(i) * 10 << 10
				live := uint64(4 << 20)
				if tc.callerLiveGrowth {
					live += growth
				}
				setCallerLiveHeap(t, s, live)
				if tc.callerRSSGrowth {
					s.RSSBytes += growth
					s.PhysicalFootprintBytes = s.RSSBytes
					s.HeapInUseBytes += growth
				}
			}
			if tc.productRSSGrowth {
				for i := range r.Telemetry.Processes[0].Memory {
					r.Telemetry.Processes[0].Memory[i].RSSBytes += uint64(i) * 10 << 10
				}
			}
			v := judgeSoak(c, r)
			require.Equal(t, tc.want, v.Outcome)
			require.Equal(t, tc.want == "pass", v.Acceptance)
			if tc.callerRSSGrowth {
				for _, metric := range []string{"rss", "physical-footprint", "heap-in-use"} {
					found := false
					for _, row := range v.Memory {
						if row.Name != "caller" || row.Metric != metric {
							continue
						}
						found = true
						data, err := json.Marshal(row)
						require.NoError(t, err)
						var wire map[string]any
						require.NoError(t, json.Unmarshal(data, &wire))
						require.Equal(t, true, wire["diagnostic"])
						require.Equal(t, "fail", row.Outcome, "diagnostic slope still records the exceeded threshold")
						require.InDelta(t, 10<<10, row.SlopeBytesPerSecond, .001)
					}
					require.True(t, found, "missing diagnostic %s", metric)
				}
			}
		})
	}
}

func TestSoakAcceptanceNeedsLiveHeapForEveryCaller(t *testing.T) {
	c, r := soakFixture()
	c.Callers, c.Duration = 50, time.Hour
	for i := range r.Processes[0].Samples {
		setCallerLiveHeap(t, &r.Processes[0].Samples[i], 4<<20)
	}
	v := judgeSoak(c, r)
	require.True(t, v.Acceptance)
	// A second caller PID has diagnostics but no live-heap evidence. The first
	// caller's passing row must not establish memory proof for both processes.
	v.Run.Processes = append(v.Run.Processes, loadgen.ProcessReport{PID: 99})
	v.judgeAcceptance()
	require.False(t, v.Acceptance)
	require.Contains(t, v.AcceptanceReasons, "caller pid=99 live heap memory unproven")
	for i := range r.Processes[0].Samples {
		r.Processes[0].Samples[i].RuntimeMemory = nil
	}
	v = judgeSoak(c, r)
	require.Equal(t, "pass", v.Outcome, "legacy samples do not invent growth")
	require.False(t, v.Acceptance, "legacy RSS-only reports cannot prove caller live heap")
}

func TestSoakCallerLiveHeapObservationWindow(t *testing.T) {
	at := time.Unix(1000, 0)
	for _, duration := range []time.Duration{9*time.Minute + 59*time.Second, 10 * time.Minute} {
		want := "insufficient-data"
		if duration == 10*time.Minute {
			want = "pass"
		}
		v := judgeMemory("caller", 42, "heap-live", memorySamples(at, duration, 0), at, time.Hour, true)
		require.Equal(t, want, v.Outcome)
	}
	// Ten minutes per PID still needs five minutes of actual post-warm-up
	// observations. A missing sample interval cannot stand in for that window.
	shortWindow := memorySamples(at, 10*time.Minute, 0)
	shortWindow = append(shortWindow[:2], shortWindow[360:]...)
	require.Equal(t, "insufficient-data", judgeMemory("caller", 42, "heap-live", shortWindow, at, time.Hour, true).Outcome)
	samples := memorySamples(at, time.Hour, 0)
	for i := range 300 {
		samples[i].HeapLiveBytes = 500 << 20
	}
	v := judgeMemory("caller", 42, "heap-live", samples, at, time.Hour, true)
	require.Equal(t, "pass", v.Outcome, "startup live heap is outside the fit")
	require.Equal(t, float64(16<<20), v.LimitBytes)
	// A replacement starts a new warm-up and cannot borrow the previous PID's
	// observations to satisfy its own minimum lifetime.
	samples = memorySamples(at.Add(30*time.Minute), 9*time.Minute, 0)
	require.Equal(t, "insufficient-data", judgeMemory("caller", 99, "heap-live", samples, at, time.Hour, true).Outcome)
}

func TestSoakProductMemoryStillGates(t *testing.T) {
	for _, name := range []string{"relay", "control", "store", "worker-0"} {
		for _, metric := range []string{"rss", "heap-in-use"} {
			if name == "store" && metric == "heap-in-use" {
				continue
			}
			t.Run(name+"/"+metric, func(t *testing.T) {
				c, r := soakFixture()
				for i := range r.Telemetry.Processes {
					p := &r.Telemetry.Processes[i]
					if p.Name != name {
						continue
					}
					for j := range p.Memory {
						s := &p.Memory[j]
						if metric == "rss" {
							s.RSSBytes += uint64(j) * 10 << 10
						} else {
							s.HeapInUseBytes += uint64(j) * 10 << 10
						}
					}
				}
				v := judgeSoak(c, r)
				require.Equal(t, "fail", v.Outcome)
				found := false
				for _, row := range v.Memory {
					if row.Name == name && row.Metric == metric {
						found = true
						require.False(t, row.Diagnostic)
						require.Equal(t, "fail", row.Outcome)
					}
				}
				require.True(t, found)
			})
		}
	}
}
