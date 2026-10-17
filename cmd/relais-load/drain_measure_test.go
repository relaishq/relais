package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/workerdriver"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

// Opt-in bounded measurement, not a capacity or latency acceptance gate. Real
// relay/control/custom-worker processes use dedicated Redis and default drain
// parallelism. Each trial gets a fresh topology because a drain mark is sticky.
func TestDrainMoveMeasurements(t *testing.T) {
	if testing.Short() || os.Getenv("RELAIS_DRAIN_MEASURE") != "1" {
		t.Skip("opt-in loaded drain measurement")
	}
	trials := 20
	if v := os.Getenv("RELAIS_DRAIN_MEASURE_TRIALS"); v != "" {
		_, err := fmt.Sscan(v, &trials)
		require.NoError(t, err)
		require.GreaterOrEqual(t, trials, 1)
		require.LessOrEqual(t, trials, 20)
	}
	bin := t.TempDir()
	for _, role := range []struct{ name, path string }{{"relais-relay", "../../cmd/relais-relay"}, {"relais-control", "../../cmd/relais-control"}, {"delayed-loopback", "../../examples/delayed-loopback"}} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, role.name), role.path)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	t.Logf("MEASUREMENT host=%s/%s logical_cpus=%d driver_gomaxprocs=%d trials=%d", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0), trials)
	for _, count := range []int{1, 8, 32} {
		t.Run(fmt.Sprintf("sessions-%d", count), func(t *testing.T) {
			var adoptions, rounds, gaps []time.Duration
			for trial := range trials {
				t.Run(fmt.Sprintf("trial-%02d", trial+1), func(t *testing.T) {
					adopted, round, gap := measureDrainTrial(t, bin, count)
					adoptions = append(adoptions, adopted)
					rounds = append(rounds, round)
					gaps = append(gaps, gap)
					t.Logf("DRAIN_SAMPLE sessions=%d trial=%d adoption_ms=%.3f round_ms=%.3f playout_gap_ms=%.3f", count, trial+1, float64(adopted)/float64(time.Millisecond), float64(round)/float64(time.Millisecond), float64(gap)/float64(time.Millisecond))
				})
			}
			for _, series := range []struct {
				name   string
				values []time.Duration
			}{{"adoption", adoptions}, {"round", rounds}, {"playout_gap", gaps}} {
				slices.Sort(series.values)
				percentile := func(p int) float64 {
					return float64(series.values[(len(series.values)*p+99)/100-1]) / float64(time.Millisecond)
				}
				t.Logf("DRAIN_SUMMARY sessions=%d metric=%s n=%d p50_ms=%.3f p95_ms=%.3f max_ms=%.3f", count, series.name, len(series.values), percentile(50), percentile(95), percentile(100))
			}
		})
	}
}
func measureDrainTrial(t *testing.T, bin string, count int) (time.Duration, time.Duration, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	manager := new(clusterprocess.Manager)
	topology, err := startTopology(ctx, manager, bin, t.TempDir(), 2, "loopback", workerdriver.Options{Binary: filepath.Join(bin, "delayed-loopback"), Agent: "delayed-loopback/v1"})
	require.NoError(t, err)
	t.Cleanup(func() { manager.Stop(false) })
	h, err := callharness.Start(callharness.Options{External: &topology.External})
	require.NoError(t, err)
	defer func() { require.NoError(t, h.Close()) }()
	calls := make([]*callharness.Call, count)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() { var err error; calls[i], err = h.Dial(ctx, callharness.CallOptions{}); require.NoError(t, err) })
	}
	wg.Wait()
	for _, call := range calls {
		wg.Go(func() { require.NoError(t, call.SendMedia(ctx, 2100*time.Millisecond)) })
	}
	wg.Wait()
	// Every session has published a prefix and is pacing audio before drain.
	for _, call := range calls {
		require.Eventually(t, func() bool {
			var status mediaworker.WorkerStatus
			if request(ctx, "GET", topology.Launch[0].URL+"/status?agent-session="+call.SessionID(), nil, &status) != nil || status.AgentSession == nil {
				return false
			}
			var state struct {
				Phase  string
				Cursor uint64
			}
			return json.Unmarshal(status.AgentSession.App, &state) == nil && state.Phase == "playout" && state.Cursor >= 2
		}, 5*time.Second, 5*time.Millisecond)
	}
	var result controlplane.DrainResult
	start := time.Now()
	require.NoError(t, request(ctx, "POST", topology.External.SignalingURL[:len(topology.External.SignalingURL)-len("/calls")]+"/workers/0/drain", nil, &result))
	finished := time.Now()
	require.Empty(t, result.Error)
	require.Len(t, result.Moves, count)
	// Drain replies after adoption and relay release. Also read control
	// LastMove, recorded immediately after acknowledged target adoption. All
	// processes use this machine's clock; the full HTTP round is a separate bound.
	var status controlplane.Status
	require.NoError(t, request(ctx, "GET", topology.External.SignalingURL[:len(topology.External.SignalingURL)-len("/calls")]+"/status", nil, &status))
	require.Len(t, status.Calls, count)
	lastAdoption := start
	for _, call := range status.Calls {
		require.NotNil(t, call.LastMove)
		if call.LastMove.After(lastAdoption) {
			lastAdoption = *call.LastMove
		}
		require.Equal(t, "1", call.Owner)
		require.EqualValues(t, 1, call.MoveCount)
	}
	// Capture packets on both sides of the whole drain interval, before another
	// turn can add an intentional recording/processing pause to the sample.
	time.Sleep(350 * time.Millisecond)
	reports := make([]*callharness.Report, count)
	for i, call := range calls {
		wg.Go(func() { var err error; reports[i], err = call.Hangup(ctx); require.NoError(t, err) })
	}
	wg.Wait()
	var largest time.Duration
	for _, report := range reports {
		require.Zero(t, report.DecryptionFailures.Total())
		require.Zero(t, report.Renegotiations)
		require.Zero(t, report.ICERestarts)
		found := false
		for _, track := range report.Tracks {
			if track.Kind != "audio" {
				continue
			}
			var before, after time.Duration
			for _, arrival := range track.Arrivals {
				instant := report.StartedAt.Add(arrival)
				if !instant.After(start) {
					before = arrival
					continue
				}
				if after == 0 {
					after = arrival
				}
				if instant.After(start.Add(-100*time.Millisecond)) && instant.Before(finished.Add(250*time.Millisecond)) && before > 0 {
					largest = max(largest, arrival-before)
				}
				before = arrival
			}
			found = found || after > 0
		}
		require.True(t, found, "playout must continue across drain")
	}
	return lastAdoption.Sub(start), finished.Sub(start), largest
}
