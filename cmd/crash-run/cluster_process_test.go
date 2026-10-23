//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// Reuses the same production process trials and media gates as standalone.
// The opt-in requires built process binaries (make build; use -race for race runs).
func TestClusterProcessTopology(t *testing.T) {
	addrs := redistestcluster.Addresses(t)
	bin, err := filepath.Abs("../../bin")
	require.NoError(t, err)
	require.NoError(t, binaries(bin))
	manager := &clusterprocess.Manager{}
	ctx, cancel := manager.Context()
	defer cancel()
	dir := redistestcluster.Directory(t, "process")
	prefix := fmt.Sprintf("cluster-process:%d:", time.Now().UnixNano())
	env := processEnv(strings.Repeat("00", 32), strings.Join(addrs, ","), prefix)
	env = append(env, "RELAIS_REDIS_CLUSTER=1")
	t.Run("move-and-SIGKILL", func(t *testing.T) {
		runProcessProfileProbe(t, ctx, manager, bin, dir, env, nil)
	})
	t.Run("relay-standby", func(t *testing.T) {
		trialDir := filepath.Join(dir, "standby")
		require.NoError(t, os.Mkdir(trialDir, 0700))
		trialCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		result, err := relayStandbyTrial(trialCtx, manager, bin, trialDir, freshTrialEnv(env), 5*time.Second, 5*time.Second, false, false)
		require.NoError(t, err)
		require.True(t, result.Pass, "%+v", result)
		require.Positive(t, result.RoutesRestored)
	})
	t.Run("coordinated-failover", func(t *testing.T) {
		faultDir := filepath.Join(dir, "failover")
		require.NoError(t, os.Mkdir(faultDir, 0700))
		// Independent observer: the process clients must detect the changed primary
		// themselves. Never force their slot cache to refresh from the test.
		type promotion struct{ started, completed time.Time }
		var promotions []promotion
		fault := func(ctx context.Context, id, prefix string) error {
			observer, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addrs: addrs, Cluster: true, Prefix: prefix}, make([]byte, 32))
			if err != nil {
				return err
			}
			defer func() { _ = observer.Close() }()
			event := promotion{started: time.Now()}
			defer func() { event.completed = time.Now(); promotions = append(promotions, event) }()

			// Each promotion starts from current topology. A prior trial may already
			// have promoted this slot; test administration must not reuse stale state.
			admin := redis.NewClusterClient(storage.RedisClusterOptions(&redis.UniversalOptions{Addrs: addrs, MaxRetries: -1}))
			defer func() { _ = admin.Close() }()
			lease, err := observer.Get(ctx, id)
			if err != nil {
				return err
			}
			routes, err := observer.LoadRoutes(ctx, 100)
			if err != nil {
				return err
			}
			var before []sessionstore.Route
			for _, route := range routes {
				if route.SessionID == id {
					before = append(before, route)
				}
			}
			if len(before) == 0 {
				return fmt.Errorf("live call has no persisted route")
			}
			leaseKey := prefix + "lease:{sess:" + id + "}"
			controlKey := prefix + "control:{control}"
			callPrimary, err := admin.MasterForKey(ctx, leaseKey)
			if err != nil {
				return err
			}
			controlPrimary, err := admin.MasterForKey(ctx, controlKey)
			if err != nil {
				return err
			}
			t.Logf("CLUSTER_FAILOVER call_slot=%d control_slot=%d call_primary=%s control_primary=%s same_primary=%t", redistestcluster.Slot(leaseKey), redistestcluster.Slot(controlKey), callPrimary.Options().Addr, controlPrimary.Options().Addr, callPrimary.Options().Addr == controlPrimary.Options().Addr)
			if err := redistestcluster.Failover(ctx, admin, leaseKey); err != nil {
				return err
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				got, err := observer.Get(ctx, id)
				if err == nil {
					if got.Epoch != lease.Epoch || got.Worker != lease.Worker {
						return fmt.Errorf("failover changed live ownership")
					}
					break
				}
				if time.Now().After(deadline) {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(20 * time.Millisecond):
				}
			}
			restored, err := observer.LoadRoutes(ctx, 100)
			if err != nil {
				return err
			}
			for _, route := range before {
				intact := false
				for _, got := range restored {
					if got.SessionID == id && got.Caller == route.Caller && got.Generation == route.Generation {
						intact = true
					}
				}
				if !intact {
					return fmt.Errorf("failover lost live route")
				}
			}
			// Check the worker index on its independent slot after script-cache loss.
			indexed, err := observer.ListByWorker(ctx, lease.Worker)
			if err != nil {
				return err
			}
			for _, entry := range indexed {
				if entry.SessionID == id && entry.Epoch == lease.Epoch {
					return nil
				}
			}
			return fmt.Errorf("failover lost worker index for %s at %s", id, lease.Worker)
		}
		runProcessProfileProbe(t, ctx, manager, bin, faultDir, env, fault)
		require.Len(t, promotions, 2)
		for i, scenario := range []string{"move", "takeover"} {
			data, err := os.ReadFile(filepath.Join(faultDir, scenario, "caller-report.json"))
			require.NoError(t, err)
			var report callharness.Report
			require.NoError(t, json.Unmarshal(data, &report))
			require.NoError(t, clusterFailoverMedia(&report, promotions[i].started, promotions[i].completed))
		}

	})
}

// Apply the existing one-second persisted-route interruption gate to the
// promotion itself, not just to the later planned move / worker-kill event.
func clusterFailoverMedia(report *callharness.Report, started, completed time.Time) error {
	if len(report.Tracks) != 2 {
		return fmt.Errorf("failover needs audio and video observations")
	}
	at := started.Sub(report.StartedAt)
	end := completed.Add(time.Second).Sub(report.StartedAt)
	for _, track := range report.Tracks {
		gap, _, ok := relayRestartGap(track.Arrivals, at, end)
		resumed := false
		for _, arrival := range track.Arrivals {
			if arrival > at && arrival < at+time.Second {
				resumed = true
				break
			}
		}
		if !ok || !resumed || gap >= time.Second {
			return fmt.Errorf("%s media did not continue within 1s of store failover: gap=%s", track.Kind, gap)
		}
	}
	return nil
}

func TestClusterFailoverMediaGate(t *testing.T) {
	started := time.Unix(100, 0)
	report := &callharness.Report{StartedAt: started, Tracks: []callharness.TrackReport{{Kind: "audio"}, {Kind: "video"}}}
	for i := range report.Tracks {
		for step := range 31 {
			report.Tracks[i].Arrivals = append(report.Tracks[i].Arrivals, time.Duration(step)*100*time.Millisecond)
		}
	}
	require.NoError(t, clusterFailoverMedia(report, started.Add(time.Second), started.Add(1500*time.Millisecond)))
	report.Tracks[0].Arrivals = []time.Duration{900 * time.Millisecond, 2100 * time.Millisecond, 2200 * time.Millisecond}
	require.ErrorContains(t, clusterFailoverMedia(report, started.Add(time.Second), started.Add(1500*time.Millisecond)), "audio")
}
