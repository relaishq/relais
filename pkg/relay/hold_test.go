package relay

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/relais/internal/relayleg"
	"github.com/stretchr/testify/require"
)

// beginHold acknowledges the drain barrier as a worker read loop does.
func beginHold(t *testing.T, sys *testSystem, worker *testWorker, id string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- sys.relay.HoldSession(context.Background(), id, worker.addr()) }()
	datagram, ok := receive(t, worker.conn, receiveTimeout)
	require.True(t, ok)
	nonce, ack, valid := relayleg.ParseBarrier(datagram)
	require.True(t, valid)
	require.False(t, ack)
	_, err := worker.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
	require.NoError(t, err)
	require.NoError(t, <-done)
}

func TestHoldReleaseAndRollback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[rollback], func(t *testing.T) {
			sys := startTestRelay(t, Config{})
			a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
			caller := newTestCaller(t)
			sys.connect(t, caller, a, sessionA)
			// A's last pre-hold datagram precedes the barrier on its private socket.
			caller.send(t, media, sys.relay.PublicAddr())
			require.Eventually(t, func() bool { return sys.relay.Stats().CallerPackets >= 2 }, receiveTimeout, time.Millisecond)
			done := make(chan error, 1)
			go func() { done <- sys.relay.HoldSession(context.Background(), sessionA, a.addr()) }()
			a.expect(t, caller.addr(), media)
			barrier, ok := receive(t, a.conn, receiveTimeout)
			require.True(t, ok)
			nonce, ack, valid := relayleg.ParseBarrier(barrier)
			require.True(t, valid)
			require.False(t, ack)
			// Packets arriving while A drains, including consent and an opaque packet
			// whose contents resemble a binding request for B, all wait in order.
			opaque := bytes.Clone(bindingRequest(t, sessionB))
			opaque[0] = 0x40
			packets := [][]byte{{0x80, 1}, bindingRequest(t, sessionA), opaque, {0x80, 2}}
			for _, packet := range packets {
				caller.send(t, packet, sys.relay.PublicAddr())
			}
			require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == len(packets) }, receiveTimeout, time.Millisecond)
			a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
			caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
			_, err := a.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
			require.NoError(t, err)
			require.NoError(t, <-done)
			a.expectNothing(t)
			transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
			require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
			target, other := b, a
			if rollback {
				transferTestLease(t, sys.cfg.Owners, sessionA, a.addr())
				require.NoError(t, sys.relay.MoveSession(sessionA, b.addr(), a.addr()))
				target, other = a, b
			}
			count, err := sys.relay.ReleaseSession(sessionA, target.addr())
			require.NoError(t, err)
			require.Equal(t, len(packets), count)
			for _, packet := range packets {
				target.expect(t, caller.addr(), packet)
			}
			other.expectNothing(t)
			require.Zero(t, sys.relay.Stats().HeldPackets)
			require.Zero(t, sys.relay.Stats().HeldBytes)
			require.Zero(t, sys.relay.Stats().HoldDrops)
			require.Zero(t, sys.relay.Stats().PendingFlows, "opaque payload cannot admit another session")
		})
	}
}

func TestHoldBounds(t *testing.T) {
	size := MaxHeaderLen + len(media)
	for name, cfg := range map[string]Config{
		"packets":       {MaxHeldPackets: 2},
		"session bytes": {MaxHeldBytes: 2 * size},
		"total bytes":   {MaxTotalHeldBytes: 2 * size},
	} {
		t.Run(name, func(t *testing.T) {
			sys := startTestRelay(t, cfg)
			a := sys.worker(t, sessionA)
			caller := newTestCaller(t)
			sys.connect(t, caller, a, sessionA)
			beginHold(t, sys, a, sessionA)
			for range 3 {
				caller.send(t, media, sys.relay.PublicAddr())
			}
			require.Eventually(t, func() bool { return sys.relay.Stats().HoldDrops == 1 }, receiveTimeout, time.Millisecond)
			require.Equal(t, 2, sys.relay.Stats().HeldPackets)
			require.Equal(t, 2*size, sys.relay.Stats().HeldBytes)
			count, err := sys.relay.ReleaseSession(sessionA, a.addr())
			require.NoError(t, err)
			require.Equal(t, 2, count)
			a.expect(t, caller.addr(), media)
			a.expect(t, caller.addr(), media)
			a.expectNothing(t)
			require.Zero(t, sys.relay.Stats().HeldBytes)
		})
	}
	t.Run("concurrent sessions", func(t *testing.T) {
		sys := startTestRelay(t, Config{MaxHeldSessions: 1})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		beginHold(t, sys, a, sessionA)
		require.ErrorIs(t, sys.relay.HoldSession(context.Background(), sessionB, b.addr()), ErrHoldLimit)
		_, err := sys.relay.ReleaseSession(sessionA, a.addr())
		require.NoError(t, err)
	})
	t.Run("global bytes across sessions", func(t *testing.T) {
		sys := startTestRelay(t, Config{MaxTotalHeldBytes: size})
		a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
		ca, cb := newTestCaller(t), newTestCaller(t)
		sys.connect(t, ca, a, sessionA)
		sys.connect(t, cb, b, sessionB)
		beginHold(t, sys, a, sessionA)
		beginHold(t, sys, b, sessionB)
		ca.send(t, media, sys.relay.PublicAddr())
		require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, receiveTimeout, time.Millisecond)
		cb.send(t, media, sys.relay.PublicAddr())
		require.Eventually(t, func() bool { return sys.relay.Stats().HoldDrops == 1 }, receiveTimeout, time.Millisecond)
		require.Equal(t, size, sys.relay.Stats().HeldBytes)
		_, err := sys.relay.ReleaseSession(sessionA, a.addr())
		require.NoError(t, err)
		_, err = sys.relay.ReleaseSession(sessionB, b.addr())
		require.NoError(t, err)
		a.expect(t, ca.addr(), media)
		b.expectNothing(t)
	})
}

func TestHoldTimeoutReleasesCurrentRoute(t *testing.T) {
	sys := startTestRelay(t, Config{HoldTimeout: 150 * time.Millisecond})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	beginHold(t, sys, a, sessionA)
	caller.send(t, media, sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, receiveTimeout, time.Millisecond)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	b.expect(t, caller.addr(), media)
	stats := sys.relay.Stats()
	require.EqualValues(t, 1, stats.HoldTimeouts)
	require.Zero(t, stats.Holds)
	require.Zero(t, stats.HeldBytes)
	_, err := sys.relay.ReleaseSession(sessionA, b.addr())
	require.ErrorIs(t, err, ErrHoldExpired)
	a.expectNothing(t)
}

func TestHoldMissingAcknowledgementTimesOut(t *testing.T) {
	sys := startTestRelay(t, Config{BarrierTimeout: 150 * time.Millisecond})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	done := make(chan error, 1)
	go func() { done <- sys.relay.HoldSession(context.Background(), sessionA, a.addr()) }()
	datagram, ok := receive(t, a.conn, receiveTimeout)
	require.True(t, ok)
	nonce, _, valid := relayleg.ParseBarrier(datagram)
	require.True(t, valid)
	// An acknowledgement from another registered worker cannot drain A.
	b := sys.worker(t, sessionB)
	_, err := b.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
	require.NoError(t, err)
	caller.send(t, media, sys.relay.PublicAddr())
	require.ErrorIs(t, <-done, ErrBarrierTimeout)
	expectAfterBarriers(t, a, caller.addr(), media)
	require.EqualValues(t, 1, sys.relay.Stats().BarrierTimeouts)
}

func TestSessionIndexPrunesExpiredAndReplacedFlows(t *testing.T) {
	table := newFlowTable(flowLimits{stickinessWindow: time.Second, idleTimeout: 10 * time.Second, pendingTimeout: time.Second, maxFlows: 8, maxPending: 8})
	caller := newTestCaller(t).addr()
	worker := listenLoopback(t)
	addr := localAddr(worker)
	now := time.Now()
	requestA := bindingRequest(t, sessionA)
	_, txA, _ := parseBindingRequest(requestA)
	require.True(t, table.admit(caller, addr, sessionA, txA, now))
	allowed, promoted := table.answer(caller, addr, bindingSuccess(t, requestA), now)
	require.True(t, allowed)
	require.Equal(t, sessionA, promoted)
	requestB := bindingRequest(t, sessionB)
	_, txB, _ := parseBindingRequest(requestB)
	require.True(t, table.admit(caller, addr, sessionB, txB, now.Add(time.Second)))
	require.Len(t, table.sessions, 2)
	allowed, promoted = table.answer(caller, addr, bindingSuccess(t, requestB), now.Add(time.Second))
	require.True(t, allowed)
	require.Equal(t, sessionB, promoted)
	require.Len(t, table.sessions, 1, "replacing a route drops the old session index")
	table.sweep(now.Add(12 * time.Second))
	require.Empty(t, table.sessions)
	require.Empty(t, table.callers)
}

func TestHoldQueueExcludesUnconfirmedCallers(t *testing.T) {
	sys := startTestRelay(t, Config{MaxHeldPackets: 2})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller, stranger, other := newTestCaller(t), newTestCaller(t), newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	sys.connect(t, other, b, sessionB)
	beginHold(t, sys, a, sessionA)
	// Neither an unknown address nor a caller authenticated for another
	// session may spend this session's held-packet budget.
	for _, intruder := range []*testCaller{stranger, other} {
		for range 3 {
			before := sys.relay.Stats()
			intruder.send(t, bindingRequest(t, sessionA), sys.relay.PublicAddr())
			require.Eventually(t, func() bool {
				after := sys.relay.Stats()
				if intruder == other {
					return after.FlowsRejected > before.FlowsRejected
				}
				return after.STUNRouted > before.STUNRouted
			}, receiveTimeout, time.Millisecond)
		}
	}
	a.expectNothing(t)
	require.Zero(t, sys.relay.Stats().HeldPackets)
	require.Zero(t, sys.relay.Stats().HoldDrops)
	packets := [][]byte{media, bindingRequest(t, sessionA)}
	for _, packet := range packets {
		caller.send(t, packet, sys.relay.PublicAddr())
	}
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == len(packets) }, receiveTimeout, time.Millisecond)
	count, err := sys.relay.ReleaseSession(sessionA, a.addr())
	require.NoError(t, err)
	require.Equal(t, len(packets), count)
	for _, packet := range packets {
		a.expect(t, caller.addr(), packet)
	}
	require.Zero(t, sys.relay.Stats().HoldDrops, "unrelated checks cannot displace the real caller")
}

func TestDefaultBarrierDeadlineReleasesSilentSource(t *testing.T) {
	sys := startTestRelay(t, Config{})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- sys.relay.HoldSession(context.Background(), sessionA, a.addr()) }()
	barrier, ok := receive(t, a.conn, receiveTimeout)
	require.True(t, ok)
	_, ack, valid := relayleg.ParseBarrier(barrier)
	require.True(t, valid)
	require.False(t, ack)
	// A reads but never acknowledges the barrier. Its confirmed caller's
	// media and consent must return to A in order when the short wait ends.
	packets := [][]byte{{0x80, 1}, bindingRequest(t, sessionA), {0x80, 2}}
	for _, packet := range packets {
		caller.send(t, packet, sys.relay.PublicAddr())
	}
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == len(packets) }, time.Second/2, time.Millisecond)
	select {
	case err := <-done:
		require.ErrorContains(t, err, "barrier acknowledgement timed out")
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("silent source exceeded the short barrier deadline")
	}
	duration := time.Since(started)
	require.Less(t, duration, 1500*time.Millisecond)
	for _, packet := range packets {
		expectAfterBarriers(t, a, caller.addr(), packet)
	}
	require.Zero(t, sys.relay.Stats().HeldPackets)
	require.Zero(t, sys.relay.Stats().HeldBytes)
	require.EqualValues(t, 1, sys.relay.Stats().BarrierTimeouts)
	require.Zero(t, sys.relay.Stats().HoldTimeouts, "long backstop did not begin")
	t.Logf("BARRIER_RELAY_METRICS duration=%s released_packets=%d", duration, len(packets))
}

func TestHoldBackstopStartsAfterAcknowledgement(t *testing.T) {
	sys := startTestRelay(t, Config{HoldTimeout: 100 * time.Millisecond})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	done := make(chan error, 1)
	go func() { done <- sys.relay.HoldSession(context.Background(), sessionA, a.addr()) }()
	barrier, ok := receive(t, a.conn, receiveTimeout)
	require.True(t, ok)
	nonce, _, valid := relayleg.ParseBarrier(barrier)
	require.True(t, valid)
	caller.send(t, media, sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, time.Second/2, time.Millisecond)
	// Waiting 200 ms exceeds the configured 100 ms coordination backstop,
	// but the source has not yet acknowledged: only the barrier clock runs.
	// Receive and discard retransmissions for 200 ms; no media may escape.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		datagram, ok := receive(t, a.conn, time.Until(deadline))
		if !ok {
			break
		}
		retry, ack, valid := relayleg.ParseBarrier(datagram)
		require.True(t, valid)
		require.False(t, ack)
		require.Equal(t, nonce, retry)
	}
	require.Equal(t, 1, sys.relay.Stats().Holds)
	require.Zero(t, sys.relay.Stats().HoldTimeouts)
	_, err := a.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
	require.NoError(t, err)
	require.NoError(t, <-done)
	// With no explicit release, the post-acknowledgement backstop now fires.
	expectAfterBarriers(t, a, caller.addr(), media)
	require.EqualValues(t, 1, sys.relay.Stats().HoldTimeouts)
	require.Zero(t, sys.relay.Stats().BarrierTimeouts)
}

// Discard only valid unacknowledged barrier retries, preserving exact media
// and caller assertions after a deliberately silent drain phase.
func expectAfterBarriers(t *testing.T, w *testWorker, caller netip.AddrPort, want []byte) {
	t.Helper()
	deadline := time.Now().Add(receiveTimeout)
	for {
		datagram, ok := receive(t, w.conn, time.Until(deadline))
		require.True(t, ok)
		if _, ack, valid := relayleg.ParseBarrier(datagram); valid {
			require.False(t, ack)
			continue
		}
		gotCaller, got, err := ParseHeader(datagram)
		require.NoError(t, err)
		require.Equal(t, caller, gotCaller)
		require.Equal(t, want, got)
		return
	}
}

func TestBarrierResendsSameNonceUntilAcknowledged(t *testing.T) {
	sys := startTestRelay(t, Config{})
	a := sys.worker(t, sessionA)
	done := make(chan error, 1)
	go func() { done <- sys.relay.HoldSession(context.Background(), sessionA, a.addr()) }()
	initial, ok := receive(t, a.conn, receiveTimeout)
	require.True(t, ok)
	nonce, ack, valid := relayleg.ParseBarrier(initial)
	require.True(t, valid)
	require.False(t, ack)
	// Drop the first barrier, then drop the first ACK. Both losses require a
	// new datagram; no synthetic relay acknowledgement bypasses the UDP leg.
	for range 2 {
		retry, ok := receive(t, a.conn, 200*time.Millisecond)
		require.True(t, ok)
		got, isAck, valid := relayleg.ParseBarrier(retry)
		require.True(t, valid)
		require.False(t, isAck)
		require.Equal(t, nonce, got)
	}
	_, err := a.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
	require.NoError(t, err)
	require.NoError(t, <-done)
	a.expectNothing(t)
	_, err = sys.relay.ReleaseSession(sessionA, a.addr())
	require.NoError(t, err)
}

func TestDefaultHoldBackstopReleasesAbandonedMove(t *testing.T) {
	require.Equal(t, 3*time.Second, DefaultHoldTimeout)
	sys := startTestRelay(t, Config{})
	a := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	beginHold(t, sys, a, sessionA)
	started := time.Now()
	caller.send(t, media, sys.relay.PublicAddr())
	// Simulate a vanished control process after the barrier was acknowledged:
	// no explicit ReleaseSession occurs, and the queued media must escape.
	datagram, ok := receive(t, a.conn, 4*time.Second)
	require.True(t, ok)
	addr, payload, err := ParseHeader(datagram)
	require.NoError(t, err)
	require.Equal(t, caller.addr(), addr)
	require.Equal(t, media, payload)
	elapsed := time.Since(started)
	require.GreaterOrEqual(t, elapsed, DefaultHoldTimeout-100*time.Millisecond)
	require.Less(t, elapsed, 4*time.Second)
	require.EqualValues(t, 1, sys.relay.Stats().HoldTimeouts)
	require.Zero(t, sys.relay.Stats().Holds)
}
