package mediaworker

import (
	"net"
	"net/netip"
	"sync"

	"github.com/relaishq/relais/internal/relayleg"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/relay"
)

// relayConn is the worker's end of the relay leg: the worker's private UDP
// socket. Caller packets carry the relay-leg header (see package relay);
// private drain barriers are acknowledged by the single worker read loop.
// It reads and writes packets addressed by
// caller, as the worker's own socket does without a relay, so the rest of
// the worker cannot tell the two apart.
type relayConn struct {
	conn      *net.UDPConn
	relay     netip.AddrPort
	peersMu   sync.RWMutex
	peers     map[netip.AddrPort]bool
	lastRelay netip.AddrPort // written/read only by the worker's single packet loop

	// buffers hold outgoing datagrams: header plus packet. Sessions, their
	// DTLS connections and the read loop all send concurrently.
	buffers sync.Pool

	// readMu guards readBuf, which holds an incoming datagram: header plus
	// a packet as long as the caller's read buffer.
	readMu       sync.Mutex
	readBuf      []byte
	bindingMu    sync.Mutex
	bindingToken string
	observed     chan legObservation
}

func newRelayConn(conn *net.UDPConn, relayAddr netip.AddrPort) *relayConn {
	return &relayConn{
		conn:     conn,
		observed: make(chan legObservation, 1),
		relay:    netip.AddrPortFrom(relayAddr.Addr().Unmap(), relayAddr.Port()),
		buffers: sync.Pool{New: func() any {
			buf := make([]byte, 0, relay.MaxHeaderLen+receiveMTU)

			return &buf
		}},
	}
}

// ReadFromUDPAddrPort reads the next packet the relay forwarded into b and
// returns it with the address of the caller that sent it. The datagram is
// read with room for the header on top of len(b), so b holds a packet as
// long as the worker's own socket would deliver into it. Datagrams from
// anywhere but the relay, or with a malformed header, are dropped.
func (c *relayConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	if need := relay.MaxHeaderLen + len(b); len(c.readBuf) < need {
		c.readBuf = make([]byte, need)
	}
	buf := c.readBuf[:relay.MaxHeaderLen+len(b)]
	for {
		n, from, err := c.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return 0, netip.AddrPort{}, err
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		c.peersMu.RLock()
		allowed := from == c.relay || c.peers[from]
		c.peersMu.RUnlock()
		if !allowed {
			continue
		}
		if kind, token, addr, ok := relayleg.ParseBinding(buf[:n]); ok {
			c.bindingMu.Lock()
			matches := token == c.bindingToken
			c.bindingMu.Unlock()
			if kind == relayleg.Observed && matches {
				select {
				case c.observed <- legObservation{address: addr, token: token}:
				default:
				}
			}
			continue
		}
		if id, ack, ok := relayleg.ParseBarrier(buf[:n]); ok {
			if !ack && !workerprobe.BarrierIgnored(c.conn.LocalAddr().(*net.UDPAddr).AddrPort()) {
				// The worker's single read loop finished the previous packet before
				// requesting this read, so the acknowledgement is a drain barrier.
				_, _ = c.conn.WriteToUDPAddrPort(relayleg.Barrier(id, true), from)
			}
			continue
		}
		caller, pkt, err := relay.ParseHeader(buf[:n])
		if err != nil {
			continue
		}

		c.lastRelay = from
		return copy(b, pkt), caller, nil
	}
}

// WriteToUDPAddrPort sends a packet to a caller through the relay.
func (c *relayConn) WriteToUDPAddrPort(p []byte, caller netip.AddrPort) (int, error) {
	return c.writeVia(p, caller, c.relay)
}
func (c *relayConn) writeVia(p []byte, caller, leg netip.AddrPort) (int, error) {
	bufp, _ := c.buffers.Get().(*[]byte)
	datagram := append(relay.AppendHeader((*bufp)[:0], caller), p...)
	_, err := c.conn.WriteToUDPAddrPort(datagram, leg)
	*bufp = datagram
	c.buffers.Put(bufp)
	if err != nil {
		return 0, err
	}

	return len(p), nil
}

func (c *relayConn) Close() error {
	return c.conn.Close()
}
