package callharness

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
)

// The caller observes its ICE consent checks on its own UDP socket, not
// through the PeerConnection: Pion's GetStats is not safe while the
// PeerConnection closes, and a worker can close it at any time (a hangup,
// or its consent timer, sends a DTLS close_notify on which Pion closes the
// PeerConnection itself). The caller's ICE agent runs on a UDP mux over a
// socket the harness owns, wrapped in stunObserver, which timestamps every
// STUN binding request the caller sends and every binding success response
// it receives for one of them. Nothing here calls into the PeerConnection.

const (
	stunHeaderSize      = 20
	stunMagicCookie     = 0x2112A442
	stunBindingRequest  = 0x0001
	stunBindingSuccess  = 0x0101
	stunTransactionSize = 12

	// maxUnansweredChecks bounds the binding requests the observer waits
	// on; the caller sends one every 2 s once connected.
	maxUnansweredChecks = 1024
)

// callerSocket is the UDP socket a call's ICE agent sends and receives on,
// through mux, with the observer in between.
type callerSocket struct {
	mux      *ice.UDPMuxDefault
	observer *stunObserver
}

// newCallerSocket opens a loopback UDP socket for one call.
func newCallerSocket(rec *recorder) (*callerSocket, error) {
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		return nil, fmt.Errorf("callharness: caller socket: %w", err)
	}
	observer := &stunObserver{conn: conn, rec: rec, unanswered: make(map[[stunTransactionSize]byte]struct{})}

	return &callerSocket{
		mux: ice.NewUDPMuxDefault(ice.UDPMuxParams{
			UDPConn: observer,
			Logger:  logging.NewDefaultLoggerFactory().NewLogger("ice"),
		}),
		observer: observer,
	}, nil
}

// close closes the mux and the socket under it.
func (s *callerSocket) close() error {
	return s.mux.Close()
}

// stunObserver is a net.PacketConn over the caller's UDP socket that records
// the caller's consent checks. It does not embed the socket, so the mux uses
// its ReadFrom and WriteTo.
type stunObserver struct {
	conn net.PacketConn
	rec  *recorder

	mu         sync.Mutex
	unanswered map[[stunTransactionSize]byte]struct{}
	inspect    func([]byte) // test-only raw caller observation; nil normally
}

// WriteTo sends a packet. A binding request counts as sent only once the
// write succeeded. Its answer is awaited from just before the write, so a
// response that the read loop sees before WriteTo returns still matches; the
// request is timestamped then too, so it never appears after its answer.
func (o *stunObserver) WriteTo(p []byte, addr net.Addr) (int, error) {
	txID, isRequest := stunMessage(p, stunBindingRequest)
	if !isRequest {
		return o.conn.WriteTo(p, addr)
	}

	o.mu.Lock()
	if len(o.unanswered) >= maxUnansweredChecks {
		clear(o.unanswered)
	}
	o.unanswered[txID] = struct{}{}
	o.mu.Unlock()

	sentAt := time.Now()
	n, err := o.conn.WriteTo(p, addr)
	if err != nil {
		o.mu.Lock()
		delete(o.unanswered, txID)
		o.mu.Unlock()

		return n, err
	}
	o.rec.consentRequest(sentAt)

	return n, nil
}

func (o *stunObserver) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := o.conn.ReadFrom(p)
	if err != nil {
		return n, addr, err
	}
	o.mu.Lock()
	if o.inspect != nil {
		o.inspect(p[:n])
	}
	o.mu.Unlock()
	if txID, ok := stunMessage(p[:n], stunBindingSuccess); ok {
		o.mu.Lock()
		_, answers := o.unanswered[txID]
		delete(o.unanswered, txID)
		o.mu.Unlock()
		if answers {
			o.rec.consentResponse(time.Now())
		}
	}

	return n, addr, nil
}

func (o *stunObserver) Close() error                       { return o.conn.Close() }
func (o *stunObserver) LocalAddr() net.Addr                { return o.conn.LocalAddr() }
func (o *stunObserver) SetDeadline(t time.Time) error      { return o.conn.SetDeadline(t) }
func (o *stunObserver) SetReadDeadline(t time.Time) error  { return o.conn.SetReadDeadline(t) }
func (o *stunObserver) SetWriteDeadline(t time.Time) error { return o.conn.SetWriteDeadline(t) }

// stunMessage returns the transaction ID of a STUN message of the given type,
// read from its header only.
func stunMessage(p []byte, messageType uint16) ([stunTransactionSize]byte, bool) {
	var txID [stunTransactionSize]byte
	if len(p) < stunHeaderSize || p[0]&0xc0 != 0 ||
		binary.BigEndian.Uint16(p[0:2]) != messageType ||
		binary.BigEndian.Uint32(p[4:8]) != stunMagicCookie {
		return txID, false
	}
	copy(txID[:], p[8:stunHeaderSize])

	return txID, true
}

// consentRequest records a binding request the caller sent.
func (r *recorder) consentRequest(at time.Time) {
	r.consentEvent(at, 1, 0)
}

// consentResponse records a binding success response to one of them.
func (r *recorder) consentResponse(at time.Time) {
	r.consentEvent(at, 0, 1)
}

// consentEvent adds a sample with the running totals of requests sent and
// responses received. Samples stay in time order: a request recorded just
// after its own answer (see WriteTo) takes the answer's time.
func (r *recorder) consentEvent(at time.Time, requests, responses uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}
	sample := consentSample{at: r.since(at), requests: requests, responses: responses}
	if n := len(r.consent); n > 0 {
		last := r.consent[n-1]
		sample.at = max(sample.at, last.at)
		sample.requests += last.requests
		sample.responses += last.responses
	}
	r.consent = append(r.consent, sample)
}
