package relay

import (
	"context"
	"github.com/relais/internal/privateapi"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The UDP send path checks the lease permission itself. A fenced send must
// not suppress that same input during a later recovery.
func TestReplayFailedSendDoesNotAdvanceFloor(t *testing.T) {
	var permitted atomic.Bool
	permitted.Store(true)
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour, ForwardingAllowed: permitted.Load})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	packet := bufferRTP(t, 42, 11)
	caller.send(t, packet, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), packet)
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	sys.relay.forwardMu.Lock()
	hold := sys.relay.holds[sessionA]
	sys.relay.forwardMu.Unlock()
	permitted.Store(false)
	result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	require.Equal(t, 1, result.SendFailures)
	require.Zero(t, result.Packets)
	require.False(t, result.Complete)
	require.EqualValues(t, 10, hold.delivered[42], "failed send cannot advance the checkpoint floor")
	require.Zero(t, sys.relay.buffer.sessions[sessionA].replayedThrough[42], "failed send cannot record delivery")
	b.expectNothing(t)
	permitted.Store(true)
	transferTestLease(t, sys.cfg.Owners, sessionA, a.addr())
	_, err = sys.relay.BeginReplay(context.Background(), sessionA, b.addr(), nil)
	require.NoError(t, err)
	require.NoError(t, sys.relay.MoveSession(sessionA, b.addr(), a.addr()))
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, b.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	require.True(t, plan.Complete)
	result, err = sys.relay.ReplaySession(context.Background(), sessionA, a.addr())
	require.NoError(t, err)
	require.Equal(t, 1, result.Packets)
	a.expect(t, caller.addr(), packet)
}

func TestReplayStopsWhenTargetMoves(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{ReplayBatchPackets: 1, ReplayInterval: 200 * time.Millisecond}, HoldTimeout: time.Hour})
	a, b, c := sys.worker(t, sessionA), sys.worker(t, sessionB), sys.worker(t, sessionC)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12, 13} {
		packet := bufferRTP(t, 42, seq)
		caller.send(t, packet, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), packet)
	}
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), nil)
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	_, err = sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	type outcome struct {
		result ReplayResult
		err    error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { result, err := sys.relay.ReplaySession(ctx, sessionA, b.addr()); done <- outcome{result, err} }()
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	transferTestLease(t, sys.cfg.Owners, sessionA, c.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, b.addr(), c.addr()))
	out := <-done
	require.Error(t, out.err, "a moved target must stop the old drain")
	require.Equal(t, 1, out.result.Packets)
	require.False(t, out.result.Complete, "a partial drain cannot advertise completion on the new target")
	require.Equal(t, 1, sys.relay.Stats().Holds, "the undrained queue remains gated")
	b.expectNothing(t)
	c.expectNothing(t)
	sys.relay.forwardMu.Lock()
	remaining := len(sys.relay.holds[sessionA].queue)
	sys.relay.forwardMu.Unlock()
	require.Equal(t, 2, remaining)
}

// Returning to an address is a new routing tenure, not confirmation of the
// old target's receipt. Repeat the cycle to exercise address reuse too.
func TestReplayFloorIsScopedToTargetTenure(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12} {
		packet := bufferRTP(t, 42, seq)
		caller.send(t, packet, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), packet)
	}
	source, target := a, b
	for range 3 {
		_, err := sys.relay.BeginReplay(context.Background(), sessionA, source.addr(), nil)
		require.NoError(t, err)
		transferTestLease(t, sys.cfg.Owners, sessionA, target.addr())
		require.NoError(t, sys.relay.MoveSession(sessionA, source.addr(), target.addr()))
		plan, err := sys.relay.BeginReplay(context.Background(), sessionA, source.addr(), map[uint32]uint64{42: 10})
		require.NoError(t, err)
		require.True(t, plan.Complete)
		require.Equal(t, 2, plan.Packets)
		result, err := sys.relay.ReplaySession(context.Background(), sessionA, target.addr())
		require.NoError(t, err)
		require.Equal(t, 2, result.Packets)
		target.expect(t, caller.addr(), bufferRTP(t, 42, 11))
		target.expect(t, caller.addr(), bufferRTP(t, 42, 12))
		source, target = target, source
	}
}

func TestReplayReceiptUsesTransferredEpochAtSameAddress(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12} {
		packet := bufferRTP(t, 42, seq)
		caller.send(t, packet, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), packet)
	}
	server := httptest.NewServer(sys.relay.PrivateHandler())
	defer server.Close()
	move := func(from, to netip.AddrPort, epoch uint64) {
		require.NoError(t, privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/move", RouteRequest{From: from, To: to, Epoch: epoch}, nil, RemoteErrors))
	}
	_, err := sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), nil)
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	move(a.addr(), b.addr(), 7)
	_, err = sys.relay.BeginReplay(context.Background(), sessionA, a.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	first, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	b.expect(t, caller.addr(), bufferRTP(t, 42, 12))
	move(b.addr(), b.addr(), 7)
	again, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	require.Equal(t, first, again, "same epoch confirms the saved receipt")
	b.expectNothing(t)
	move(b.addr(), b.addr(), 8)
	_, err = sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.ErrorIs(t, err, ErrHoldExpired, "fresh epoch cannot use the previous receiver's receipt")
	plan, err := sys.relay.BeginReplay(context.Background(), sessionA, b.addr(), map[uint32]uint64{42: 10})
	require.NoError(t, err)
	require.True(t, plan.Complete)
	result, err := sys.relay.ReplaySession(context.Background(), sessionA, b.addr())
	require.NoError(t, err)
	require.Equal(t, 2, result.Packets, "fresh receiver at the same address still receives uncheckpointed input")
	require.True(t, result.Complete)
	b.expect(t, caller.addr(), bufferRTP(t, 42, 11))
	b.expect(t, caller.addr(), bufferRTP(t, 42, 12))
}
