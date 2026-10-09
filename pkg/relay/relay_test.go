package relay

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/sessionstore"
)

// These tests drive the relay through its two sockets only: test callers
// send to the public address, and test workers speak the relay-leg header
// on the private one. The call harness runs real calls through the relay; it
// cannot send arbitrary bytes from a caller's address or see what reaches a
// worker, which is why the forwarding rules are proven here.

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
// packet that is not STUN. Once the caller's flow to worker A is confirmed,
// every other packet from the caller reaches worker A byte for byte,
// whatever it contains, including packets that differ from a valid binding
// request for session B in their first byte only, and RTP that carries the
// STUN magic cookie and a binding request for session B. Only a real STUN
// binding request moves the flow.
//
// The first-byte variants catch a relay that recognizes STUN by its magic
// cookie alone: STUN decoders ignore the top two bits of the message type,
// so the variants starting 0x40, 0x80 (an RTP version byte) and 0xC0 decode
// as binding requests for session B.
func TestNonSTUNPacketsAreForwardedUnparsed(t *testing.T) {
	sys := startTestRelay(t, Config{})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, workerA, sessionA)

	bindB := bindingRequest(t, sessionB)
	for i, pkt := range nonSTUNPackets(t, bindB) {
		caller.send(t, pkt, sys.relay.PublicAddr())
		workerA.expect(t, caller.addr(), pkt, "non-STUN packet %d (first byte %d, %d bytes)", i, pkt[0], len(pkt))
	}
	workerB.expectNothing(t)

	// The same bytes as a STUN binding request do move the flow to B, and,
	// once B answers, the caller's other packets follow it.
	caller.send(t, bindB, sys.relay.PublicAddr())
	workerB.expect(t, caller.addr(), bindB)
	sys.answer(t, workerB, caller)
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

	// Each caller's other packets follow its own flow.
	pktA, pktB := []byte{0x17, 0xfe, 0xfd, 0xaa}, []byte{0x17, 0xfe, 0xfd, 0xbb}
	callerA.send(t, pktA, sys.relay.PublicAddr())
	callerB.send(t, pktB, sys.relay.PublicAddr())
	workerA.expect(t, callerA.addr(), pktA)
	workerB.expect(t, callerB.addr(), pktB)

	// A binding request for a session nobody owns is dropped, and so is
	// anything from an address without a flow.
	stranger := newTestCaller(t)
	stranger.send(t, bindingRequest(t, "nobodyownsthis00"), sys.relay.PublicAddr())
	stranger.send(t, pktA, sys.relay.PublicAddr())
	workerA.expectNothing(t)
	workerB.expectNothing(t)

	stats := sys.relay.Stats()
	assert.EqualValues(t, 1, stats.UnknownSession, "binding requests for unknown sessions")
	assert.EqualValues(t, 1, stats.Unroutable, "packets without a flow")
	assert.Equal(t, 2, stats.Flows, "flows")
}

// TestRelayLegOnlyCarriesWorkersToTheirOwnCallers covers the way back. The
// relay accepts relay-leg datagrams only from registered workers, and sends
// a worker's packet only to a caller whose flow that worker owns, from the
// relay's public address. Anything else on the relay leg is dropped and
// counted, so the relay cannot be made to send from its public address to
// arbitrary destinations.
func TestRelayLegOnlyCarriesWorkersToTheirOwnCallers(t *testing.T) {
	sys := startTestRelay(t, Config{})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	outsider := &testWorker{conn: listenLoopback(t)} // never registered

	// Without a flow, not even a registered worker reaches the caller.
	workerA.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)

	bind := bindingRequest(t, sessionA)
	caller.send(t, bind, sys.relay.PublicAddr())
	workerA.expect(t, caller.addr(), bind)

	// The caller's flow belongs to worker A: an unregistered sender and
	// another registered worker are both refused.
	outsider.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	workerB.sendTo(t, caller.addr(), dtlsReply, sys.relay.WorkerAddr())
	caller.expectNothing(t)

	// Worker A reaches it, from the public address, with packets unchanged.
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
	assert.EqualValues(t, 2, stats.WorkerNoFlow, "datagrams for callers the sender has no flow with")
	assert.EqualValues(t, 1, stats.Malformed, "malformed relay-leg datagrams")
	assert.EqualValues(t, 3, stats.WorkerPackets, "worker packets delivered")
}

// TestFlowsArePendingUntilTheWorkerAnswers: a binding request naming a known
// session admits only a pending flow, because the relay cannot check ICE
// credentials. A pending flow carries the caller's binding requests and
// nothing else. The worker's answer, which it sends only after checking the
// credentials, confirms the flow; a flow the worker never answers expires.
func TestFlowsArePendingUntilTheWorkerAnswers(t *testing.T) {
	const pendingTimeout = 300 * time.Millisecond

	sys := startTestRelay(t, Config{PendingFlowTimeout: pendingTimeout})
	worker := sys.worker(t, sessionA)
	caller, prober := newTestCaller(t), newTestCaller(t)
	bind := bindingRequest(t, sessionA)

	caller.send(t, bind, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), bind)
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)
	caller.send(t, bind, sys.relay.PublicAddr()) // a retransmission
	worker.expect(t, caller.addr(), bind)

	sys.answer(t, worker, caller)
	caller.send(t, media, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), media)

	// The prober knows the session ID but not the ICE password, so the
	// worker never answers it.
	prober.send(t, bind, sys.relay.PublicAddr())
	worker.expect(t, prober.addr(), bind)
	require.Eventually(t, func() bool { return sys.relay.Stats().PendingFlows == 0 },
		10*pendingTimeout, pendingTimeout/10, "unanswered pending flow expires")
	prober.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)

	stats := sys.relay.Stats()
	assert.Equal(t, 1, stats.Flows, "flows")
	assert.EqualValues(t, 2, stats.Unroutable, "packets on pending or expired flows")
}

// TestFlowTableIsBounded floods the relay with binding requests from new
// addresses that name a real session but are never answered. Pending flows
// stay within their limit, the oldest evicted first, and established calls
// keep their flows. Only when nothing is pending does a new flow evict a
// confirmed one: the one idle the longest.
func TestFlowTableIsBounded(t *testing.T) {
	sys := startTestRelay(t, Config{MaxFlows: 4, MaxPendingFlows: 2})
	worker := sys.worker(t, sessionA)
	bind := bindingRequest(t, sessionA)
	call1, call2 := newTestCaller(t), newTestCaller(t)
	sys.connect(t, call1, worker, sessionA)
	sys.connect(t, call2, worker, sessionA)

	probers := make([]*testCaller, 5)
	for i := range probers {
		probers[i] = newTestCaller(t)
		probers[i].send(t, bind, sys.relay.PublicAddr())
		worker.expect(t, probers[i].addr(), bind)
	}
	stats := sys.relay.Stats()
	assert.Equal(t, 2, stats.PendingFlows, "pending flows after the flood")
	assert.Equal(t, 4, stats.Flows, "flows after the flood")
	assert.EqualValues(t, 3, stats.FlowsEvicted, "evicted flows")
	for _, call := range []*testCaller{call1, call2} {
		call.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, call.addr(), media)
	}

	// The newest prober turns out legitimate. A new flow at the limit still
	// evicts a pending one first, then is answered too.
	sys.answer(t, worker, probers[4])
	extra := newTestCaller(t)
	extra.send(t, bind, sys.relay.PublicAddr())
	worker.expect(t, extra.addr(), bind)
	sys.answer(t, worker, extra)
	stats = sys.relay.Stats()
	assert.Equal(t, 0, stats.PendingFlows, "pending flows")
	assert.EqualValues(t, 4, stats.FlowsEvicted, "evicted flows")

	// With four confirmed flows and none pending, a new flow evicts the
	// confirmed flow idle the longest: call1.
	for _, call := range []*testCaller{call2, probers[4], extra} {
		call.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, call.addr(), media)
	}
	late := newTestCaller(t)
	late.send(t, bind, sys.relay.PublicAddr())
	worker.expect(t, late.addr(), bind)
	call1.send(t, media, sys.relay.PublicAddr())
	worker.expectNothing(t)
	call2.send(t, media, sys.relay.PublicAddr())
	worker.expect(t, call2.addr(), media)
	assert.EqualValues(t, 5, sys.relay.Stats().FlowsEvicted, "evicted flows")
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
	workerA.expect(t, callerA.addr(), media, "media on an established flow")
	consent := bindingRequest(t, sessionA)
	callerA.send(t, consent, sys.relay.PublicAddr())
	workerA.expect(t, callerA.addr(), consent, "consent check on an established flow")
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
		require.NoError(t, store.Claim(context.Background(), session, worker.addr()))
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

// TestFlowsAreRebuiltFromTheStore covers both ways a flow disappears: it
// expires when the caller goes quiet, and a restarted relay starts without
// any. Either way the caller's media is dropped until its next binding
// request rebuilds the flow from the session-owner store and the worker
// answers it.
func TestFlowsAreRebuiltFromTheStore(t *testing.T) {
	const flowTimeout = 300 * time.Millisecond

	sys := startTestRelay(t, Config{FlowTimeout: flowTimeout})
	worker := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	sys.connect(t, caller, worker, sessionA)

	t.Run("idle flow expires", func(t *testing.T) {
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

		sys.connect(t, caller, worker, sessionA)
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
// SRTP packet on a confirmed flow: the flow lookup and the relay-leg header
// written in front of the packet in place. Socket I/O is not included.
func BenchmarkCallerPacket(b *testing.B) {
	r, err := New(Config{Owners: sessionstore.NewMemory()})
	require.NoError(b, err)
	b.Cleanup(func() { _ = r.Close() })

	caller := netip.MustParseAddrPort("198.51.100.7:50000")
	worker := netip.MustParseAddrPort("127.0.0.1:4000")
	r.flows.admit(caller, worker, sessionA, time.Now())
	r.flows.answer(caller, worker, time.Now())

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
	require.NoError(t, s.cfg.Owners.Claim(context.Background(), sessionID, w.addr()))
	s.relay.AddWorker(w.addr())
	s.workers = append(s.workers, w.addr())

	return w
}

// connect makes a caller's flow to a worker confirmed, as ICE does: the
// caller's binding request reaches the worker, and the worker answers.
func (s *testSystem) connect(t *testing.T, c *testCaller, w *testWorker, sessionID string) {
	t.Helper()

	bind := bindingRequest(t, sessionID)
	c.send(t, bind, s.relay.PublicAddr())
	w.expect(t, c.addr(), bind, "binding request")
	s.answer(t, w, c)
}

// answer has a worker answer a caller through the relay, as its binding
// success response would.
func (s *testSystem) answer(t *testing.T, w *testWorker, c *testCaller) {
	t.Helper()

	w.sendTo(t, c.addr(), dtlsReply, s.relay.WorkerAddr())
	c.expect(t, s.relay.PublicAddr(), dtlsReply)
}

// testWorker plays a media worker on the relay leg.
type testWorker struct {
	conn *net.UDPConn
}

func (w *testWorker) addr() netip.AddrPort {
	return localAddr(w.conn)
}

// expect requires the next datagram to be pkt from caller. An empty pkt
// accepts any packet.
func (w *testWorker) expect(t *testing.T, caller netip.AddrPort, pkt []byte, msgAndArgs ...any) {
	t.Helper()

	got := w.expectAny(t, caller, msgAndArgs...)
	if len(pkt) > 0 {
		require.Equal(t, pkt, got, msgAndArgs...)
	}
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

	msg, err := stun.Build(
		stun.BindingRequest,
		stun.NewTransactionIDSetter(stun.NewTransactionID()),
		stun.NewUsername(sessionID+":callerufrag"),
		stun.NewShortTermIntegrity("worker-ice-password"),
		stun.Fingerprint,
	)
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
