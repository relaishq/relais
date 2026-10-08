package main

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/transport/v5/packetio"
)

// dtlsPipe is the net.PacketConn a DTLS conn runs on. Reads come from packets
// the demux pushes in; writes go straight to the shared UDP socket unless the
// owning endpoint has been fenced (killed), in which case they are silently
// dropped. Fencing is how an endpoint is destroyed without its DTLS conn
// sending close_notify to the caller.
type dtlsPipe struct {
	sock   *net.UDPConn
	raddr  *net.UDPAddr
	buf    *packetio.Buffer
	fenced *atomic.Bool
	sent   atomic.Int64
	fdrop  atomic.Int64
}

func newDTLSPipe(sock *net.UDPConn, raddr *net.UDPAddr, fenced *atomic.Bool) *dtlsPipe {
	b := packetio.NewBuffer()
	b.SetLimitSize(1 << 20)

	return &dtlsPipe{sock: sock, raddr: raddr, buf: b, fenced: fenced}
}

func (p *dtlsPipe) push(b []byte) { _, _ = p.buf.Write(b, nil) }

func (p *dtlsPipe) ReadFrom(b []byte) (int, net.Addr, error) {
	n, _, err := p.buf.Read(b, nil)

	return n, p.raddr, err
}

func (p *dtlsPipe) WriteTo(b []byte, _ net.Addr) (int, error) {
	if p.fenced.Load() {
		p.fdrop.Add(1)

		return len(b), nil
	}
	p.sent.Add(1)

	return p.sock.WriteToUDP(b, p.raddr)
}

func (p *dtlsPipe) Close() error                       { return p.buf.Close() }
func (p *dtlsPipe) LocalAddr() net.Addr                { return p.sock.LocalAddr() }
func (p *dtlsPipe) SetDeadline(t time.Time) error      { return p.buf.SetReadDeadline(t) }
func (p *dtlsPipe) SetReadDeadline(t time.Time) error  { return p.buf.SetReadDeadline(t) }
func (p *dtlsPipe) SetWriteDeadline(_ time.Time) error { return nil }
