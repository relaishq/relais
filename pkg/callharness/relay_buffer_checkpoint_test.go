package callharness

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type bufferCheckpointClock struct {
	sessionstore.Store
	armed       atomic.Bool
	r           *relay.Relay
	forwarded   atomic.Uint64
	gatePackets atomic.Int64
}

func (s *bufferCheckpointClock) Checkpoint(ctx context.Context, id string, state []byte) (sessionstore.Checkpoint, error) {
	checkpoint, err := s.Store.Checkpoint(ctx, id, state)
	if err == nil && s.armed.Load() {
		stats := s.r.Stats()
		s.forwarded.Store(stats.CallerPackets)
		s.gatePackets.Store(int64(stats.HeldPackets))
		// Advance only the observed store clock. Preserve metadata validation
		// against the exact bytes, while making the counter envelope unsafe.
		checkpoint.Now = checkpoint.Now.Add(4 * time.Second)
		checkpoint.Age += 4 * time.Second
	}
	return checkpoint, err
}

func TestRelayBufferCheckpointDefinitiveLossDropsGateAndRing(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		clock := &bufferCheckpointClock{Store: store}
		var resumes atomic.Uint64
		sys := startBufferHarness(t, clock, true, func() { resumes.Add(1) })
		clock.r = sys.r
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{Video: true})
		require.NoError(t, err)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(ctx, 1500*time.Millisecond) }()
		require.Eventually(t, func() bool { return sys.r.Stats().BufferedPackets > 10 }, time.Second, time.Millisecond)
		clock.armed.Store(true)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		var status controlplane.Status
		require.Eventually(t, func() bool {
			var err error
			status, err = sys.h.Status(ctx)
			return err == nil && len(status.Takeovers) == 1
		}, 2*time.Second, 5*time.Millisecond)
		require.True(t, status.Takeovers[0].Lost)
		require.Equal(t, "definitive-loss", status.Takeovers[0].CheckpointPolicy)
		require.Greater(t, clock.gatePackets.Load(), int64(10), "terminal policy must exercise an allocated ring and gate")
		require.Zero(t, resumes.Load(), "unsafe checkpoint never reaches resume")
		stats := sys.r.Stats()
		require.Equal(t, clock.forwarded.Load(), stats.CallerPackets, "loss never flushes held ciphertext to the target")
		require.Zero(t, stats.Holds)
		require.Zero(t, stats.HeldPackets)
		require.Zero(t, stats.HeldBytes)
		require.Zero(t, stats.BufferedSessions)
		require.Zero(t, stats.BufferedPackets)
		require.Zero(t, stats.ReplayPackets)
		require.NoError(t, <-sent)
		_, err = call.Hangup(ctx)
		require.Error(t, err, "terminal loss removes the call")
		t.Logf("BUFFER_CHECKPOINT_LOSS store=%s gate_packets=%d holds=0 ring_packets=0 resume_requests=0 flushed_packets=0", t.Name(), clock.gatePackets.Load())
	})
}
