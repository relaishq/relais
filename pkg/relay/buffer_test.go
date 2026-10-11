package relay

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func bufferRTP(t *testing.T, ssrc uint32, seq uint16) []byte {
	t.Helper()
	raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: seq, PayloadType: 111}, Payload: []byte{1, 2, 3}}).Marshal()
	require.NoError(t, err)
	return raw
}

func TestBufferBoundsDropOldest(t *testing.T) {
	caller := netip.MustParseAddrPort("127.0.0.1:12345")
	packet := bufferRTP(t, 1, 1)
	size := len(packet) + MaxHeaderLen
	for _, total := range []bool{false, true} {
		t.Run(map[bool]string{false: "session", true: "total"}[total], func(t *testing.T) {
			cfg := BufferConfig{MaxSessionBytes: 2 * size}
			if total {
				cfg = BufferConfig{MaxTotalBytes: 2 * size}
			}
			b := newPacketBuffer(cfg)
			now := time.Now()
			b.add("a", caller, packet, now)
			second := "a"
			if total {
				second = "b"
			}
			b.add(second, caller, bufferRTP(t, 1, 2), now.Add(time.Millisecond))
			b.add(second, caller, bufferRTP(t, 1, 3), now.Add(2*time.Millisecond))
			require.Equal(t, 2*size, b.bytes)
			require.EqualValues(t, 1, b.drops)
			require.EqualValues(t, 2, b.packets.Front().Value.(*bufferedPacket).index)
			require.EqualValues(t, 3, b.packets.Back().Value.(*bufferedPacket).index)
		})
	}
	t.Run("oversized", func(t *testing.T) {
		b := newPacketBuffer(BufferConfig{MaxSessionBytes: size - 1})
		b.add("a", caller, packet, time.Now())
		require.Zero(t, b.bytes)
		require.EqualValues(t, 1, b.drops)
	})
}

func TestBufferTimeAndSSRCBounds(t *testing.T) {
	caller := netip.MustParseAddrPort("127.0.0.1:12345")
	b := newPacketBuffer(BufferConfig{Window: time.Second, MaxSSRCs: 1})
	now := time.Now()
	b.add("a", caller, bufferRTP(t, 1, 65535), now)
	b.add("a", caller, bufferRTP(t, 2, 1), now)
	require.EqualValues(t, 1, b.untracked)
	require.Len(t, b.sessions["a"].indexes, 1)
	b.expire(now.Add(time.Second))
	require.Zero(t, b.bytes)
	require.Zero(t, b.packets.Len())
	require.EqualValues(t, 1, b.expired)
	require.Zero(t, b.drops)
	b.add("a", caller, bufferRTP(t, 1, 0), now.Add(2*time.Second))
	require.EqualValues(t, 65536, b.packets.Front().Value.(*bufferedPacket).index, "ROC survives an empty ring")
	b.forget("a")
	require.Empty(t, b.sessions)
	require.Zero(t, b.bytes)
	require.EqualValues(t, 1, b.expired, "forget is cleanup, not time expiry")
}

func TestBufferSessionMetadataBound(t *testing.T) {
	b := newPacketBuffer(BufferConfig{})
	b.maxSessions = 1
	caller := netip.MustParseAddrPort("127.0.0.1:12345")
	b.add("a", caller, bufferRTP(t, 1, 1), time.Now())
	b.add("b", caller, bufferRTP(t, 2, 1), time.Now())
	require.Len(t, b.sessions, 1)
	require.EqualValues(t, 1, b.untracked)
	b.forget("a")
	b.add("b", caller, bufferRTP(t, 2, 2), time.Now())
	require.Contains(t, b.sessions, "b")
}

func TestBufferReplayGatesFiltersAndOrders(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{ReplayBatchPackets: 1, ReplayInterval: 10 * time.Millisecond}})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	// A reorder plus a repeated ciphertext must not advance the inbound
	// replay window or produce a duplicate outbound encryption on the target.
	for _, seq := range []uint16{10, 12, 11, 12} {
		raw := bufferRTP(t, 42, seq)
		caller.send(t, raw, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), raw)
	}
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	require.True(t, plan.Complete)
	caller.send(t, bufferRTP(t, 42, 13), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 4 }, time.Second, time.Millisecond)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	done := make(chan error, 1)
	go func() {
		result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
		if err == nil && result.Packets != 3 {
			err = context.Canceled
		}
		done <- err
	}()
	for _, seq := range []uint16{11, 12, 13} {
		b.expect(t, caller.addr(), bufferRTP(t, 42, seq))
	}
	require.NoError(t, <-done)
	caller.send(t, bufferRTP(t, 42, 14), sys.relay.PublicAddr())
	b.expect(t, caller.addr(), bufferRTP(t, 42, 14))
	a.expectNothing(t)
	require.Zero(t, sys.relay.Stats().Holds)
	require.Zero(t, sys.relay.Stats().HeldBytes)
	require.EqualValues(t, 3, sys.relay.Stats().ReplayPackets)
	require.EqualValues(t, 2, sys.relay.Stats().ReplayFiltered)
}

func TestBufferOnlyConfirmedCallers(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	request := bindingRequest(t, sessionA)
	caller.send(t, request, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), request)
	caller.send(t, bufferRTP(t, 42, 1), sys.relay.PublicAddr())
	a.expectNothing(t)
	require.Zero(t, sys.relay.Stats().BufferedPackets)
	sys.connect(t, caller, a, sessionA)
	caller.send(t, bufferRTP(t, 42, 2), sys.relay.PublicAddr())
	a.expect(t, caller.addr(), bufferRTP(t, 42, 2))
	require.Equal(t, 1, sys.relay.Stats().BufferedPackets)
	sys.relay.ForgetSession(sessionA)
	require.Zero(t, sys.relay.Stats().BufferedBytes)
	require.Zero(t, sys.relay.Stats().BufferedSessions)
}

func TestBufferRestoredROCAndIncompleteCoverage(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{MaxSessionBytes: 2 * (MaxHeaderLen + 15)}})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{100, 101, 102} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 7<<16 | 99})
	require.NoError(t, err)
	require.False(t, plan.Complete, "capacity lost a packet after the checkpoint")
	require.Equal(t, 2, plan.Packets)
	sys.relay.forwardMu.Lock()
	require.EqualValues(t, 7<<16|101, sys.relay.holds[sessionA].queue[0].index)
	sys.relay.forwardMu.Unlock()
	_, err = sys.relay.ReleaseSession(sessionA, a.addr())
	require.NoError(t, err)
}

func TestBufferMissingCheckpointSuccessorIsIncomplete(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	p := bufferRTP(t, 42, 12)
	caller.send(t, p, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), p)
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	require.False(t, plan.Complete)
	_, err = sys.relay.ReleaseSession(sessionA, a.addr())
	require.NoError(t, err)
}

func TestBufferGateBeforeSnapshotAndRouteFence(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	p := bufferRTP(t, 0, 10)
	caller.send(t, p, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), p)
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), nil)
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	// Even if reading the checkpoint is stalled, old output is fenced and
	// new caller input is held. Control packets must survive an SSRC-zero floor.
	a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)
	consent := bindingRequest(t, sessionA)
	caller.send(t, consent, sys.relay.PublicAddr())
	caller.send(t, bufferRTP(t, 0, 11), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 3 }, time.Second, time.Millisecond)
	_, err = sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.ErrorIs(t, err, ErrReplayNotReady)
	inbound := map[uint32]uint64{0: 10}
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), inbound)
	require.NoError(t, err)
	require.True(t, plan.Complete)
	require.Equal(t, 2, plan.Packets)
	// A lost gate reply can repeat the same preparation without allocating
	// another queue or replacing an already applied checkpoint.
	again, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), inbound)
	require.NoError(t, err)
	require.Equal(t, plan, again)
	_, err = sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	b.expect(t, caller.addr(), consent)
	b.expect(t, caller.addr(), bufferRTP(t, 0, 11))
	require.Zero(t, sys.relay.Stats().HeldBytes)
}
