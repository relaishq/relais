package relay

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A lost export cannot use the exact-input release path. Recovery must replay
// pre-barrier input as well as held input before live media overtakes either.
func TestReplayPromotesNATDrainHoldAfterUncertainExport(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Second * 5})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for seq := uint16(10); seq <= 12; seq++ {
		packet := bufferRTP(t, 42, seq)
		caller.send(t, packet, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), packet)
	}
	beginHold(t, sys, a, sessionA)
	token := "00112233445566778899aabbccddeeff"
	rebound := netip.MustParseAddrPort("127.0.0.1:17009")
	require.NoError(t, sys.relay.ExpectWorker(token))
	require.True(t, sys.relay.observeLeg(token, rebound))
	require.NoError(t, sys.relay.PrepareRebind(a.addr(), rebound, token))
	caller.send(t, bufferRTP(t, 42, 13), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, receiveTimeout, time.Millisecond)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_, err := sys.relay.BeginReplay(ctx, sessionA, a.addr(), nil)
	require.NoError(t, err, "checkpoint recovery must adopt the interrupted planned gate")
	plan, err := sys.relay.BeginReplay(ctx, sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	require.Equal(t, 3, plan.Packets)
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	result, err := sys.relay.ReplaySession(ctx, sessionA, b.addr())
	require.NoError(t, err)
	require.Equal(t, 3, result.Packets)
	for seq := uint16(11); seq <= 13; seq++ {
		b.expect(t, caller.addr(), bufferRTP(t, 42, seq))
	}
	b.expectNothing(t)
	require.Zero(t, sys.relay.Stats().HeldPackets)
}

func TestPromotedNATDrainGetsFullReplayTimeout(t *testing.T) {
	const budget = 3 * time.Second
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: budget})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	beginHold(t, sys, a, sessionA)
	caller.send(t, bufferRTP(t, 42, 11), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, receiveTimeout, time.Millisecond)
	token := "00112233445566778899aabbccddeeff"
	rebound := netip.MustParseAddrPort("127.0.0.1:17009")
	require.NoError(t, sys.relay.ExpectWorker(token))
	require.True(t, sys.relay.observeLeg(token, rebound))
	require.NoError(t, sys.relay.PrepareRebind(a.addr(), rebound, token))
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())

	// Spend most of the planned hold's budget before checkpoint recovery.
	time.Sleep(5 * budget / 6)
	promoted := time.Now()
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), nil)
	require.NoError(t, err)
	time.Sleep(budget / 2) // beyond the original planned deadline
	require.EqualValues(t, 0, sys.relay.Stats().HoldTimeouts)
	require.Equal(t, 1, sys.relay.Stats().HeldPackets, "promotion must retain queued input")

	// Stop cannot retract a timer callback already waiting for forwardMu.
	// Deliver that old callback after promotion; it must not expire this gate.
	sys.relay.forwardMu.Lock()
	hold := sys.relay.holds[sessionA]
	sys.relay.forwardMu.Unlock()
	require.NotNil(t, hold)
	sys.relay.expireHold(hold)
	require.EqualValues(t, 0, sys.relay.Stats().HoldTimeouts)
	require.Equal(t, 1, sys.relay.Stats().HeldPackets)

	require.Eventually(t, func() bool { return sys.relay.Stats().HoldTimeouts == 1 }, budget+2*time.Second, 10*time.Millisecond)
	require.GreaterOrEqual(t, time.Since(promoted), budget, "replay gets a full fresh budget")
	require.Zero(t, sys.relay.Stats().Holds)
	require.Zero(t, sys.relay.Stats().HeldPackets)
	a.expectNothing(t)
	b.expectNothing(t) // abandoned replay still discards, rather than flushing
}
