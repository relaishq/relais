package relay

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/sessionstore"
)

// These tests drive the relay through its two sockets only: test callers
// send to the public address, and test workers speak the relay-leg header
// on the private one. A test worker "authenticates" a binding request by
// answering it with a binding success for its transaction ID, as a real
// worker does after checking the ICE credentials. The call harness runs
// real calls through the relay; it cannot send arbitrary bytes from a
// caller's address or see what reaches a worker, which is why the
// forwarding rules are proven here.

const (
	sessionA = "sessionAsessionA"
	sessionB = "sessionBsessionB"
	sessionC = "sessionCsessionC"

	receiveTimeout = 2 * time.Second
	// quietPeriod is how long a socket must stay silent to show that the
	// relay dropped a packet. Loopback delivers in microseconds.
	quietPeriod = 200 * time.Millisecond
)

var (
	media     = []byte{0x80, 0x6f, 0x00, 0x01, 0xde, 0xad, 0xbe, 0xef}
	dtlsReply = []byte{0x16, 0xfe, 0xfd, 0x00, 0x01}
)

func TestHeaderRoundTrip(t *testing.T) {
	for _, caller := range []string{"192.0.2.7:5000", "[2001:db8::1]:65535", "[::ffff:192.0.2.8]:1"} {
		addr := netip.MustParseAddrPort(caller)
		payload := []byte{0x80, 0x60, 0x00, 0x01}

		datagram := append(AppendHeader(nil, addr), payload...)
		require.Len(t, datagram, HeaderLen(addr)+len(payload), "datagram for %s", caller)

		got, pkt, err := ParseHeader(datagram)
		require.NoError(t, err, caller)
		assert.Equal(t, unmap(addr), got, "caller address")
		assert.Equal(t, payload, pkt, "packet after the header")
	}

	for name, datagram := range map[string][]byte{
		"empty":           nil,
		"short fixed":     {HeaderVersion, familyIPv4, 0},
		"short IPv4":      {HeaderVersion, familyIPv4, 0, 1, 127, 0, 0},
		"short IPv6":      append([]byte{HeaderVersion, familyIPv6, 0, 1}, make([]byte, 15)...),
		"unknown version": {HeaderVersion + 1, familyIPv4, 0, 1, 127, 0, 0, 1},
		"unknown family":  {HeaderVersion, 5, 0, 1, 127, 0, 0, 1},
	} {
		_, _, err := ParseHeader(datagram)
		assert.ErrorIs(t, err, ErrMalformedHeader, name)
	}
}

// TestNonSTUNPacketsAreForwardedUnparsed proves the relay never parses a
// packet that is not STUN. Once the caller's route to worker A is
// confirmed, every other packet from the caller reaches worker A byte for
// byte, whatever it contains, including packets that differ from a valid
// binding request for session B in their first byte only, and RTP that
// carries the STUN magic cookie and a binding request for session B. Only a
// real STUN binding request, once its worker answers it, moves the route.
//
// The first-byte variants catch a relay that recognizes STUN by its magic
// cookie alone: STUN decoders ignore the top two bits of the message type,
// so the variants starting 0x40, 0x80 (an RTP version byte) and 0xC0 decode
// as binding requests for session B.
func TestNonSTUNPacketsAreForwardedUnparsed(t *testing.T) {
	// The caller switches to session B below, which needs its route to A to
	// be inactive first.
	const window = 50 * time.Millisecond

	sys := startTestRelay(t, Config{RouteStickinessWindow: window})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, workerA, sessionA)

	bindB := bindingRequest(t, sessionB)
	for i, pkt := range nonSTUNPackets(t, bindB) {
		caller.send(t, pkt, sys.relay.PublicAddr())
		workerA.expect(t, caller.addr(), pkt, "non-STUN packet %d (first byte %d, %d bytes)", i, pkt[0], len(pkt))
	}
	workerB.expectNothing(t)

	time.Sleep(window) // no answered check for A since connecting
	caller.send(t, bindB, sys.relay.PublicAddr())
	workerB.expect(t, caller.addr(), bindB)
	sys.confirm(t, workerB, caller, bindB)
	caller.send(t, media, sys.relay.PublicAddr())
	workerB.expect(t, caller.addr(), media)
	workerA.expectNothing(t)

	stats := sys.relay.Stats()
	assert.EqualValues(t, 2, stats.STUNRouted, "binding requests routed")
	assert.Zero(t, stats.Unroutable, "unroutable packets")
}

// nonSTUNPackets returns packets that are not STUN under RFC 7983 but would
// misroute to session B if the relay looked inside them.
func nonSTUNPackets(t *testing.T, bindB []byte) [][]byte {
	t.Helper()

	var pkts [][]byte

	// A valid binding request for B with only the first byte changed: every
	// value outside STUN's 0-3, which covers DTLS (20-63), RTP and RTCP
	// (128-191) and everything else.
	for first := 4; first <= 255; first++ {
		pkt := bytes.Clone(bindB)
		pkt[0] = byte(first)
		pkts = append(pkts, pkt)
	}

	// STUN's first bytes without the magic cookie.
	for first := range 4 {
		pkt := bytes.Clone(bindB)
		pkt[0] = byte(first)
		pkt[4] ^= 0xff
		pkts = append(pkts, pkt)
	}

	// RTP whose timestamp is the STUN magic cookie and whose payload is the
	// binding request for B.
	rtp := []byte{0x80, 0x6f, 0x00, 0x01, 0x21, 0x12, 0xa4, 0x42, 0x00, 0x00, 0x00, 0x01}
	pkts = append(pkts, append(rtp, bindB...))

	// A DTLS handshake record header followed by garbage, a one-byte packet,
	// and random packets of every size up to a full Ethernet MTU.
	pkts = append(pkts, append([]byte{0x16, 0xfe, 0xfd, 0x00, 0x00}, bytes.Repeat([]byte{0xff}, 64)...))
	pkts = append(pkts, []byte{0xc0})
	random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test data, not security
	for range 200 {
		pkt := make([]byte, 1+random.IntN(1500))
		for i := range pkt {
			pkt[i] = byte(random.Uint32())
		}
		if pkt[0] <= 3 {
			pkt[0] += 4
		}
		pkts = append(pkts, pkt)
	}

	return pkts
}

func TestBindingRequestsRouteBySessionOwner(t *testing.T) {
	sys := startTestRelay(t, Config{})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	callerA, callerB := newTestCaller(t), newTestCaller(t)
	sys.connect(t, callerA, workerA, sessionA)
	sys.connect(t, callerB, workerB, sessionB)

	// Each caller's other packets follow its own route.
	pktA, pktB := []byte{0x17, 0xfe, 0xfd, 0xaa}, []byte{0x17, 0xfe, 0xfd, 0xbb}
	callerA.send(t, pktA, sys.relay.PublicAddr())
	callerB.send(t, pktB, sys.relay.PublicAddr())
	workerA.expect(t, callerA.addr(), pktA)
	workerB.expect(t, callerB.addr(), pktB)

	// A binding request for a session nobody owns is dropped, and so is
	// anything from an address without a route.
	stranger := newTestCaller(t)
	stranger.send(t, bindingRequest(t, "nobodyownsthis00"), sys.relay.PublicAddr())
	stranger.send(t, pktA, sys.relay.PublicAddr())
	workerA.expectNothing(t)
	workerB.expectNothing(t)

	stats := sys.relay.Stats()
	assert.EqualValues(t, 1, stats.UnknownSession, "binding requests for unknown sessions")
	assert.EqualValues(t, 1, stats.Unroutable, "packets without a route")
	assert.Equal(t, 2, stats.Flows, "confirmed routes")
	assert.EqualValues(t, 2, stats.FlowsPromoted, "promoted candidates")
}

// TestRelayLegOnlyCarriesWorkersToTheirOwnCallers covers the way back. The
// relay accepts relay-leg datagrams only from registered workers, and sends
// a worker's packet only to a caller whose confirmed route is that worker,
// or, for the binding success that confirms it, whose candidate is.
// Anything else on the relay leg is dropped and counted, so the relay cannot
// be made to send from its public address to arbitrary destinations.
func TestRelayLegOnlyCarriesWorkersToTheirOwnCallers(t *testing.T) {
	sys := startTestRelay(t, Config{})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	outsider := &testWorker{conn: listenLoopback(t)} // never registered

	// Without a route, not even a registered worker reaches the caller.
	workerA.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)

	bind := bindingRequest(t, sessionA)
	caller.send(t, bind, sys.relay.PublicAddr())
	workerA.expect(t, caller.addr(), bind)

	// The caller's candidate is worker A. A binding success for the request
	// from an outsider or from worker B is refused, and so is anything but
	// that binding success from worker A.
	success := bindingSuccess(t, bind)
	outsider.sendTo(t, caller.addr(), success, sys.relay.WorkerAddr())
	workerB.sendTo(t, caller.addr(), success, sys.relay.WorkerAddr())
	workerA.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)

	// Worker A's binding success confirms the route; then worker A reaches
	// the caller, from the public address, with packets unchanged.
	sys.confirm(t, workerA, caller, bind)
	for _, pkt := range [][]byte{dtlsReply, bytes.Repeat([]byte{0x90}, 1200), bindingRequest(t, sessionB)} {
		workerA.sendTo(t, caller.addr(), pkt, sys.relay.WorkerAddr())
		caller.expect(t, sys.relay.PublicAddr(), pkt)
	}

	// A datagram without a valid header is dropped.
	_, err := workerA.conn.WriteToUDPAddrPort([]byte{0x80, 0x00}, sys.relay.WorkerAddr())
	require.NoError(t, err)
	require.Eventually(t, func() bool { return sys.relay.Stats().Malformed == 1 }, receiveTimeout, time.Millisecond,
		"malformed datagram dropped")

	// A removed worker is an outsider again.
	sys.relay.RemoveWorker(workerA.addr())
	workerA.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)

	stats := sys.relay.Stats()
	assert.EqualValues(t, 2, stats.UnknownWorker, "datagrams from unregistered senders")
	assert.EqualValues(t, 3, stats.WorkerNoFlow, "datagrams for callers the sender may not reach")
	assert.EqualValues(t, 1, stats.Malformed, "malformed relay-leg datagrams")
	assert.EqualValues(t, 4, stats.WorkerPackets, "worker packets delivered")
}

// TestCandidatesNeedTheWorkersBindingSuccess: a binding request naming a
// known session only proposes a candidate, because the relay cannot check
// ICE credentials. A candidate carries the caller's binding requests for
// its session and nothing else. Only the candidate worker's binding success
// for one of those requests confirms it: the worker's media, or a binding
// success for another transaction, does not. A candidate nobody confirms
// expires.
func TestCandidatesNeedTheWorkersBindingSuccess(t *testing.T) {
	// Long enough for the steps up to the confirmation (about 0.4 s).
	const pendingTimeout = 1500 * time.Millisecond

	sys := startTestRelay(t, Config{PendingFlowTimeout: pendingTimeout})
	worker := sys.worker(t, sessionA)
	caller, prober := newTestCaller(t), newTestCaller(t)

	bind := bindingRequest(t, sessionA)
	caller.send(t, bind, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), bind)
	retry := bindingRequest(t, sessionA) // a new check rides the candidate
	caller.send(t, retry, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), retry)

	worker.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	worker.sendTo(t, caller.addr(), bindingSuccess(t, bindingRequest(t, sessionA)), sys.relay.WorkerAddr())
	caller.expectNothing(t)
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)
	assert.Zero(t, sys.relay.Stats().FlowsPromoted, "promotions without a matching binding success")

	sys.confirm(t, worker, caller, retry)
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), media)

	// The prober knows the session ID but not the ICE password, so the
	// worker never confirms it.
	prober.send(t, bind, sys.relay.PublicAddr())
	worker.expect(t, prober.addr(), bind)
	require.Eventually(t, func() bool { return sys.relay.Stats().PendingFlows == 0 },
		10*pendingTimeout, pendingTimeout/10, "unconfirmed candidate expires")
	prober.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)

	stats := sys.relay.Stats()
	assert.Equal(t, 1, stats.Flows, "confirmed routes")
	assert.EqualValues(t, 1, stats.FlowsPromoted, "promoted candidates")
	assert.EqualValues(t, 2, stats.Unroutable, "packets without a confirmed route")
}

// TestSpoofedRequestForAnotherSessionLeavesTheCallAlone sends, from an
// established caller's address, a binding request for another known
// session: what an attacker spoofing the caller's address would send. The
// caller's route is active, so the relay drops the request instead of
// proposing the other worker, and the call keeps its route, media and
// consent checks.
func TestSpoofedRequestForAnotherSessionLeavesTheCallAlone(t *testing.T) {
	sys := startTestRelay(t, Config{})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, workerA, sessionA)

	spoofed := bindingRequest(t, sessionB)
	caller.send(t, spoofed, sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().FlowsRejected == 1 }, receiveTimeout, time.Millisecond,
		"the active route rejects the other session")
	workerB.expectNothing(t)
	require.Zero(t, sys.relay.Stats().PendingFlows, "no candidate for the other session")

	caller.send(t, media, sys.relay.PublicAddr())
	workerA.expect(t, caller.addr(), media, "media after the spoofed request")
	consent := bindingRequest(t, sessionA)
	caller.send(t, consent, sys.relay.PublicAddr())
	workerA.expect(t, caller.addr(), consent, "consent check after the spoofed request")
	sys.confirm(t, workerA, caller, consent)
	workerB.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)
}

// TestOwnerChangeMovesTheCallOnlyOnceTheNewOwnerAnswers changes a session's
// owner in the store mid-call. The relay notices on the caller's next
// consent check and proposes the new owner as a candidate, but media keeps
// going to the old owner until the new one authenticates a consent check.
func TestOwnerChangeMovesTheCallOnlyOnceTheNewOwnerAnswers(t *testing.T) {
	sys := startTestRelay(t, Config{})
	oldOwner, newOwner := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, oldOwner, sessionA)

	transferTestLease(t, sys.cfg.Owners, sessionA, newOwner.addr())
	consent := bindingRequest(t, sessionA)
	caller.send(t, consent, sys.relay.PublicAddr())
	oldOwner.expect(t, caller.addr(), consent, "the check that finds the new owner")
	require.Eventually(t, func() bool { return sys.relay.Stats().PendingFlows == 1 }, receiveTimeout, time.Millisecond,
		"new owner proposed")

	caller.send(t, media, sys.relay.PublicAddr())
	oldOwner.expect(t, caller.addr(), media, "media before the new owner answers")

	consent = bindingRequest(t, sessionA)
	caller.send(t, consent, sys.relay.PublicAddr())
	newOwner.expect(t, caller.addr(), consent, "the next check goes to the new owner")
	sys.confirm(t, newOwner, caller, consent)
	caller.send(t, media, sys.relay.PublicAddr())
	newOwner.expect(t, caller.addr(), media, "media after the new owner answers")
	oldOwner.expectNothing(t)
}

// TestFlowTableIsBoundedWithoutEvictingCalls floods the relay with binding
// requests from new addresses that name a real session but are never
// confirmed. Candidates stay within their limit, the oldest evicted first.
// When confirmed routes fill the table, a new candidate is rejected rather
// than evicting a live call.
func TestFlowTableIsBoundedWithoutEvictingCalls(t *testing.T) {
	sys := startTestRelay(t, Config{MaxFlows: 4, MaxPendingFlows: 2})
	worker := sys.worker(t, sessionA)
	call1, call2 := newTestCaller(t), newTestCaller(t)
	sys.connect(t, call1, worker, sessionA)
	sys.connect(t, call2, worker, sessionA)

	probers := make([]*testCaller, 5)
	requests := make([][]byte, len(probers))
	for i := range probers {
		probers[i], requests[i] = newTestCaller(t), bindingRequest(t, sessionA)
		probers[i].send(t, requests[i], sys.relay.PublicAddr())
		worker.expect(t, probers[i].addr(), requests[i])
	}
	stats := sys.relay.Stats()
	assert.Equal(t, 2, stats.PendingFlows, "candidates after the flood")
	assert.Equal(t, 2, stats.Flows, "confirmed routes after the flood")
	assert.EqualValues(t, 3, stats.FlowsEvicted, "evicted candidates")

	// Two more callers turn out legitimate and fill the table with
	// confirmed routes; the second evicts the last stale candidate.
	sys.confirm(t, worker, probers[4], requests[4])
	call3 := newTestCaller(t)
	sys.connect(t, call3, worker, sessionA)
	stats = sys.relay.Stats()
	assert.Equal(t, 4, stats.Flows, "confirmed routes")
	assert.Equal(t, 0, stats.PendingFlows, "candidates")
	assert.EqualValues(t, 4, stats.FlowsEvicted, "evicted candidates")

	// The table is full of calls: a new caller is turned away and no call
	// loses its route.
	late := newTestCaller(t)
	late.send(t, bindingRequest(t, sessionA), sys.relay.PublicAddr())
	worker.expectNothing(t)
	assert.EqualValues(t, 1, sys.relay.Stats().FlowsRejected, "rejected candidates")
	for _, call := range []*testCaller{call1, call2, probers[4], call3} {
		call.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, call.addr(), media)
	}
}

// TestSlowStoreDoesNotStallEstablishedFlows blocks the session-owner store.
// An established call's media and consent checks still go through at once,
// while new binding requests wait for their lookup, which runs once per
// session however many requests wait on it.
func TestSlowStoreDoesNotStallEstablishedFlows(t *testing.T) {
	store := newGatedOwners()
	sys := startTestRelay(t, Config{Owners: store, OwnerLookupTimeout: time.Minute})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	callerA, callerB := newTestCaller(t), newTestCaller(t)
	sys.connect(t, callerA, workerA, sessionA)

	store.block()
	t.Cleanup(store.unblock)

	bindsB := [][]byte{bindingRequest(t, sessionB), bindingRequest(t, sessionB), bindingRequest(t, sessionB)}
	for _, bind := range bindsB {
		callerB.send(t, bind, sys.relay.PublicAddr())
	}
	require.Eventually(t, func() bool { return store.calls(sessionB) == 1 }, receiveTimeout, time.Millisecond,
		"lookup for session B started")

	callerA.send(t, media, sys.relay.PublicAddr())
	workerA.expect(t, callerA.addr(), media, "media on an established route")
	consent := bindingRequest(t, sessionA)
	callerA.send(t, consent, sys.relay.PublicAddr())
	workerA.expect(t, callerA.addr(), consent, "consent check on an established route")
	workerB.expectNothing(t)

	store.unblock()
	for i, bind := range bindsB {
		workerB.expect(t, callerB.addr(), bind, "binding request %d for session B", i)
	}
	assert.Equal(t, 1, store.calls(sessionB), "lookups for session B")
}

// TestOwnerLookupsAreBounded: lookups run OwnerLookups at a time, at most
// MaxQueuedLookups sessions wait, and each session holds a bounded number of
// binding requests. Requests beyond those bounds are dropped and counted;
// callers retransmit.
func TestOwnerLookupsAreBounded(t *testing.T) {
	store := newGatedOwners()
	sys := startTestRelay(t, Config{
		Owners: store, OwnerLookups: 1, MaxQueuedLookups: 2, OwnerLookupTimeout: time.Minute,
	})
	worker := sys.worker(t, sessionA)
	for _, session := range []string{sessionB, sessionC} {
		claimTestLease(t, store, session, worker.addr())
	}
	caller := newTestCaller(t)

	store.block()
	t.Cleanup(store.unblock)

	caller.send(t, bindingRequest(t, sessionA), sys.relay.PublicAddr())
	caller.send(t, bindingRequest(t, sessionB), sys.relay.PublicAddr())
	caller.send(t, bindingRequest(t, sessionC), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().LookupsDropped == 1 }, receiveTimeout, time.Millisecond,
		"the third session's request is dropped")
	assert.Equal(t, 0, store.calls(sessionB), "the second lookup waits for the first")

	for range maxWaitersPerSession + 2 {
		caller.send(t, bindingRequest(t, sessionB), sys.relay.PublicAddr())
	}
	require.Eventually(t, func() bool { return sys.relay.Stats().LookupsDropped == 4 }, receiveTimeout, time.Millisecond,
		"requests beyond the per-session bound are dropped")

	store.unblock()
	for i := range 1 + maxWaitersPerSession {
		worker.expectAny(t, caller.addr(), "forwarded binding request %d", i)
	}
	worker.expectNothing(t)
	assert.Zero(t, store.calls(sessionC), "lookups for the dropped session")
}

// TestLookupQueueHoldsBoundedBytes: binding requests copied while their
// lookup waits stay within MaxQueuedLookupBytes, and an oversized binding
// request is never held at all.
func TestLookupQueueHoldsBoundedBytes(t *testing.T) {
	bind := bindingRequest(t, sessionB)
	held := MaxHeaderLen + len(bind)
	store := newGatedOwners()
	sys := startTestRelay(t, Config{
		Owners: store, MaxQueuedLookupBytes: 2*held + held/2, OwnerLookupTimeout: time.Minute,
	})
	worker := sys.worker(t, sessionB)
	claimTestLease(t, store, sessionC, worker.addr())
	caller, other := newTestCaller(t), newTestCaller(t)

	store.block()
	t.Cleanup(store.unblock)

	for range 4 {
		caller.send(t, bindingRequest(t, sessionB), sys.relay.PublicAddr())
	}
	other.send(t, paddedBindingRequest(t, sessionC, maxQueuedBindingRequest+4), sys.relay.PublicAddr())
	require.Eventually(t, func() bool { return sys.relay.Stats().LookupsDropped == 3 }, receiveTimeout, time.Millisecond,
		"requests over the byte budget, and the oversized one, are dropped")

	store.unblock()
	for i := range 2 {
		worker.expectAny(t, caller.addr(), "held binding request %d", i)
	}
	worker.expectNothing(t)
	assert.Zero(t, store.calls(sessionC), "lookups for the oversized request")
}

// TestStaleLookupResultCannotOverrideNewerRoute holds a lookup's result
// back after it read the session's old owner, moves the session to a new
// owner, and sends another binding request. The request waits for the held
// lookup to be applied and then gets a fresh lookup of its own, so the new
// owner's answer is what confirms the caller's route; the stale result
// never replaces it.
func TestStaleLookupResultCannotOverrideNewerRoute(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	sys := startTestRelay(t, Config{
		OwnerLookups: 2,
		beforeApply: func(sessionID string) {
			if sessionID == sessionA && held.CompareAndSwap(false, true) {
				close(reached)
				<-release
			}
		},
	})
	oldOwner, newOwner := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)

	first := bindingRequest(t, sessionA)
	caller.send(t, first, sys.relay.PublicAddr())
	select {
	case <-reached:
	case <-time.After(receiveTimeout):
		t.Fatal("the first lookup never finished")
	}

	transferTestLease(t, sys.cfg.Owners, sessionA, newOwner.addr())
	second := bindingRequest(t, sessionA)
	caller.send(t, second, sys.relay.PublicAddr())
	time.Sleep(quietPeriod) // room for a concurrent lookup to apply first, if there were one
	close(release)

	oldOwner.expect(t, caller.addr(), first, "the held result forwards the first request")
	newOwner.expect(t, caller.addr(), second, "the second request gets the new owner")
	sys.confirm(t, newOwner, caller, second)
	oldOwner.sendTo(t, caller.addr(), bindingSuccess(t, first), sys.relay.WorkerAddr())
	caller.expectNothing(t)

	caller.send(t, media, sys.relay.PublicAddr())
	newOwner.expect(t, caller.addr(), media)
	oldOwner.expectNothing(t)
}

// TestFlowsAreRebuiltFromTheStore covers both ways a route disappears: it
// expires when the caller goes quiet, and a restarted relay starts without
// any. Either way the caller's media is dropped until its next binding
// request is routed by the session-owner store and the worker answers it;
// after a restart the worker's own media confirms nothing.
func TestFlowsAreRebuiltFromTheStore(t *testing.T) {
	const flowTimeout = 300 * time.Millisecond

	sys := startTestRelay(t, Config{FlowTimeout: flowTimeout, RouteStickinessWindow: flowTimeout})
	worker := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, worker, sessionA)

	t.Run("idle route expires", func(t *testing.T) {
		time.Sleep(2 * flowTimeout)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expectNothing(t)

		sys.connect(t, caller, worker, sessionA)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), media)
	})

	t.Run("restarted relay", func(t *testing.T) {
		sys.restart(t)

		caller.send(t, media, sys.relay.PublicAddr())
		worker.expectNothing(t)
		worker.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
		caller.expectNothing(t)

		consent := bindingRequest(t, sessionA)
		caller.send(t, consent, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), consent)
		worker.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
		caller.expectNothing(t)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expectNothing(t)

		sys.confirm(t, worker, caller, consent)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), media)
	})
}

func TestNewRequiresOwnersAndSpecificIP(t *testing.T) {
	_, err := New(Config{})
	require.Error(t, err, "no session-owner store")

	_, err = New(Config{Owners: sessionstore.NewMemory(), PublicAddr: "0.0.0.0:0"})
	require.Error(t, err, "unspecified public IP")
}

// BenchmarkCallerPacket measures the relay's per-packet work for a caller's
// SRTP packet on a confirmed route: the route lookup and the relay-leg
// header written in front of the packet in place. Socket I/O is not
// included.
func BenchmarkCallerPacket(b *testing.B) {
	r, err := New(Config{Owners: sessionstore.NewMemory()})
	require.NoError(b, err)
	b.Cleanup(func() { _ = r.Close() })

	caller := netip.MustParseAddrPort("198.51.100.7:50000")
	worker := netip.MustParseAddrPort("127.0.0.1:4000")
	txID := stun.NewTransactionID()
	success, err := stun.Build(stun.NewTransactionIDSetter(txID), stun.BindingSuccess, stun.Fingerprint)
	require.NoError(b, err)
	require.True(b, r.flows.admit(caller, worker, sessionA, txID, time.Now()))
	allowed, promoted := r.flows.answer(caller, worker, success.Raw, time.Now())
	require.True(b, allowed && promoted == sessionA)

	buf := make([]byte, MaxHeaderLen+1200)
	buf[MaxHeaderLen] = 0x80 // an RTP version byte
	pkt := buf[MaxHeaderLen:]

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got, ok := r.route(pkt, caller)
		if !ok || got != worker {
			b.Fatal("no route")
		}
		start := MaxHeaderLen - HeaderLen(caller)
		AppendHeader(buf[start:start], caller)
	}
}

// testSystem is a relay with its session-owner store and test workers.
type testSystem struct {
	cfg     Config
	relay   *Relay
	workers []netip.AddrPort
}

// startTestRelay starts a relay; cfg.Owners defaults to an in-memory store.
func startTestRelay(t *testing.T, cfg Config) *testSystem {
	t.Helper()

	if cfg.Owners == nil {
		cfg.Owners = sessionstore.NewMemory()
	}
	sys := &testSystem{cfg: cfg}
	var err error
	sys.relay, err = New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sys.relay.Close()) })

	return sys
}

// restart replaces the relay with a new one on the same addresses, store
// and workers.
func (s *testSystem) restart(t *testing.T) {
	t.Helper()

	require.NoError(t, s.relay.Close())
	cfg := s.cfg
	cfg.PublicAddr = s.relay.PublicAddr().String()
	cfg.WorkerAddr = s.relay.WorkerAddr().String()
	cfg.Workers = s.workers
	var err error
	s.relay, err = New(cfg)
	require.NoError(t, err)
}

// worker starts a registered test worker that owns a session.
func (s *testSystem) worker(t *testing.T, sessionID string) *testWorker {
	t.Helper()

	w := &testWorker{conn: listenLoopback(t)}
	claimTestLease(t, s.cfg.Owners, sessionID, w.addr())
	s.relay.AddWorker(w.addr())
	s.workers = append(s.workers, w.addr())

	return w
}

// connect gives a caller a confirmed route to a worker, as ICE does: the
// caller's binding request reaches the worker, and the worker answers it.
func (s *testSystem) connect(t *testing.T, c *testCaller, w *testWorker, sessionID string) {
	t.Helper()

	bind := bindingRequest(t, sessionID)
	c.send(t, bind, s.relay.PublicAddr())
	w.expect(t, c.addr(), bind, "binding request")
	s.confirm(t, w, c, bind)
}

// confirm has a worker answer a caller's binding request with a binding
// success through the relay, and requires the caller to receive it.
func (s *testSystem) confirm(t *testing.T, w *testWorker, c *testCaller, request []byte) {
	t.Helper()

	success := bindingSuccess(t, request)
	w.sendTo(t, c.addr(), success, s.relay.WorkerAddr())
	c.expect(t, s.relay.PublicAddr(), success)
}

// testWorker plays a media worker on the relay leg.
type testWorker struct {
	conn *net.UDPConn
}

func (w *testWorker) addr() netip.AddrPort {
	return localAddr(w.conn)
}

// expect requires the next datagram to be pkt from caller.
func (w *testWorker) expect(t *testing.T, caller netip.AddrPort, pkt []byte, msgAndArgs ...any) {
	t.Helper()

	got := w.expectAny(t, caller, msgAndArgs...)
	require.Equal(t, pkt, got, msgAndArgs...)
}

// expectAny requires the next datagram to come from caller and returns its
// packet.
func (w *testWorker) expectAny(t *testing.T, caller netip.AddrPort, msgAndArgs ...any) []byte {
	t.Helper()

	datagram, ok := receive(t, w.conn, receiveTimeout)
	require.True(t, ok, msgAndArgs...)
	gotCaller, gotPkt, err := ParseHeader(datagram)
	require.NoError(t, err, msgAndArgs...)
	require.Equal(t, caller, gotCaller, msgAndArgs...)

	return gotPkt
}

func (w *testWorker) expectNothing(t *testing.T) {
	t.Helper()

	datagram, ok := receive(t, w.conn, quietPeriod)
	require.False(t, ok, "worker received %d bytes", len(datagram))
}

func (w *testWorker) sendTo(t *testing.T, caller netip.AddrPort, pkt []byte, relay netip.AddrPort) {
	t.Helper()

	_, err := w.conn.WriteToUDPAddrPort(append(AppendHeader(nil, caller), pkt...), relay)
	require.NoError(t, err)
}

// testCaller plays a caller on the public side.
type testCaller struct {
	conn *net.UDPConn
}

func newTestCaller(t *testing.T) *testCaller {
	t.Helper()

	return &testCaller{conn: listenLoopback(t)}
}

func (c *testCaller) addr() netip.AddrPort {
	return localAddr(c.conn)
}

func (c *testCaller) send(t *testing.T, pkt []byte, to netip.AddrPort) {
	t.Helper()

	_, err := c.conn.WriteToUDPAddrPort(pkt, to)
	require.NoError(t, err)
}

func (c *testCaller) expect(t *testing.T, from netip.AddrPort, pkt []byte) {
	t.Helper()

	require.NoError(t, c.conn.SetReadDeadline(time.Now().Add(receiveTimeout)))
	buf := make([]byte, maxDatagram)
	n, gotFrom, err := c.conn.ReadFromUDPAddrPort(buf)
	require.NoError(t, err)
	require.Equal(t, from, unmap(gotFrom), "packet source")
	require.Equal(t, pkt, buf[:n])
}

func (c *testCaller) expectNothing(t *testing.T) {
	t.Helper()

	datagram, ok := receive(t, c.conn, quietPeriod)
	require.False(t, ok, "caller received %d bytes", len(datagram))
}

// gatedOwners is an in-memory store whose lookups can be held up.
type gatedOwners struct {
	*sessionstore.Memory

	mu      sync.Mutex
	gate    chan struct{} // lookups wait for it to close; nil when open
	lookups map[string]int
}

func newGatedOwners() *gatedOwners {
	return &gatedOwners{Memory: sessionstore.NewMemory(), lookups: make(map[string]int)}
}

func (g *gatedOwners) Owner(ctx context.Context, sessionID string) (netip.AddrPort, error) {
	g.mu.Lock()
	g.lookups[sessionID]++
	gate := g.gate
	g.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return netip.AddrPort{}, ctx.Err()
		}
	}

	return g.Memory.Owner(ctx, sessionID)
}

func (g *gatedOwners) block() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gate == nil {
		g.gate = make(chan struct{})
	}
}

func (g *gatedOwners) unblock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gate != nil {
		close(g.gate)
		g.gate = nil
	}
}

func (g *gatedOwners) calls(sessionID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.lookups[sessionID]
}

func bindingRequest(t *testing.T, sessionID string) []byte {
	t.Helper()

	return paddedBindingRequest(t, sessionID, 0)
}

// paddedBindingRequest is a binding request for a session, padded with a
// comprehension-optional attribute to at least size bytes.
func paddedBindingRequest(t *testing.T, sessionID string, size int) []byte {
	t.Helper()

	build := func(padding int) []byte {
		setters := []stun.Setter{
			stun.BindingRequest,
			stun.NewTransactionIDSetter(stun.NewTransactionID()),
			stun.NewUsername(sessionID + ":callerufrag"),
		}
		if padding > 0 {
			setters = append(setters, stun.RawAttribute{Type: 0x8070, Value: make([]byte, padding)})
		}
		setters = append(setters, stun.NewShortTermIntegrity("worker-ice-password"), stun.Fingerprint)
		msg, err := stun.Build(setters...)
		require.NoError(t, err)

		return msg.Raw
	}

	pkt := build(0)
	if len(pkt) < size {
		pkt = build(size - len(pkt) - 4)
	}

	return pkt
}

// bindingSuccess is a worker's binding success response to a request.
func bindingSuccess(t *testing.T, request []byte) []byte {
	t.Helper()

	req := &stun.Message{Raw: append([]byte(nil), request...)}
	require.NoError(t, req.Decode())
	msg, err := stun.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingSuccess,
		stun.NewShortTermIntegrity("worker-ice-password"), stun.Fingerprint)
	require.NoError(t, err)

	return msg.Raw
}

func listenLoopback(t *testing.T) *net.UDPConn {
	t.Helper()

	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func localAddr(conn *net.UDPConn) netip.AddrPort {
	return unmap(conn.LocalAddr().(*net.UDPAddr).AddrPort())
}

// receive reads one datagram, or reports false when none arrives within d.
func receive(t *testing.T, conn *net.UDPConn, d time.Duration) ([]byte, bool) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(d)))
	buf := make([]byte, maxDatagram)
	n, err := conn.Read(buf)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return nil, false
	}
	require.NoError(t, err)

	return buf[:n], true
}

func claimTestLease(t *testing.T, owners sessionstore.Owners, id string, addr netip.AddrPort) {
	t.Helper()
	_, err := owners.(sessionstore.Store).Claim(context.Background(), id, addr, time.Minute)
	require.NoError(t, err)
}
func transferTestLease(t *testing.T, owners sessionstore.Owners, id string, addr netip.AddrPort) {
	t.Helper()
	store := owners.(sessionstore.Store)
	lease, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	_, err = store.Transfer(context.Background(), lease, addr, time.Minute)
	require.NoError(t, err)
}

// Trusted moves retain authenticated routes and immediately exclude the old
// private leg, including its late binding successes for pending checks.
func TestTrustedMovePreservesRoutesAndFencesOldWorker(t *testing.T) {
	sys := startTestRelay(t, Config{MaxFlows: 2, MaxPendingFlows: 1})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller, other := newTestCaller(t), newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	// Keep another confirmed route at the capacity limit: the move must need
	// no candidate slot and must not evict this route.
	sys.connect(t, other, b, sessionB)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	require.Equal(t, 2, sys.relay.Stats().Flows)
	require.Zero(t, sys.relay.Stats().PendingFlows)
	a.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)
	b.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expect(t, sys.relay.PublicAddr(), dtlsReply)
	caller.send(t, media, sys.relay.PublicAddr())
	b.expect(t, caller.addr(), media)
	other.send(t, media, sys.relay.PublicAddr())
	b.expect(t, other.addr(), media)
	a.expectNothing(t)

	// A pending address for the moved session is not authenticated by a
	// notification. Its old owner's delayed answer must reach nobody.
	fresh := newTestCaller(t)
	// Use a new relay with spare capacity to admit a pending check on A.
	sys2 := startTestRelay(t, Config{})
	a2, b2 := sys2.worker(t, sessionA), sys2.worker(t, sessionB)
	check := bindingRequest(t, sessionA)
	fresh.send(t, check, sys2.relay.PublicAddr())
	a2.expect(t, fresh.addr(), check)
	require.NoError(t, sys2.relay.MoveSession(sessionA, a2.addr(), b2.addr()))
	a2.sendTo(t, fresh.addr(), bindingSuccess(t, check), sys2.relay.WorkerAddr())
	fresh.expectNothing(t)
	require.Zero(t, sys2.relay.Stats().Flows)
	require.Zero(t, sys2.relay.Stats().PendingFlows)
}

// Hold a lookup after it read A, then notify A->B. Even if the late lookup
// proposes an authenticated A answer, it may never restore the old route.
func TestTrustedMoveInvalidatesStaleLookup(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var armed, held atomic.Bool
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	sys := startTestRelay(t, Config{beforeApply: func(id string) {
		if id == sessionA && armed.Load() && held.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}})
	// Release before relay cleanup waits for the held lookup.
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	armed.Store(true)
	consent := bindingRequest(t, sessionA)
	caller.send(t, consent, sys.relay.PublicAddr())
	a.expect(t, caller.addr(), consent)
	select {
	case <-reached:
	case <-time.After(receiveTimeout):
		t.Fatal("lookup not held")
	}
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	once.Do(func() { close(release) })
	require.Eventually(t, func() bool {
		sys.relay.lookups.mu.Lock()
		defer sys.relay.lookups.mu.Unlock()
		return len(sys.relay.lookups.pending) == 0
	}, receiveTimeout, time.Millisecond)
	a.sendTo(t, caller.addr(), bindingSuccess(t, consent), sys.relay.WorkerAddr())
	caller.expectNothing(t)
	require.Zero(t, sys.relay.Stats().PendingFlows)
	caller.send(t, media, sys.relay.PublicAddr())
	b.expect(t, caller.addr(), media)
	a.expectNothing(t)
}
