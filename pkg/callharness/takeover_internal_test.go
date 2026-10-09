package callharness

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/mediaworker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayPausedWorkerTakeover(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)
	var mu sync.Mutex
	received := map[string]bool{}
	call.socket.observer.mu.Lock()
	call.socket.observer.inspect = func(raw []byte) { mu.Lock(); received[string(raw)] = true; mu.Unlock() }
	call.socket.observer.mu.Unlock()
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	time.Sleep(time.Second)
	old := h.workers.list[0]
	zombie, err := workerprobe.Zombie(old.LocalAddr(), call.SessionID())
	require.NoError(t, err)
	marker := []byte("RELAIS-PAUSED-WORKER-MARKER")
	h.callsMu.Lock()
	h.failures[strconv.Itoa(0)] = append(h.failures[strconv.Itoa(0)], time.Now())
	h.callsMu.Unlock()
	require.NoError(t, workerprobe.Pause(old.LocalAddr(), true))
	require.Eventually(t, func() bool {
		status, err := h.Status(ctx)
		return err == nil && len(status.Calls) == 1 && status.Calls[0].Owner == "1" && len(status.Takeovers) == 1 && !status.Workers[0].Recovering
	}, 2*time.Second, 5*time.Millisecond)
	_, err = h.Dial(ctx, CallOptions{Worker: 0})
	require.Error(t, err, "declared-dead worker receives no new call")
	ciphertext := make([][]byte, 0, 40)
	// The socket is still open, even while presumed dead. Its independent old
	// sender must be stopped at the relay, rather than merely by decryption.
	for range 20 {
		raw, err := zombie(marker)
		require.NoError(t, err)
		ciphertext = append(ciphertext, raw)
	}
	// Keep periodic snapshot fencing blocked. Rejoin itself must drop the
	// stale session before the control plane can select this address again.
	require.NoError(t, workerprobe.SetBeforeSnapshot(old.LocalAddr(), func(lifetime context.Context, id string) {
		if id == call.SessionID() {
			<-lifetime.Done()
		}
	}))
	require.NoError(t, workerprobe.Pause(old.LocalAddr(), false))
	for range 20 {
		raw, err := zombie(marker)
		require.NoError(t, err)
		ciphertext = append(ciphertext, raw)
		time.Sleep(5 * time.Millisecond)
	}
	require.Eventually(t, func() bool {
		status, err := h.Status(ctx)
		return err == nil && !status.Workers[0].Dead && status.Workers[0].Calls == 0
	}, time.Second, 5*time.Millisecond, "heartbeats rejoin with no leases")
	// Healthy means the stale session is already gone, rather than a
	// promise that a later snapshot/renewal will discard it.
	_, err = old.SessionDecryptFailures(call.SessionID())
	require.ErrorIs(t, err, mediaworker.ErrUnknownSession)
	require.NoError(t, workerprobe.SetBeforeSnapshot(old.LocalAddr(), nil))
	// A normal move straight back must succeed without old contexts or an
	// already-running-session rollback.
	require.NoError(t, call.Handover(HandoverOptions{To: 0}))
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	mu.Lock()
	accepted := 0
	for _, raw := range ciphertext {
		if received[string(raw)] {
			accepted++
		}
	}
	mu.Unlock()
	require.Zero(t, accepted)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.Renegotiations)
	require.Zero(t, report.ICERestarts)
	require.Len(t, report.Moves, 2)
	require.Equal(t, "takeover", report.Moves[0].Kind)
	require.Equal(t, "move", report.Moves[1].Kind)
	require.Equal(t, report.Moves[1].End, report.Consent.Since)
	for _, track := range report.Tracks {
		require.Zero(t, track.UnmatchedPayloads)
		require.Zero(t, track.OutOfOrderPackets)
	}
	t.Logf("PAUSED_TAKEOVER_METRICS old_packets_sent=%d caller_received=%d decryption_failures=%d old_session_fenced=true move_back_succeeded=true", len(ciphertext), accepted, report.DecryptionFailures.Total())
}

// With no healthy target, released ownership must also remove the relay's
// cached route. Otherwise the merely paused source could still reach the
// caller until a future consent check happened to invalidate that cache.
func TestRelayLostTakeoverFencesPausedWorker(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, time.Second))
	old := h.workers.list[0]
	zombie, err := workerprobe.Zombie(old.LocalAddr(), call.SessionID())
	require.NoError(t, err)
	var mu sync.Mutex
	received := map[string]bool{}
	call.socket.observer.mu.Lock()
	call.socket.observer.inspect = func(raw []byte) { mu.Lock(); received[string(raw)] = true; mu.Unlock() }
	call.socket.observer.mu.Unlock()
	require.NoError(t, workerprobe.Pause(old.LocalAddr(), true))
	require.Eventually(t, func() bool {
		status, err := h.Status(ctx)
		return err == nil && len(status.Calls) == 0 && status.LostCount == 1 && len(status.Takeovers) == 1 && status.Takeovers[0].Lost
	}, 2*time.Second, 5*time.Millisecond)
	packets := make([][]byte, 0, 20)
	for range 20 {
		raw, err := zombie([]byte("RELAIS-LOST-CALL-MARKER"))
		require.NoError(t, err)
		packets = append(packets, raw)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	accepted := 0
	for _, raw := range packets {
		if received[string(raw)] {
			accepted++
		}
	}
	mu.Unlock()
	require.Zero(t, accepted, "lost tenure has no cached path to the caller")
	require.NoError(t, workerprobe.Pause(old.LocalAddr(), false))
	t.Logf("LOST_TAKEOVER_METRICS old_packets_sent=%d caller_received=%d lost_count=1", len(packets), accepted)
}
