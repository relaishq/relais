package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/relayleg"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func routeTenure(sys *testSystem, id string) uint64 {
	sys.relay.forwardMu.Lock()
	defer sys.relay.forwardMu.Unlock()
	return sys.relay.buffer.sessions[id].routeTenure
}

func storeLease(t *testing.T, sys *testSystem, id string) sessionstore.Lease {
	t.Helper()
	lease, err := sys.cfg.Owners.(sessionstore.Store).Get(context.Background(), id)
	require.NoError(t, err)
	return lease
}

// Characterization (ADR 0009, #443): control now sends the transferred lease
// epoch on a planned move instead of zero. The export drain, the planned
// release and the receiver tenure the move starts must not change.
func TestPlannedMoveAtTransferredEpochKeepsDrainAndReplay(t *testing.T) {
	for _, transferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("transferred=%v", transferred), func(t *testing.T) {
			sys := startTestRelay(t, Config{Buffer: &BufferConfig{}, HoldTimeout: time.Hour})
			a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
			caller := newTestCaller(t)
			sys.connect(t, caller, a, sessionA)
			for _, p := range seqs(42, 10, 13) {
				caller.send(t, p.raw(t), sys.relay.PublicAddr())
				a.expect(t, caller.addr(), p.raw(t))
			}
			beginHold(t, sys, a, sessionA)
			caller.send(t, bufferRTP(t, 42, 14), sys.relay.PublicAddr())
			require.Eventually(t, func() bool { return sys.relay.Stats().HeldPackets == 1 }, receiveTimeout, time.Millisecond)
			before := routeTenure(sys, sessionA)
			transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
			lease := storeLease(t, sys, sessionA)
			var epoch uint64
			if transferred {
				epoch = lease.Epoch
			}
			done := make(chan error, 1)
			go func() { done <- sys.relay.MoveSessionAfterExport(sessionA, a.addr(), b.addr(), epoch) }()
			datagram, ok := receive(t, a.conn, receiveTimeout)
			require.True(t, ok)
			nonce, ack, valid := relayleg.ParseBarrier(datagram)
			require.True(t, valid)
			require.False(t, ack)
			_, err := a.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
			require.NoError(t, err)
			require.NoError(t, <-done)
			require.EqualValues(t, 1, sys.relay.Stats().OutputDrains, "the export drain runs at either epoch")
			require.Equal(t, before+1, routeTenure(sys, sessionA), "a planned move starts one receiver tenure")

			reply, err := sys.relay.ReleaseAfterExport(sessionA, b.addr(), map[uint32]ConsumedInput{42: {High: 12, Window: window(12, 10, 11, 12), Exact: true}})
			require.NoError(t, err)
			require.Equal(t, 1, reply.Recovered)
			require.True(t, reply.Complete)
			b.expect(t, caller.addr(), bufferRTP(t, 42, 13))
			b.expect(t, caller.addr(), bufferRTP(t, 42, 14))
			a.expectNothing(t)

			// A later confirmation of the same tenure (restart repair) keeps the
			// receiver tenure only when the planned move carried the epoch.
			require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, netip.AddrPort{}, b.addr(), lease.Epoch))
			want := before + 1
			if !transferred {
				want++
			}
			require.Equal(t, want, routeTenure(sys, sessionA))
		})
	}
}

// Characterization (#443): a repair with an empty source re-points every
// route of the session, whatever worker each route is at, at any epoch.
func TestRepairWithEmptySourceRepointsEveryRoute(t *testing.T) {
	for _, epoch := range []uint64{0, 9} {
		t.Run(fmt.Sprint(epoch), func(t *testing.T) {
			sys := startTestRelay(t, Config{})
			a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
			first, second := newTestCaller(t), newTestCaller(t)
			sys.connect(t, first, a, sessionA)
			sys.connect(t, second, a, sessionA)
			require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, netip.AddrPort{}, b.addr(), epoch))
			for _, caller := range []*testCaller{first, second} {
				caller.send(t, media, sys.relay.PublicAddr())
				b.expect(t, caller.addr(), media)
			}
			a.expectNothing(t)
		})
	}
}

// #443: moves are ordered by lease epoch. A stale control process that
// resumes after a newer one moved the call B->A must not point the caller
// back at B, through either move call, and must not rewrite the persisted
// route. Epoch zero (in-process callers without a store) stays unordered.
func TestStaleMoveIsRefusedAndLeavesRoute(t *testing.T) {
	store := sessionstore.NewMemory()
	sys := startTestRelay(t, Config{Owners: store, Routes: store})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	transferTestLease(t, store, sessionA, b.addr())
	e2 := storeLease(t, sys, sessionA).Epoch
	require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, a.addr(), b.addr(), e2))
	transferTestLease(t, store, sessionA, a.addr())
	e3 := storeLease(t, sys, sessionA).Epoch
	require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, b.addr(), a.addr(), e3))
	persistedRoute(t, store, caller.addr(), func(record sessionstore.Route) bool { return record.Generation == e3 })
	writes := sys.relay.Stats().RouteWrites

	require.ErrorIs(t, sys.relay.MoveSessionAtEpoch(sessionA, a.addr(), b.addr(), e2), ErrStaleMove)
	require.ErrorIs(t, sys.relay.MoveSessionAtEpoch(sessionA, netip.AddrPort{}, b.addr(), e2), ErrStaleMove, "a stale repair is refused too")
	require.ErrorIs(t, sys.relay.MoveSessionAfterExport(sessionA, a.addr(), b.addr(), e2), ErrStaleMove)
	caller.send(t, media, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), media, "the route stays at the newer owner")
	b.expectNothing(t)
	a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
	require.Never(t, func() bool { return sys.relay.Stats().RouteWrites != writes }, 100*time.Millisecond, 10*time.Millisecond, "a refused move persists nothing")
	loaded, err := store.LoadRoutes(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, e3, loaded[0].Generation)

	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()), "epoch zero is never ordered")
	caller.send(t, media, sys.relay.PublicAddr())
	b.expect(t, caller.addr(), media)

	sys.relay.ForgetSession(sessionA)
	sys.relay.flows.mu.Lock()
	defer sys.relay.flows.mu.Unlock()
	require.Empty(t, sys.relay.flows.moveEpochs, "the applied epoch leaves with the session")
}

// #443: repeating a move at the epoch already applied succeeds and changes
// nothing: the route, its persisted generation and the receiver tenure.
func TestSameEpochMoveIsIdempotent(t *testing.T) {
	store := sessionstore.NewMemory()
	sys := startTestRelay(t, Config{Owners: store, Routes: store, Buffer: &BufferConfig{}})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	caller.send(t, bufferRTP(t, 42, 1), sys.relay.PublicAddr())
	a.expect(t, caller.addr(), bufferRTP(t, 42, 1))
	transferTestLease(t, store, sessionA, b.addr())
	e2 := storeLease(t, sys, sessionA).Epoch
	require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, a.addr(), b.addr(), e2))
	persistedRoute(t, store, caller.addr(), func(record sessionstore.Route) bool { return record.Generation == e2 })
	tenure := routeTenure(sys, sessionA)

	require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, a.addr(), b.addr(), e2))
	require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, netip.AddrPort{}, b.addr(), e2))
	require.Equal(t, tenure, routeTenure(sys, sessionA))
	caller.send(t, bufferRTP(t, 42, 2), sys.relay.PublicAddr())
	b.expect(t, caller.addr(), bufferRTP(t, 42, 2))
	a.expectNothing(t)
	loaded, err := store.LoadRoutes(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, e2, loaded[0].Generation)
}

// #443: over the private API a stale move is a definite 409 refusal with
// its own code, never transport uncertainty. RemoteRelay maps it the same
// way (controlplane TestRemoteRelayStaleMoveIsTyped).
func TestStaleMoveOverHTTPIsTyped(t *testing.T) {
	sys := startTestRelay(t, Config{})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, a.addr(), b.addr(), 5))
	server := httptest.NewServer(sys.relay.PrivateHandler())
	defer server.Close()
	for _, request := range []struct {
		path string
		body RouteRequest
	}{
		{"move", RouteRequest{From: b.addr(), To: a.addr(), Epoch: 4}},
		{"move", RouteRequest{To: a.addr(), Repair: true, Epoch: 4}},
		{"move-after-export", RouteRequest{From: b.addr(), To: a.addr(), Epoch: 4}},
	} {
		err := privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/"+request.path, request.body, nil, RemoteErrors)
		require.ErrorIs(t, err, ErrStaleMove, request.path)
		require.NotErrorIs(t, err, privateapi.ErrUncertain)
		var status *privateapi.HTTPError
		require.ErrorAs(t, err, &status)
		require.Equal(t, http.StatusConflict, status.StatusCode)
	}
	caller.send(t, media, sys.relay.PublicAddr())
	b.expect(t, caller.addr(), media)
	a.expectNothing(t)
}

// #443: a stale move-after-export is refused before its output drain, so
// it sends no marker and leaves the planned hold for the current move.
func TestStaleMoveAfterExportLeavesHold(t *testing.T) {
	sys := startTestRelay(t, Config{HoldTimeout: time.Hour})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	require.NoError(t, sys.relay.MoveSessionAtEpoch(sessionA, a.addr(), b.addr(), 5))
	beginHold(t, sys, b, sessionA)
	require.ErrorIs(t, sys.relay.MoveSessionAfterExport(sessionA, b.addr(), a.addr(), 4), ErrStaleMove)
	b.expectNothing(t)
	done := make(chan error, 1)
	go func() { done <- sys.relay.MoveSessionAfterExport(sessionA, b.addr(), a.addr(), 6) }()
	datagram, ok := receive(t, b.conn, receiveTimeout)
	require.True(t, ok, "the current move still drains the source")
	nonce, ack, valid := relayleg.ParseBarrier(datagram)
	require.True(t, valid)
	require.False(t, ack)
	_, err := b.conn.WriteToUDPAddrPort(relayleg.Barrier(nonce, true), sys.relay.WorkerAddr())
	require.NoError(t, err)
	require.NoError(t, <-done)
	require.EqualValues(t, 1, sys.relay.Stats().OutputDrains)
}
