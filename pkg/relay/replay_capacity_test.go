package relay

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplayLossResult(t *testing.T) {
	for _, failure := range []string{"ring-copy", "live-after-plan", "send", "expiry", "restart"} {
		t.Run(failure, func(t *testing.T) {
			size := packetCost(make([]byte, MaxHeaderLen+15))
			cfg := Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour}
			if failure == "ring-copy" {
				cfg.MaxHeldBytes = size
			}
			if failure == "live-after-plan" {
				cfg.MaxHeldBytes = 2 * size
			}
			sys := startTestRelay(t, cfg)
			a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
			caller := newTestCaller(t)
			sys.connect(t, caller, a, sessionA)
			for _, seq := range []uint16{11, 12} {
				p := bufferRTP(t, 42, seq)
				caller.send(t, p, sys.relay.PublicAddr())
				a.expect(t, caller.addr(), p)
			}
			plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
			require.NoError(t, err)
			transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
			require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
			switch failure {
			case "ring-copy":
				require.False(t, plan.Complete)
			case "live-after-plan":
				require.True(t, plan.Complete)
				caller.send(t, bufferRTP(t, 42, 13), sys.relay.PublicAddr())
				require.Eventually(t, func() bool { return sys.relay.Stats().HoldDrops == 1 }, time.Second, time.Millisecond)
			case "send":
				require.NoError(t, sys.relay.workers.SetWriteDeadline(time.Now().Add(-time.Second))) // real UDP write failure
				t.Cleanup(func() { require.NoError(t, sys.relay.workers.SetWriteDeadline(time.Time{})) })
			case "expiry":
				sys.relay.forwardMu.Lock()
				h := sys.relay.holds[sessionA]
				sys.relay.forwardMu.Unlock()
				sys.relay.expireHold(h)
			case "restart":
				// A fresh relay has neither the old cache nor its held replay.
				fresh := startTestRelay(t, Config{Buffer: &BufferConfig{}})
				fresh.relay.AddWorker(b.addr())
				result, err := fresh.relay.ReplaySession(context.Background(), sessionA, b.addr())
				require.ErrorIs(t, err, ErrHoldExpired)
				require.True(t, result.Expired)
				require.False(t, result.Complete)
				return
			}
			result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
			require.False(t, result.Complete)
			switch failure {
			case "expiry":
				require.ErrorIs(t, err, ErrHoldExpired)
				require.True(t, result.Expired)
			case "send":
				require.NoError(t, err)
				require.Equal(t, 2, result.SendFailures)
			default:
				require.NoError(t, err)
				require.Equal(t, 1, result.Dropped)
			}
		})
	}
}

func TestReplaySixteenRingsFitDefaultHoldBudget(t *testing.T) {
	// Pin sixteen realistic half-MiB rings concurrently. Test actual admission,
	// rather than merely comparing the configured budget numbers.
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{Window: time.Hour}})
	a := sys.worker(t, sessionA)
	now := time.Now()
	caller := netip.MustParseAddrPort("127.0.0.1:19001")
	raw := make([]byte, 1200)
	raw[0], raw[1], raw[11] = 0x80, 111, 42
	sys.relay.forwardMu.Lock()
	for i := range 16 {
		id := fmt.Sprintf("capacity-%d", i)
		for seq := range 440 {
			raw[2], raw[3] = byte(seq>>8), byte(seq)
			sys.relay.buffer.add(id, caller, raw, now)
		}
	}
	sys.relay.forwardMu.Unlock()
	for i := range 16 {
		plan, err := sys.relay.BeginReplay(context.Background(), fmt.Sprintf("capacity-%d", i), a.addr(), map[uint32]uint64{})
		require.NoError(t, err)
		require.True(t, plan.Complete)
		require.Equal(t, 440, plan.Packets)
	}
	require.Zero(t, sys.relay.Stats().HoldDrops)
	require.Greater(t, sys.relay.Stats().HeldBytes, 8<<20)
}

func TestReplayTargetPacingAcrossSessions(t *testing.T) {
	cfg := BufferConfig{ReplayBatchPackets: 2, ReplayInterval: 10 * time.Millisecond}
	sys := startTestRelay(t, Config{Buffer: &cfg, HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	target := sys.worker(t, "shared-target")
	ca, cb := newTestCaller(t), newTestCaller(t)
	sys.connect(t, ca, a, sessionA)
	sys.connect(t, cb, b, sessionB)
	for _, pair := range []struct {
		id     string
		caller *testCaller
		worker *testWorker
	}{{sessionA, ca, a}, {sessionB, cb, b}} {
		for seq := uint16(11); seq <= 30; seq++ {
			p := bufferRTP(t, 42, seq)
			pair.caller.send(t, p, sys.relay.PublicAddr())
			pair.worker.expect(t, pair.caller.addr(), p)
		}
		plan, err := sys.relay.BeginReplay(context.Background(), pair.id, pair.worker.addr(), map[uint32]uint64{42: 10})
		require.NoError(t, err)
		require.True(t, plan.Complete)
		transferTestLease(t, sys.cfg.Owners, pair.id, target.addr())
		require.NoError(t, sys.relay.MoveSession(pair.id, pair.worker.addr(), target.addr()))
	}
	started := time.Now()
	done := make(chan error, 2)
	for _, id := range []string{sessionA, sessionB} {
		go func() {
			result, err := sys.relay.ReplaySession(context.Background(), id, target.addr())
			if err == nil && result.Packets != 20 {
				err = fmt.Errorf("packets=%d", result.Packets)
			}
			done <- err
		}()
	}
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	require.GreaterOrEqual(t, time.Since(started), 180*time.Millisecond, "40 packets share 2 packets/10ms, rather than each session receiving that allowance")
	require.Zero(t, sys.relay.Stats().HoldSendFailures)
}

func TestReplayLateArrivalIsInsertedBeforeHigherIndex(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{ReplayBatchPackets: 1}})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 13} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	caller.send(t, bufferRTP(t, 42, 12), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 3 }, time.Second, time.Millisecond)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	require.True(t, result.Complete)
	for _, seq := range []uint16{11, 12, 13} {
		b.expect(t, caller.addr(), bufferRTP(t, 42, seq))
	}
}

func TestBufferEmptyFlowEvents(t *testing.T) {
	table := newFlowTable(flowLimits{maxFlows: 16, maxPending: 16, idleTimeout: time.Second, pendingTimeout: time.Second})
	table.watchEmpty("buffered")
	table.mu.Lock()
	table.sessions["buffered"] = map[netip.AddrPort]*callerFlows{}
	table.unindex(&callerFlows{}, "buffered")
	table.unindex(&callerFlows{}, "not-buffered")
	table.mu.Unlock()
	require.Len(t, table.emptyEvents, 1, "unbuffered flow removal cannot grow the event queue")
	require.Empty(t, table.takeEmptySessions(map[string]*sessionHold{"buffered": {}}), "a gate preserves index metadata")
	require.Equal(t, []string{"buffered"}, table.takeEmptySessions(nil))
	require.Empty(t, table.emptyWatch)
	require.Empty(t, table.emptyEvents)
	// A renewed flow cancels an obsolete empty event.
	table.watchEmpty("renewed")
	table.mu.Lock()
	table.unindex(&callerFlows{}, "renewed")
	table.sessions["renewed"] = map[netip.AddrPort]*callerFlows{{}: {}}
	table.mu.Unlock()
	require.Empty(t, table.takeEmptySessions(nil))
	require.True(t, table.emptyWatch["renewed"])
}

func TestReplayWindowUsesConfiguredEnvelope(t *testing.T) {
	cfg := BufferConfig{SnapshotMaxAge: time.Second, DeadAfter: 600 * time.Millisecond, CheckInterval: 100 * time.Millisecond, TransferAllowance: 400 * time.Millisecond}
	require.Equal(t, 2100*time.Millisecond, cfg.defaults().Window)
	cfg.Window = 3 * time.Second
	require.Equal(t, 3*time.Second, cfg.defaults().Window)
	r := Config{Buffer: &BufferConfig{MaxSessionBytes: 2 << 20, TakeoverParallelism: 32}}
	applyDefaults(&r)
	require.Equal(t, 128<<20, r.MaxTotalHeldBytes)
	require.Equal(t, 4<<20, r.MaxHeldBytes)
}

// Production batch seam: even a 32K queue must incur only one batch of lock
// work. All entries are filtered, isolating maintenance from network latency.
func replayBatchFixture(count int) (*Relay, *sessionHold, *replayPacer, []heldPacket) {
	cfg := BufferConfig{}.defaults()
	r := &Relay{buffer: newPacketBuffer(cfg)}
	queue := make([]heldPacket, count)
	for i := range queue {
		queue[i] = heldPacket{media: true, ssrc: 42, index: uint64(i), datagram: make([]byte, 32)}
	}
	h := &sessionHold{id: "batch", queue: queue, delivered: map[uint32]uint64{42: uint64(count)}, replay: true}
	return r, h, newReplayPacer(cfg, time.Now()), append([]heldPacket(nil), queue[:16]...)
}
func measureReplayBatch(count, repeats int) time.Duration {
	r, h, pacer, head := replayBatchFixture(count)
	queue := h.queue
	started := time.Now()
	for range repeats {
		h.queue = queue
		copy(queue, head)
		pacer.packets, pacer.bytes = 16, 16<<10
		r.drainReplayBatch(h, netip.AddrPort{}, pacer)
	}
	return time.Since(started) / time.Duration(repeats)
}
func TestReplayBatchWorkDoesNotScaleWithQueue(t *testing.T) {
	small, large := measureReplayBatch(32, 64), measureReplayBatch(32768, 64)
	t.Logf("REPLAY_BATCH small=%s large_32k=%s", small, large)
	require.Less(t, large, 10*small+100*time.Microsecond)
}
func BenchmarkReplayBatch32K(b *testing.B) {
	r, h, pacer, head := replayBatchFixture(32768)
	queue := h.queue
	b.ResetTimer()
	for range b.N {
		h.queue = queue
		copy(queue, head)
		pacer.packets, pacer.bytes = 16, 16<<10
		r.drainReplayBatch(h, netip.AddrPort{}, pacer)
	}
}

func TestReplayExpiryDuringDrainRetainsLossReceipt(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{ReplayBatchPackets: 1, ReplayInterval: 100 * time.Millisecond}, HoldTimeout: 50 * time.Millisecond})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12, 13} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	require.True(t, plan.Complete)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	first, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.ErrorIs(t, err, ErrHoldExpired)
	require.Equal(t, 1, first.Packets)
	require.Equal(t, 2, first.Dropped)
	require.True(t, first.Expired)
	require.False(t, first.Complete)
	second, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.ErrorIs(t, err, ErrHoldExpired)
	require.Equal(t, first, second)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	b.expectNothing(t)
	require.Zero(t, sys.relay.Stats().HeldBytes)
}

func TestReplayThroughPreventsRepeatOnNewCheckpoint(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	_, err = sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	b.expect(t, caller.addr(), bufferRTP(t, 42, 12))
	// A different recovery from a stale copy filters max(checkpoint,through).
	// It needs codec fallback because this target's copy predates that through.
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, b.addr(), map[uint32]uint64{42: 11})
	require.NoError(t, err)
	require.Zero(t, plan.Packets)
	require.False(t, plan.Complete)
	sys.relay.ForgetSession(sessionA)
	require.Zero(t, sys.relay.Stats().BufferedSessions)
	require.Empty(t, sys.relay.flows.emptyWatch)
}

func TestReplayTargetBucketRefillsByElapsedTime(t *testing.T) {
	cfg := BufferConfig{}.defaults()
	now := time.Now()
	bucket := newReplayPacer(cfg, now)
	for range 16 {
		require.True(t, bucket.take(now, 1000))
	}
	require.False(t, bucket.take(now, 1000))
	half := now.Add(time.Millisecond / 2)
	for range 8 {
		require.True(t, bucket.take(half, 1000))
	}
	require.False(t, bucket.take(half, 1000), "half an interval refills only half a burst")
	require.True(t, newReplayPacer(cfg, half).take(half, 1000), "another target has independent capacity")
	large := newReplayPacer(cfg, now)
	require.True(t, large.take(now, 32<<10))
	require.False(t, large.take(now, 32))
	require.True(t, large.take(now.Add(time.Millisecond), 32<<10))
}

func TestReplayColdRingAnchorsFirstLiveSSRCToCheckpoint(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	// A restarted relay may have restored consent but no history for this SSRC.
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 7<<16 | 10})
	require.NoError(t, err)
	require.False(t, plan.Complete)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	caller.send(t, bufferRTP(t, 42, 11), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, time.Second, time.Millisecond, "new SSRC input must not be filtered in local ROC zero")
	result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	require.Equal(t, 1, result.Packets)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	require.False(t, result.Complete)
}
