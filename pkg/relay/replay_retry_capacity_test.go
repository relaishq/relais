package relay

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplayCancelledLosslessQueueRemainsComplete(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			sys := startTestRelay(t, Config{Buffer: &BufferConfig{ReplayBatchPackets: 1, ReplayInterval: time.Second}, HoldTimeout: time.Hour})
			a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
			caller := newTestCaller(t)
			sys.connect(t, caller, a, sessionA)
			for _, seq := range []uint16{11, 12} {
				packet := bufferRTP(t, 42, seq)
				caller.send(t, packet, sys.relay.PublicAddr())
				a.expect(t, caller.addr(), packet)
			}
			inbound := map[uint32]uint64{42: 10}
			plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), inbound)
			require.NoError(t, err)
			require.True(t, plan.Complete)
			transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
			require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			if !partial {
				cancel()
			}
			go func() { _, err := sys.relay.ReplaySession(ctx, sessionA, b.addr()); done <- err }()
			if partial {
				b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
				cancel()
			}
			require.ErrorIs(t, <-done, context.Canceled)
			plan, err = sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), inbound)
			require.NoError(t, err)
			require.True(t, plan.Complete, "retry must still tell resume to skip frame-cache/PLI when nothing was lost")
			result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
			require.NoError(t, err)
			require.True(t, result.Complete)
			require.Equal(t, 2, result.Packets)
			require.Zero(t, result.Dropped)
			require.Zero(t, result.SendFailures)
			if !partial {
				b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
			}
			b.expect(t, caller.addr(), bufferRTP(t, 42, 12))
			b.expectNothing(t)
		})
	}
}

func TestReplayFullRingsLeaveDefaultRoomForGatedLiveInput(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{Window: time.Hour}, HoldTimeout: time.Hour})
	a := sys.worker(t, sessionA)
	caller := netip.MustParseAddrPort("127.0.0.1:19001")
	raw := make([]byte, 1200)
	raw[0], raw[1], raw[11] = 0x80, 111, 42
	packets := DefaultMaxBufferedBytes / packetCost(make([]byte, MaxHeaderLen+len(raw)))
	sys.relay.forwardMu.Lock()
	for i := range 16 {
		for seq := 1; seq <= packets; seq++ {
			raw[2], raw[3] = byte(seq>>8), byte(seq) //nolint:gosec // RTP sequence low bytes
			sys.relay.buffer.add(fmt.Sprintf("full-ring-%d", i), caller, raw, time.Now())
		}
	}
	sys.relay.forwardMu.Unlock()
	for i := range 16 {
		plan, err := sys.relay.BeginReplay(context.Background(), fmt.Sprintf("full-ring-%d", i), a.addr(), map[uint32]uint64{42: 0})
		require.NoError(t, err)
		require.True(t, plan.Complete)
		require.Equal(t, packets, plan.Packets)
	}
	// The production hold admission must accept another ring's worth of live
	// ciphertext for every concurrent takeover, while the original is pinned.
	sys.relay.forwardMu.Lock()
	for i := range 16 {
		h := sys.relay.holds[fmt.Sprintf("full-ring-%d", i)]
		for seq := 1; seq <= packets; seq++ {
			sys.relay.appendReplay(h, heldPacket{caller: caller, datagram: make([]byte, MaxHeaderLen+len(raw)), media: true, ssrc: 42, index: uint64(packets + seq)}) //nolint:gosec // positive bounded packet count
		}
	}
	sys.relay.forwardMu.Unlock()
	stats := sys.relay.Stats()
	t.Logf("FULL_RING_HOLD rings=16 history_packets=%d live_packets=%d held_bytes=%d drops=%d", 16*packets, 16*packets, stats.HeldBytes, stats.HoldDrops)
	require.Zero(t, stats.HoldDrops)
	require.Equal(t, 32*packets, stats.HeldPackets)
	require.Greater(t, stats.HeldBytes, 31<<20)
	require.Equal(t, 2<<20, sys.relay.cfg.MaxHeldBytes)
	require.Equal(t, 32<<20, sys.relay.cfg.MaxTotalHeldBytes)
}

func TestReplayFilteredPacketsDoNotSpendPacerTokens(t *testing.T) {
	r, h, pacer, _ := replayBatchFixture(32)
	// Even with no sender tokens left, a bounded batch of stale ciphertext
	// must be removed without consuming or waiting for sender capacity.
	pacer.packets, pacer.bytes = 0, 0
	pacer.at = time.Now().Add(time.Hour)
	r.drainReplayBatch(h, netip.AddrPort{}, pacer)
	require.Equal(t, 16, h.replayResult.Filtered)
	require.Len(t, h.queue, 16)
	require.Zero(t, pacer.packets)
	require.Zero(t, pacer.bytes)
}

func TestReplayCancelledContextCannotHideRelayShutdown(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	packet := bufferRTP(t, 42, 11)
	caller.send(t, packet, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), packet)
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	require.True(t, plan.Complete)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Close cancels the relay before acquiring forwardMu to discard its holds.
	// Pin that intermediate state: caller cancellation must not mask shutdown.
	sys.relay.cancel()
	result, err := sys.relay.ReplaySession(ctx, sessionA, b.addr())
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, result.Expired)
	require.False(t, result.Complete)
}
