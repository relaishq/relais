package callharness

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// This is an in-process host-loss fault, not evidence of SIGKILL/SIGSTOP or
// netns behavior. Both packet sockets stop and the slot lease stops renewing.
func TestUnplannedRelayLossEightCalls(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var calls, other []*Call
		for len(calls) < 8 {
			c, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
			require.NoError(t, err)
			_, a, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
			require.NoError(t, err)
			if a.Slot == "r1" {
				calls = append(calls, c)
			} else {
				other = append(other, c)
			}
		}
		for _, c := range other {
			_, err := c.Hangup(ctx)
			require.NoError(t, err)
		}
		t.Cleanup(func() {
			if t.Failed() {
				for _, c := range calls {
					t.Logf("BASELINE_DIAGNOSTIC id=%s %s", c.SessionID(), c.rec.report().Summary())
				}
			}
		})
		leases := store.(sessionstore.RelayLeases)
		identity := sessionstore.RelayProcess{Owner: "in-process-relay", PID: 42, Start: "start", Host: "host", Namespace: "namespace", Executable: "relay"}
		old, err := leases.ClaimRelay(ctx, "r1", identity, time.Minute)
		require.NoError(t, err)
		live, err := leases.ClaimRelay(ctx, "r2", identity, time.Minute)
		require.NoError(t, err)
		var lost atomic.Bool
		heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
		heartbeatDone := make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-heartbeatCtx.Done():
					return
				case <-tick.C:
					_ = p.Heartbeat(w.LocalAddr())
					live, _ = leases.RenewRelay(heartbeatCtx, live, time.Minute)
					if !lost.Load() {
						old, _ = leases.RenewRelay(heartbeatCtx, old, time.Minute)
					}
				}
			}
		}()
		defer func() { stopHeartbeat(); <-heartbeatDone }()
		require.NoError(t, p.Heartbeat(w.LocalAddr()))
		runCtx, stopRun := context.WithCancel(ctx)
		runDone := make(chan error, 1)
		go func() { runDone <- p.Run(runCtx) }()
		defer func() { stopRun(); require.NoError(t, <-runDone) }()
		mediaCtx, stopMedia := context.WithCancel(ctx)
		sent := make(chan error, 8)
		for _, c := range calls {
			go func() { sent <- c.SendMedia(mediaCtx, 12*time.Second) }()
		}
		defer func() {
			stopMedia()
			for range calls {
				<-sent
			}
		}()
		require.Eventually(t, func() bool {
			for _, c := range calls {
				r := c.rec.report()
				if len(r.Tracks) != 2 {
					return false
				}
				for _, track := range r.Tracks {
					if track.Packets < 15 {
						return false
					}
				}
			}
			return true
		}, 3*time.Second, 10*time.Millisecond)
		fault := time.Now()
		lost.Store(true)
		relays[0].Fence()
		var gaps []time.Duration
		for _, c := range calls {
			require.Eventually(t, func() bool {
				_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
				return e == nil && a.Slot == "r2" && a.Generation >= 1 && a.Pending == ""
			}, 5*time.Second, 10*time.Millisecond)
			observedCommit := time.Now()
			require.Eventually(t, func() bool {
				return decodedRelayRecovery(c, fault, observedCommit) > 0
			}, time.Second, 10*time.Millisecond)
			gap := decodedRelayRecovery(c, fault, observedCommit)
			gaps = append(gaps, gap)
			require.LessOrEqual(t, gap, 5*time.Second)
			failures, e := w.SessionDecryptFailures(c.SessionID())
			require.NoError(t, e)
			require.Zero(t, failures)
			report := c.rec.report()
			require.Zero(t, report.DecryptionFailures.Total())
			for _, track := range report.Tracks {
				require.Zero(t, track.DuplicatePackets)
			}
		}
		t.Logf("UNPLANNED_RELAY row=2 fault=in-process-sockets-and-renewal metric=decoded-after-observed-commit-upper-bound n=8 p50=%s p95=%s max=%s auth=0 duplicates=0", percentile(gaps, 50), percentile(gaps, 95), percentile(gaps, 100))
	})
}

func decodedRelayRecovery(c *Call, fault, observedCommit time.Time) time.Duration {
	return c.RelayRecoveryGap(fault, observedCommit)
}
