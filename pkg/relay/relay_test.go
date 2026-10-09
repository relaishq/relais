package relay

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/relais/pkg/sessionstore"
)

// These tests drive the relay through its two sockets only: a test caller
// sends to the public address, and test workers speak the relay-leg header
// on the private one. The call harness runs real calls through the relay; it
// cannot send arbitrary bytes from a caller's address or see what reaches a
// worker, which is why the forwarding rules are proven here.

const (
	sessionA = "sessionAsessionA"
	sessionB = "sessionBsessionB"

	receiveTimeout = 2 * time.Second
	// quietPeriod is how long a socket must stay silent to show that the
	// relay dropped a packet. Loopback delivers in microseconds.
	quietPeriod = 200 * time.Millisecond
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
// packet that is not STUN. Once a binding request has bound the caller's flow
// to worker A, every other packet from the caller reaches worker A byte for
// byte, whatever it contains, including packets that differ from a valid
// binding request for session B in their first byte only, and RTP that
// carries the STUN magic cookie and a binding request for session B. Only
// a real STUN binding request moves the flow.
//
// The first-byte variants catch a relay that recognizes STUN by its magic
// cookie alone: STUN decoders ignore the top two bits of the message type,
// so the variants starting 0x40, 0x80 (an RTP version byte) and 0xC0 decode
// as binding requests for session B.
func TestNonSTUNPacketsAreForwardedUnparsed(t *testing.T) {
	sys := startTestRelay(t, Config{})
	workerA, workerB := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)

	bindA := bindingRequest(t, sessionA)
	caller.send(t, bindA, sys.relay.PublicAddr())
	workerA.expect(t, caller.addr(), bindA)

	bindB := bindingRequest(t, sessionB)
	for i, pkt := range nonSTUNPackets(t, bindB) {
		caller.send(t, pkt, sys.relay.PublicAddr())
		workerA.expect(t, caller.addr(), pkt, "non-STUN packet %d (first byte %d, %d bytes)", i, pkt[0], len(pkt))
	}
	workerB.expectNothing(t)

	// The same bytes as a STUN binding request do move the flow to B, and the
	// caller's other packets follow it.
	caller.send(t, bindB, sys.relay.PublicAddr())
	workerB.expect(t, caller.addr(), bindB)
	srtp := []byte{0x80, 0x6f, 0x12, 0x34, 0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03, 0x04, 0xff}
	caller.send(t, srtp, sys.relay.PublicAddr())
	workerB.expect(t, caller.addr(), srtp)
	workerA.expectNothing(t)

	stats := sys.relay.Stats()
	assert.EqualValues(t, 2, stats.STUNRouted, "binding requests routed by the store")
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

	bindA, bindB := bindingRequest(t, sessionA), bindingRequest(t, sessionB)
	callerA.send(t, bindA, sys.relay.PublicAddr())
	callerB.send(t, bindB, sys.relay.PublicAddr())
	workerA.expect(t, callerA.addr(), bindA)
	workerB.expect(t, callerB.addr(), bindB)

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

// TestWorkerPacketsReachTheCallerFromThePublicAddress checks the way back: a
// worker names the caller in the relay-leg header, and the caller receives
// the packet unchanged from the relay's public address, flow or no flow.
func TestWorkerPacketsReachTheCallerFromThePublicAddress(t *testing.T) {
	sys := startTestRelay(t, Config{})
	worker := sys.worker(t, sessionA)
	caller := newTestCaller(t)

	for _, pkt := range [][]byte{{0x16, 0xfe, 0xfd, 0x01}, bytes.Repeat([]byte{0x90}, 1200), bindingRequest(t, sessionB)} {
		worker.sendTo(t, caller.addr(), pkt, sys.relay.WorkerAddr())
		caller.expect(t, sys.relay.PublicAddr(), pkt)
	}

	// A datagram without a valid header is dropped.
	_, err := worker.conn.WriteToUDPAddrPort([]byte{0x80, 0x00}, sys.relay.WorkerAddr())
	require.NoError(t, err)
	caller.expectNothing(t)
	assert.EqualValues(t, 1, sys.relay.Stats().Malformed, "malformed relay-leg datagrams")
}

// TestFlowsAreRebuiltFromTheStore covers both ways a flow disappears: it
// expires when the caller goes quiet, and a restarted relay starts without
// any. Either way the caller's non-STUN packets are dropped until its next
// binding request rebuilds the flow from the session-owner store.
func TestFlowsAreRebuiltFromTheStore(t *testing.T) {
	const flowTimeout = 300 * time.Millisecond

	sys := startTestRelay(t, Config{FlowTimeout: flowTimeout})
	worker := sys.worker(t, sessionA)
	caller := newTestCaller(t)
	bind, media := bindingRequest(t, sessionA), []byte{0x80, 0x6f, 0x00, 0x01}

	caller.send(t, bind, sys.relay.PublicAddr())
	worker.expect(t, caller.addr(), bind)

	t.Run("idle flow expires", func(t *testing.T) {
		time.Sleep(2 * flowTimeout)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expectNothing(t)

		caller.send(t, bind, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), bind)
		caller.send(t, media, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), media)
	})

	t.Run("restarted relay", func(t *testing.T) {
		sys.restart(t)

		caller.send(t, media, sys.relay.PublicAddr())
		worker.expectNothing(t)

		caller.send(t, bind, sys.relay.PublicAddr())
		worker.expect(t, caller.addr(), bind)
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

// testSystem is a relay with its session-owner store.
type testSystem struct {
	cfg    Config
	owners *sessionstore.Memory
	relay  *Relay
}

func startTestRelay(t *testing.T, cfg Config) *testSystem {
	t.Helper()

	cfg.Owners = sessionstore.NewMemory()
	sys := &testSystem{cfg: cfg, owners: cfg.Owners.(*sessionstore.Memory)}
	var err error
	sys.relay, err = New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sys.relay.Close()) })

	return sys
}

// restart replaces the relay with a new one on the same addresses and store.
func (s *testSystem) restart(t *testing.T) {
	t.Helper()

	require.NoError(t, s.relay.Close())
	cfg := s.cfg
	cfg.PublicAddr = s.relay.PublicAddr().String()
	cfg.WorkerAddr = s.relay.WorkerAddr().String()
	var err error
	s.relay, err = New(cfg)
	require.NoError(t, err)
}

// testWorker plays a media worker on the relay leg.
type testWorker struct {
	conn *net.UDPConn
}

// worker starts a test worker that owns a session.
func (s *testSystem) worker(t *testing.T, sessionID string) *testWorker {
	t.Helper()

	w := &testWorker{conn: listenLoopback(t)}
	require.NoError(t, s.owners.Claim(context.Background(), sessionID, localAddr(w.conn)))

	return w
}

// expect requires the next datagram to be pkt from caller.
func (w *testWorker) expect(t *testing.T, caller netip.AddrPort, pkt []byte, msgAndArgs ...any) {
	t.Helper()

	datagram, ok := receive(t, w.conn, receiveTimeout)
	require.True(t, ok, msgAndArgs...)
	gotCaller, gotPkt, err := ParseHeader(datagram)
	require.NoError(t, err, msgAndArgs...)
	require.Equal(t, caller, gotCaller, msgAndArgs...)
	require.Equal(t, pkt, gotPkt, msgAndArgs...)
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

// BenchmarkCallerPacket measures the relay's per-packet work for a caller's
// SRTP packet on an existing flow: the flow lookup and the relay-leg header
// written in front of the packet in place. Socket I/O is not included.
func BenchmarkCallerPacket(b *testing.B) {
	r, err := New(Config{Owners: sessionstore.NewMemory()})
	require.NoError(b, err)
	b.Cleanup(func() { _ = r.Close() })

	caller := netip.MustParseAddrPort("198.51.100.7:50000")
	worker := netip.MustParseAddrPort("127.0.0.1:4000")
	r.flows.bind(caller, worker, sessionA, time.Now())

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
