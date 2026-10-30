package callharness

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayDrainLive(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var calls, other []*Call
		for len(calls) < 8 {
			require.Less(t, len(calls)+len(other), 16)
			c, e := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
			require.NoError(t, e)
			_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
			require.NoError(t, e)
			if a.Slot == "r1" {
				calls = append(calls, c)
			} else {
				other = append(other, c)
			}
		}
		for _, c := range other {
			_, e := c.Hangup(ctx)
			require.NoError(t, e)
		}
		sent := make(chan error, 8)
		for _, c := range calls {
			go func() { sent <- c.SendMedia(ctx, 5*time.Second) }()
		}
		time.Sleep(time.Second)
		baselines := map[*Call]*Report{}
		for _, c := range calls {
			baselines[c] = c.rec.report()
		}
		start := time.Now()
		drain, ok := any(p).(interface {
			DrainRelay(context.Context, string) error
		})
		require.True(t, ok, "one drain must change eight calls")
		require.NoError(t, drain.DrainRelay(ctx, "r1"))
		time.Sleep(time.Second)
		old := relays[0].Stats().WorkerPackets
		time.Sleep(100 * time.Millisecond)
		require.Equal(t, old, relays[0].Stats().WorkerPackets)
		for range calls {
			require.NoError(t, <-sent)
		}
		pauses := map[string][]time.Duration{}
		limits := map[string]time.Duration{}
		for _, c := range calls {
			_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
			require.NoError(t, e)
			require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, a)
			failures, e := w.SessionDecryptFailures(c.SessionID())
			require.NoError(t, e)
			require.Zero(t, failures)
			report, e := c.Hangup(ctx)
			require.NoError(t, e)
			require.Zero(t, report.DecryptionFailures.Total())
			for _, track := range report.Tracks {
				require.Zero(t, track.DuplicatePackets)
				var gap time.Duration
				for j := 1; j < len(track.Arrivals); j++ {
					if c.rec.start.Add(track.Arrivals[j]).After(start) {
						gap = max(gap, track.Arrivals[j]-track.Arrivals[j-1])
					}
				}
				pauses[track.Kind] = append(pauses[track.Kind], gap)
			}
			for _, base := range baselines[c].Tracks {
				limit := base.MediaGap + 50*time.Millisecond
				if limits[base.Kind] == 0 || limit < limits[base.Kind] {
					limits[base.Kind] = limit
				}
			}
			c.dialMu.Lock()
			connected := 0
			for _, state := range c.dtlsStates {
				if state.State == "connected" {
					connected++
				}
			}
			c.dialMu.Unlock()
			require.Equal(t, 1, connected)
		}
		for kind, gaps := range pauses {
			p95 := percentile(gaps, 95)
			t.Logf("RELAY_SAMPLES track=%s gaps=%v", kind, gaps)
			t.Logf("RELAY_DRAIN track=%s n=8 p95=%s baseline_limit=%s reconnects=0 auth=0 duplicates=0", kind, p95, limits[kind])
			require.LessOrEqual(t, p95, 150*time.Millisecond)
			require.LessOrEqual(t, p95, limits[kind])
		}
		c, e := h.Dial(ctx, CallOptions{RelayChange: true})
		require.NoError(t, e)
		_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
		require.NoError(t, e)
		require.Equal(t, "r2", a.Slot, "draining relay must not receive new calls")
		_, e = c.Hangup(ctx)
		require.NoError(t, e)
	})
}
