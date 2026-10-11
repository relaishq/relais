package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relais/internal/capacity"
	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/loadgen"
	"github.com/relais/internal/processrss"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestSoakSentinelGrowthFailsWhileRecycledWorkersStayShort(t *testing.T) {
	c, r := soakFixture()
	c.Callers, c.Duration = 50, time.Hour
	r.Duration = time.Hour
	for i := range r.Processes[0].Calls {
		r.Processes[0].Calls[i].Call.HungUpAt = time.Hour
	}
	for i := range r.Telemetry.Processes {
		p := &r.Telemetry.Processes[i]
		p.Memory = memorySamples(r.StartedAt, time.Hour, 0)
		p.ObservedSeconds = time.Hour.Seconds()
		if p.Name == r.SentinelWorker {
			p.Memory = memorySamples(r.StartedAt, time.Hour, float64(180<<20)/time.Hour.Seconds())
		}
	}
	r.Processes[0].Samples = nil
	for _, s := range memorySamples(r.StartedAt, time.Hour, 0) {
		r.Processes[0].Samples = append(r.Processes[0].Samples, loadgen.Stats{At: s.At, Interval: time.Second, RSSBytes: s.RSSBytes, HeapInUseBytes: s.HeapInUseBytes})
	}
	for i := 0; i < 3; i++ {
		r.Telemetry.Processes = append(r.Telemetry.Processes, capacity.Process{Name: "worker-1", PID: 100 + i, ObservedSeconds: 180, Memory: memorySamples(r.StartedAt.Add(time.Duration(i)*3*time.Minute), 3*time.Minute, float64(180<<20)/time.Hour.Seconds())})
	}
	r.Judge()
	v := judgeSoak(c, r)
	require.Equal(t, "fail", v.Outcome)
	require.False(t, v.Acceptance)
	require.Contains(t, v.AcceptanceReasons, "worker memory unproven")
	require.Len(t, v.WorkerTrends, 1)
	require.True(t, v.WorkerTrends[0].Informational)
	require.True(t, v.WorkerTrends[0].TrendAvailable)
	require.Len(t, v.WorkerTrends[0].Points, 3)
	for _, point := range v.WorkerTrends[0].Points {
		require.True(t, point.Available)
		require.Equal(t, 30*time.Second, point.SampleAt.Sub(point.StartedAt))
	}
}

func TestSoakDarwinTrendUsesFootprintInsteadOfResident(t *testing.T) {
	c, r := soakFixture()
	r.MemoryMetric = "physical-footprint"
	for i := range r.Telemetry.Processes {
		r.Telemetry.Processes[i].Memory = memorySamples(r.StartedAt, c.Duration, 0)
		for j := range r.Telemetry.Processes[i].Memory {
			r.Telemetry.Processes[i].Memory[j].RSSBytes += uint64(j) * 50 << 10
		}
	}
	for i := range r.Processes[0].Samples {
		r.Processes[0].Samples[i].PhysicalFootprintBytes = 100 << 20
		r.Processes[0].Samples[i].RSSBytes += uint64(i) * 50 << 10
	}
	require.Equal(t, "pass", judgeSoak(c, r).Outcome, "resident-size growth is informational on Darwin")
	for i := range r.Telemetry.Processes {
		if r.Telemetry.Processes[i].Name == r.SentinelWorker {
			for j := range r.Telemetry.Processes[i].Memory {
				r.Telemetry.Processes[i].Memory[j].PhysicalFootprintBytes += uint64(j) * 50 << 10
			}
		}
	}
	require.Equal(t, "fail", judgeSoak(c, r).Outcome, "physical footprint growth must fail")
}

func TestSoakWholeCallGapCannotHideBehindScheduledMaximum(t *testing.T) {
	for _, tc := range []struct {
		name         string
		saturatedPID int
		want         string
	}{{"uncontended", 0, "fail"}, {"own generator", 42, "inconclusive"}, {"other generator", 43, "fail"}} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := soakFixture()
			// The largest gap is a scheduled takeover; the smaller freeze is mid-run,
			// outside the 15 s arrival history and outside every scheduled event.
			r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGap: 2 * time.Second, MediaGapEndedAt: 152 * time.Second, MediaGapsRecorded: true, MediaGaps: []callharness.MediaGap{{EndedAt: 152 * time.Second, Duration: 2 * time.Second}, {EndedAt: 600 * time.Second, Duration: 500 * time.Millisecond}}, Arrivals: []time.Duration{1190 * time.Second, 1190*time.Second + 20*time.Millisecond}}}
			if tc.saturatedPID != 0 {
				sample := loadgen.Stats{At: r.StartedAt.Add(600 * time.Second), Interval: time.Second, IntervalLateness: loadgen.Lateness{P99: 21 * time.Millisecond}}
				if tc.saturatedPID == 42 {
					r.Processes[0].Samples[600] = sample
				} else {
					r.Processes = append(r.Processes, loadgen.ProcessReport{PID: 43, Complete: true, Samples: []loadgen.Stats{sample}})
				}
			}
			// Assert the production continuity seam without legacy aggregate gap rules
			// conflating a synthetic generator with a caller process.
			causes := soakContinuityCauses(r, r.Processes[0], r.Processes[0].Calls[0], judgeSaturation(r))
			require.Len(t, causes, 1)
			require.Equal(t, tc.want == "fail", causes[0].Failed)
			require.Contains(t, causes[0].Reason, "500ms")
			if tc.saturatedPID == 42 {
				r.Processes[0].Lateness.Max = 21 * time.Millisecond
			}
			require.Equal(t, tc.want, judgeSoak(c, r).Outcome)
		})
	}
	c, r := soakFixture()
	r.Processes[0].Calls[0].Call.Tracks = []callharness.TrackReport{{Kind: "audio", MediaGapsRecorded: true, MediaGapOverflow: 1}}
	require.Equal(t, "fail", judgeSoak(c, r).Outcome)
	r.Processes[0].Calls[0].Call.Tracks[0].MediaGapOverflow = 0
	r.Processes[0].Calls[0].Call.Tracks[0].MediaGapsRecorded = false
	require.Equal(t, "inconclusive", judgeSoak(c, r).Outcome, "old reports lack proof of whole-call continuity")
}

func TestSoakResolverProtectsSentinelAndAllowsMoveToIt(t *testing.T) {
	status := controlplane.Status{Workers: []controlplane.WorkerStatus{{Name: "0", Calls: 1}, {Name: "1", Calls: 2}, {Name: "2", Calls: 2}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(status)) }))
	defer server.Close()
	topo := &topology{Workers: map[int]*clusterprocess.Child{0: nil, 1: nil, 2: nil}}
	topo.External.SignalingURL = server.URL + "/calls"
	for _, kind := range []string{"drain", "kill"} {
		for offset := 0; offset < 9; offset++ {
			event, err := resolveSoakEvent(context.Background(), topo, loadgen.Event{Kind: kind, Worker: offset}, 8)
			require.NoError(t, err)
			require.NotEqual(t, 0, event.Worker)
		}
	}
	event, err := resolveSoakEvent(context.Background(), topo, loadgen.Event{Kind: "move"}, 8)
	require.NoError(t, err)
	require.Equal(t, 0, event.Worker)
	require.ErrorContains(t, topo.recycle(context.Background(), 0), "memory sentinel")
	status.Workers[1].Dead = true
	_, err = resolveSoakEvent(context.Background(), topo, loadgen.Event{Kind: "kill"}, 8)
	require.ErrorContains(t, err, "N+1")
}

func TestSoakMissingScanControlsIsInconclusive(t *testing.T) {
	for _, name := range []string{"session_epoch", "relay-fencing-hash"} {
		c, r := soakFixture()
		delete(r.Leaks.ScanControls, name)
		v := judgeSoak(c, r)
		require.Equal(t, "inconclusive", v.Outcome)
		require.Contains(t, v.Causes, "Redis scan positive control missing: "+name)
	}
	controls := soakScanControls([]string{"run:session_epoch", "run:relay:{relay:ZGVmYXVsdA}"}, "run:")
	require.True(t, controls["session_epoch"])
	require.True(t, controls["relay-fencing-hash"])
	for _, found := range soakScanControls([]string{"run:session_epoch", "run:relay:{relay:ZGVmYXVsdA}"}, "wrong:") {
		require.False(t, found)
	}
}

func TestSoakRecycleCompletesRealRejoin(t *testing.T) {
	if os.Getenv("RELAIS_TEST_SOAK_RECYCLE") != "1" {
		t.Skip("set RELAIS_TEST_SOAK_RECYCLE=1 after make build for owned real-process rejoin proof")
	}
	if err := processrss.Check(); err != nil {
		t.Skipf("current memory sampling unavailable: %v", err)
	}
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	topo, err := startTopology(ctx, manager, bin, t.TempDir(), 3, "loopback")
	require.NoError(t, err)
	defer func() { require.NoError(t, topo.close()) }()
	topo.Soak = startSoakExecution(ctx, topo, time.Second)
	defer topo.Soak.finish(1, time.Second)
	oldPID := topo.Workers[1].PID()
	require.NoError(t, topo.recycle(ctx, 1))
	require.Len(t, topo.Rejoins, 1)
	rejoin := topo.Rejoins[0]
	require.Equal(t, oldPID, rejoin.OldPID)
	require.NotEqual(t, oldPID, rejoin.PID)
	require.True(t, rejoin.LastHeartbeat.After(rejoin.StartedAt))
	require.True(t, rejoin.CompletedAt.After(rejoin.StartedAt))
	var status controlplane.Status
	require.NoError(t, request(ctx, http.MethodGet, topo.External.SignalingURL[:len(topo.External.SignalingURL)-len("/calls")]+"/status", nil, &status))
	for _, w := range status.Workers {
		require.False(t, w.Dead)
		require.False(t, w.Draining)
		require.False(t, w.Recovering)
	}
}
