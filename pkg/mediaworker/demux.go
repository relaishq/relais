package mediaworker

import (
	"net"
	"time"

	"github.com/pion/transport/v4/packetio"
)

// dtlsEndpointBufferSize bounds DTLS records waiting to be read.
const dtlsEndpointBufferSize = 1000 * 1000

// RFC 7983 demultiplexing by the first byte of a packet. STUN never reaches
// the session: the worker's ICE-lite responder answers it.

func isDTLS(b []byte) bool {
	return len(b) > 0 && b[0] >= 20 && b[0] <= 63
}

func isRTPOrRTCP(b []byte) bool {
	return len(b) > 0 && b[0] >= 128 && b[0] <= 191
}

// isRTCP tells RTCP from RTP on an rtcp-muxed transport (RFC 5761): RTCP
// packet types 192-223 fall where RTP's marker bit and payload type are.
func isRTCP(b []byte) bool {
	return isRTPOrRTCP(b) && len(b) >= 4 && b[1] >= 192 && b[1] <= 223
}

func isRTP(b []byte) bool {
	return isRTPOrRTCP(b) && !isRTCP(b)
}

// dtlsEndpoint is the DTLS side of a session's demultiplexed transport. The
// worker's read loop delivers the caller's DTLS records into it, and the
// DTLS connection's writes go to the caller's nominated address on the
// worker's socket. It implements net.PacketConn, which is what pion/dtls runs
// on (and resumes on).
type dtlsEndpoint struct {
	sess   *session
	buffer *packetio.Buffer
}

func newDTLSEndpoint(sess *session) *dtlsEndpoint {
	buffer := packetio.NewBuffer()
	buffer.SetLimitSize(dtlsEndpointBufferSize)

	return &dtlsEndpoint{sess: sess, buffer: buffer}
}

// deliver queues a DTLS record for the DTLS connection. A full or closed
// buffer drops the record; DTLS retransmits.
func (e *dtlsEndpoint) deliver(pkt []byte) {
	_, _ = e.buffer.Write(pkt)
}

func (e *dtlsEndpoint) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := e.buffer.Read(p)

	return n, e.RemoteAddr(), err
}

// WriteTo sends to the caller's nominated address. Before nomination there
// is nowhere to send, and the record is dropped.
func (e *dtlsEndpoint) WriteTo(p []byte, _ net.Addr) (int, error) {
	e.sess.mu.Lock()
	to := e.sess.state.ICE.RemoteAddr
	e.sess.mu.Unlock()
	if !to.IsValid() {
		return len(p), nil
	}

	return e.sess.worker.send(p, to)
}

// RemoteAddr is the caller's nominated address.
func (e *dtlsEndpoint) RemoteAddr() net.Addr {
	e.sess.mu.Lock()
	defer e.sess.mu.Unlock()

	return net.UDPAddrFromAddrPort(e.sess.state.ICE.RemoteAddr)
}

func (e *dtlsEndpoint) Close() error {
	return e.buffer.Close()
}

func (e *dtlsEndpoint) LocalAddr() net.Addr {
	return net.UDPAddrFromAddrPort(e.sess.worker.MediaAddr())
}

func (e *dtlsEndpoint) SetDeadline(t time.Time) error {
	return e.buffer.SetReadDeadline(t)
}

func (e *dtlsEndpoint) SetReadDeadline(t time.Time) error {
	return e.buffer.SetReadDeadline(t)
}

// SetWriteDeadline is a no-op: writes go straight to the UDP socket.
func (e *dtlsEndpoint) SetWriteDeadline(time.Time) error {
	return nil
}
