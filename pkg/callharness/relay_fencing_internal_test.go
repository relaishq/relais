package callharness

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayZombieFencing(t *testing.T) {
	disable := workerprobe.Enable()
	t.Cleanup(disable)
	h, err := Start(Options{Relay: true, Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	marker := []byte("RELAIS-ZOMBIE-MARKER")

	// Positive control: the captured sender actually makes authenticated media
	// on A's real private socket, and the caller can decrypt its marked payload.
	control, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	require.NoError(t, control.SendMedia(ctx, time.Second))
	zombie, err := workerprobe.Zombie(h.workers.list[0].LocalAddr(), control.SessionID())
	require.NoError(t, err)
	_, err = zombie(marker)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		control.rec.mu.Lock()
		defer control.rec.mu.Unlock()
		for _, track := range control.rec.tracks {
			if track.unmatchedPayloads > 0 {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond, "positive control: caller decrypted the marked payload")
	_, err = control.Hangup(ctx)
	require.NoError(t, err)

	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)
	// Observe the caller's socket before Pion decrypts or discards anything.
	var mu sync.Mutex
	received := map[string]bool{}
	call.socket.observer.mu.Lock()
	call.socket.observer.inspect = func(raw []byte) {
		mu.Lock()
		received[string(raw)] = true
		mu.Unlock()
	}
	call.socket.observer.mu.Unlock()
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	time.Sleep(time.Second)
	zombie, err = workerprobe.Zombie(h.workers.list[0].LocalAddr(), call.SessionID())
	require.NoError(t, err)
	require.NoError(t, call.Handover(HandoverOptions{To: 1}))
	packets := make([][]byte, 0, 100)
	for range 100 {
		raw, err := zombie(marker)
		require.NoError(t, err, "zombie actually transmitted on old worker leg")
		packets = append(packets, raw)
		time.Sleep(5 * time.Millisecond)
	}
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	mu.Lock()
	accepted := 0
	for _, raw := range packets {
		if received[string(raw)] {
			accepted++
		}
	}
	mu.Unlock()
	assert.Zero(t, accepted, "marked ciphertext at caller socket")
	assert.True(t, report.ConnectedThroughout())
	assert.Zero(t, report.DecryptionFailures.Total())
	assert.Zero(t, report.Renegotiations)
	assert.Zero(t, report.ICERestarts)
	for _, track := range report.Tracks {
		assert.Zero(t, track.DuplicatePackets, "no duplicate %s", track.Kind)
		assert.Zero(t, track.OutOfOrderPackets, "no out-of-place %s", track.Kind)
		assert.Zero(t, track.UnmatchedPayloads, "no marked audio decoded")
		if track.Video != nil {
			assert.Zero(t, track.Video.UnmatchedFrames, "no marked video decoded")
		}
	}
	t.Logf("FENCING_METRICS transmitted=%d caller_received=%d caller_decryption_failures=%d", len(packets), accepted, report.DecryptionFailures.Total())
}

func TestRelayFailedMoveRollsBack(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
	time.Sleep(time.Second)
	require.NoError(t, h.workers.list[1].Close())
	require.Error(t, call.Handover(HandoverOptions{To: 1}))
	status, err := h.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	assert.Equal(t, "0", status.Calls[0].Owner)
	assert.EqualValues(t, 3, status.Calls[0].Epoch)
	assert.Nil(t, status.Calls[0].LastMove)
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.Len(t, report.Moves, 1)
	assert.True(t, report.Moves[0].Result.RolledBack)
	assert.True(t, report.ConnectedThroughout())
	assert.Zero(t, report.DecryptionFailures.Total())
	for _, track := range report.Moves[0].Tracks {
		assert.Less(t, track.Gap, 100*time.Millisecond)
		assert.Positive(t, track.PacketsAfter)
	}
	t.Log(report.Summary())
}

// An invalid target fails before rerouting; A must still resume, even though
// the real relay will persistently reject the reverse notification as well.
func TestRelayUnregisteredTargetRollsBack(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
	time.Sleep(time.Second)
	h.workers.relay.relay.RemoveWorker(h.workers.list[1].LocalAddr())
	err = call.Handover(HandoverOptions{To: 1})
	require.ErrorContains(t, err, "rolled back")
	status, err := h.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Equal(t, "0", status.Calls[0].Owner)
	owner, ok := h.SessionOwner(call.SessionID())
	require.True(t, ok)
	require.Equal(t, 0, owner)
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Len(t, report.Moves, 1)
	require.True(t, report.Moves[0].Result.RolledBack)
	for _, track := range report.Moves[0].Tracks {
		require.Positive(t, track.PacketsAfter)
		require.Less(t, track.Gap, 100*time.Millisecond)
	}
}

// One target is unregistered, so half the drain rolls back while half moves.
// The HTTP reply must preserve both outcomes for caller gap observations.
func TestRelayPartialDrainRecordsMoves(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 3})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	calls := make([]*Call, 4)
	sent := make(chan error, len(calls))
	for i := range calls {
		calls[i], err = h.Dial(ctx, CallOptions{Video: true})
		require.NoError(t, err)
	}
	for _, call := range calls {
		go func() { sent <- call.SendMedia(ctx, 3*time.Second) }()
	}
	time.Sleep(time.Second)
	h.workers.relay.relay.RemoveWorker(h.workers.list[2].LocalAddr())
	require.ErrorContains(t, h.Drain(0), "rolled back")
	status, err := h.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 4)
	require.True(t, status.Workers[0].Draining)
	require.Equal(t, 2, status.Workers[0].Calls)
	require.Equal(t, 2, status.Workers[1].Calls)
	require.Zero(t, status.Workers[2].Calls)
	for range calls {
		require.NoError(t, <-sent)
	}
	success, rollback := 0, 0
	for _, call := range calls {
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Len(t, report.Moves, 1)
		move := report.Moves[0]
		if move.Error == "" {
			success++
		} else {
			require.True(t, move.Result.RolledBack)
			rollback++
		}
		require.True(t, report.ConnectedThroughout())
		require.Zero(t, report.DecryptionFailures.Total())
		for _, track := range move.Tracks {
			require.Zero(t, track.SkippedSequenceNumbers)
			require.Positive(t, track.PacketsAfter)
			require.Less(t, track.Gap, 100*time.Millisecond)
		}
	}
	require.Equal(t, 2, success)
	require.Equal(t, 2, rollback)
}

// Missing drain acknowledgement must abort before export, even though the
// harness's move context has no deadline. The same live call can move later.
func TestRelaySilentBarrierKeepsCallOnSource(t *testing.T) {
	// Preserve Pion's rejection reason when a rare failure reaches CI.
	t.Setenv("PION_LOG_INFO", "srtp")
	forWrapSessionStores(t, testRelaySilentBarrierKeepsCallOnSource)
}

func testRelaySilentBarrierKeepsCallOnSource(t *testing.T, store sessionstore.Store) {
	disable := workerprobe.Enable()
	t.Cleanup(disable)
	h, err := Start(Options{Relay: true, Workers: 2, SessionStore: store})
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	// Ensure a deliberately broken long wait cannot stall regression cleanup.
	t.Cleanup(func() { _ = h.workers.relay.relay.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	call, err := h.Dial(ctx, CallOptions{Video: true})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 6*time.Second) }()
	time.Sleep(time.Second)
	restore, err := workerprobe.IgnoreBarriers(h.workers.list[0].LocalAddr())
	require.NoError(t, err)
	t.Cleanup(restore)
	started := time.Now()
	failed := make(chan error, 1)
	go func() { failed <- call.Handover(HandoverOptions{To: 1}) }()
	select {
	case err := <-failed:
		require.ErrorContains(t, err, "barrier acknowledgement timed out")
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("planned move did not abort its missing barrier promptly")
	}
	duration := time.Since(started)
	status, err := h.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Calls, 1)
	require.Equal(t, "0", status.Calls[0].Owner)
	require.EqualValues(t, 1, status.Calls[0].Epoch, "no export or lease transfer")
	require.Zero(t, status.Calls[0].MoveCount)
	require.Nil(t, status.Calls[0].LastMove)
	restore()
	// Let the replay arrive before measuring the later move's own media gap.
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, call.Handover(HandoverOptions{To: 1}), "later move of the same call")
	status, err = h.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "1", status.Calls[0].Owner)
	require.EqualValues(t, 1, status.Calls[0].MoveCount)
	require.NoError(t, <-sent)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total(), "caller SRTP rejection categories: %+v", report.DecryptionFailures)
	require.Zero(t, report.Renegotiations)
	require.Zero(t, report.ICERestarts)
	require.Len(t, report.Moves, 2)
	require.NotEmpty(t, report.Moves[0].Error)
	require.Empty(t, report.Moves[1].Error)
	maxGap := time.Duration(0)
	for _, track := range report.Moves[0].Tracks {
		require.Positive(t, track.PacketsAfter)
		require.Less(t, track.Gap, 1500*time.Millisecond)
		maxGap = max(maxGap, track.Gap)
	}
	for _, track := range report.Moves[1].Tracks {
		require.Positive(t, track.PacketsAfter)
		require.Less(t, track.Gap, 100*time.Millisecond)
	}
	t.Logf("BARRIER_HARNESS_METRICS failed_move_duration=%s max_call_gap=%s later_move_succeeded=true decryption_failures=%d", duration, maxGap, report.DecryptionFailures.Total())
}
