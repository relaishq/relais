package callharness

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/stretchr/testify/require"
)

func TestCheckpointFullRelayHoldReleaseEchoed(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2, DisableFrameCache: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
	w := h.workers.list[0]
	var echoed atomic.Uint64
	require.NoError(t, workerprobe.SetAfterEcho(w.LocalAddr(), func(_ context.Context, id string, _ []byte) {
		if id == call.SessionID() {
			echoed.Add(1)
		}
	}))
	r := h.workers.relay.relay
	require.NoError(t, r.HoldSession(ctx, call.SessionID(), w.LocalAddr()))
	src, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	for range 256 {
		frame, _, err := src.next()
		require.NoError(t, err)
		require.NoError(t, call.audio.WriteSample(media.Sample{Data: frame, Duration: 2 * time.Millisecond}))
	}
	require.Eventually(t, func() bool { return r.Stats().HeldPackets == 256 }, time.Second, time.Millisecond)
	held, err := r.ReleaseSession(call.SessionID(), w.LocalAddr())
	require.NoError(t, err)
	require.Equal(t, 256, held)
	require.Eventually(t, func() bool { return echoed.Load() == 256 }, time.Second, time.Millisecond, "the full hold queue must traverse live encryption")
	// The caller's receiver also authenticates every echo in this burst.
	require.Eventually(t, func() bool {
		call.rec.mu.Lock()
		defer call.rec.mu.Unlock()
		for _, track := range call.rec.tracks {
			if track.kind == kindAudio {
				return len(track.arrivals) >= 261
			}
		}
		return false
	}, time.Second, time.Millisecond)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.Zero(t, report.DecryptionFailures.Total())
}

func TestCheckpointThreeSecondHoldSourceRateAndCrash(t *testing.T) {
	h, err := Start(Options{Relay: true, Workers: 2, DisableFrameCache: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	src, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	frame, _, err := src.next()
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		end := time.Now().Add(8 * time.Second)
		for time.Now().Before(end) {
			if err := call.audio.WriteSample(media.Sample{Data: frame, Duration: 2 * time.Millisecond}); err != nil {
				sent <- err
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				sent <- ctx.Err()
				return
			}
		}
		sent <- nil
	}()
	time.Sleep(2200 * time.Millisecond)
	w := h.workers.list[0]
	r := h.workers.relay.relay
	require.NoError(t, r.HoldSession(ctx, call.SessionID(), w.LocalAddr()))
	// Production's three-second timer, rather than an artificial meter clock,
	// releases the 256-packet backlog; sequence gaps from dropped queue overflow
	// remain visible in the subsequent source measurement.
	require.Eventually(t, func() bool { return r.Stats().HoldTimeouts == 1 }, 3500*time.Millisecond, 5*time.Millisecond)
	_, err = r.ReleaseSession(call.SessionID(), w.LocalAddr())
	require.ErrorIs(t, err, relay.ErrHoldExpired)
	// Wait beyond the old arrival window and another successful snapshot.
	// Reading immediately could see the pre-hold 500 pps checkpoint and let
	// the broken meter pass before its backlog peak became durable.
	time.Sleep(650 * time.Millisecond)
	info, err := w.SessionCheckpoint(call.SessionID())
	require.NoError(t, err)
	require.InDelta(t, 500, info.RTPPacketRate, 50)
	state, err := h.workers.relay.owners.GetState(ctx, call.SessionID())
	require.NoError(t, err)
	durable, err := mediaworker.SnapshotCheckpoint(state)
	require.NoError(t, err)
	require.InDelta(t, 500, durable.RTPPacketRate, 50, "the post-release rate must be durable before the later crash")
	require.NoError(t, h.Kill(0))
	require.Eventually(t, func() bool {
		status, err := h.Status(ctx)
		return err == nil && len(status.Takeovers) == 1 && !status.Takeovers[0].Lost
	}, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, <-sent)
	status, err := h.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "1", status.Calls[0].Owner)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.Zero(t, report.DecryptionFailures.Total())
	require.Positive(t, report.Moves[0].Tracks[0].PacketsAfter)
	t.Logf("HOLD_SOURCE_RATE rate=%.2f hold_timeout=3s policy=%s decryption_failures=%d", info.RTPPacketRate, status.Takeovers[0].CheckpointPolicy, report.DecryptionFailures.Total())
}
