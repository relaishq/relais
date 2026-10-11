package relay

import (
	"context"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A dropped gated packet must preserve the ordinary codec recovery path.
func TestReplayGateDropIsIncomplete(t *testing.T) {
	size := MaxHeaderLen + len(bufferRTP(t, 42, 11)) + bufferPacketOverhead
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, MaxHeldBytes: 2 * size, HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), nil)
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	caller.send(t, bufferRTP(t, 42, 13), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HoldDrops == 1 }, time.Second, time.Millisecond)
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	t.Logf("REPLAY_GATE_DROP plan=%+v holdDrops=%d", plan, sys.relay.Stats().HoldDrops)
	require.False(t, plan.Complete, "a dropped gated packet requires codec recovery")
	_, err = sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	b.expect(t, caller.addr(), bufferRTP(t, 42, 12))
	caller.send(t, bufferRTP(t, 42, 14), sys.relay.PublicAddr())
	b.expect(t, caller.addr(), bufferRTP(t, 42, 14)) // 13 never delivered
}

// A lost reply must not re-send ciphertext the adopted target processed.
func TestReplayRetryDoesNotResend(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{10, 11, 12} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	inbound := map[uint32]uint64{42: 10}
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), nil)
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	_, err = sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), inbound)
	require.NoError(t, err)
	first, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	b.expect(t, caller.addr(), bufferRTP(t, 42, 12))
	// Live packet after release.
	caller.send(t, bufferRTP(t, 42, 13), sys.relay.PublicAddr())
	b.expect(t, caller.addr(), bufferRTP(t, 42, 13))
	// Control plane retry: transientResume path re-issues prepare + replay.
	// Tick 1 of the retry: takeoverLocked calls MoveSession before prepare.
	require.NoError(t, sys.relay.MoveSession(sessionA, b.addr(), b.addr()))
	again, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), inbound)
	require.NoError(t, err)
	second, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err, "retry after a lost reply must use the moved owner")
	require.Equal(t, first, second, "retry returns the original receipt")
	require.True(t, again.Complete)
	b.expectNothing(t)
	caller.send(t, bufferRTP(t, 42, 14), sys.relay.PublicAddr())
	b.expect(t, caller.addr(), bufferRTP(t, 42, 14))
}

// Measure retained heap with minimum-size RTP packets, after a GC.
func TestBufferTinyPacketHeapBudget(t *testing.T) {
	caller := netip.MustParseAddrPort("[2001:db8::1]:12345")
	pkt := make([]byte, 12)
	pkt[0], pkt[1] = 0x80, 111
	b := newPacketBuffer(BufferConfig{Window: time.Hour, MaxSessionBytes: 1 << 20})
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	now := time.Now()
	for i := range 40000 {
		pkt[2], pkt[3] = byte(i>>8), byte(i)
		pkt[8], pkt[9], pkt[10], pkt[11] = 0, 0, 0, 42
		b.add("a", caller, pkt, now)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("BUFFER_HEAP accounted=%d packets=%d heap_delta=%d ratio=%.1f", b.bytes, b.packets.Len(), after.HeapAlloc-before.HeapAlloc, float64(after.HeapAlloc-before.HeapAlloc)/float64(b.bytes))
	require.LessOrEqual(t, int64(after.HeapAlloc)-int64(before.HeapAlloc), int64(2<<20), "tiny RTP packets must fit a bounded heap budget")
	runtime.KeepAlive(b)
}

func TestReplayAcceptsNewerCheckpoint(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12, 13} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 12})
	require.NoError(t, err)
	require.True(t, plan.Complete)
	require.Equal(t, 1, plan.Packets)
	_, err = sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 11})
	require.ErrorIs(t, err, ErrHeld)
}

func TestReplayDefaultCapacityAndWindow(t *testing.T) {
	cfg := Config{Buffer: &BufferConfig{}}
	applyDefaults(&cfg)
	require.GreaterOrEqual(t, cfg.MaxTotalHeldBytes, 16*cfg.Buffer.defaults().MaxSessionBytes)
	require.GreaterOrEqual(t, cfg.Buffer.defaults().Window, 1500*time.Millisecond)
}
