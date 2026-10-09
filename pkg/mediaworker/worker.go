// Package mediaworker implements the Relais media worker: a minimal WebRTC
// endpoint built from Pion's component libraries (pion/stun for ICE-lite,
// pion/dtls and pion/srtp for DTLS-SRTP, pion/rtp, pion/sdp) instead of
// Pion's PeerConnection.
//
// A media worker terminates one session per caller. A session is a single
// BUNDLEd, rtcp-muxed transport, and every session on a worker shares the
// worker's one UDP socket: its own, or a Socket shared with other workers so
// that sessions can move between them (Socket.Handover). Behind a relay
// (Config.Relay), callers reach the worker only through the relay's public
// address. For now the worker echoes the caller's media back: Opus audio and
// VP8 video, each on the worker's own outbound track with its own SSRC,
// sequence numbers and timestamps. It relays the caller's keyframe requests
// for the echoed video back to the caller's video source.
//
// The worker does not use webrtc.PeerConnection on purpose. Later work must
// export a session's state (ICE credentials, DTLS connection state, SRTP
// rollover counters, outbound track counters) to a session store and resume
// it on another worker. A PeerConnection keeps that state private, so each
// session here keeps it in one plain value; see sessionState.
package mediaworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v4"

	"github.com/relais/pkg/sessionstore"
)

const (
	defaultConnectTimeout = 30 * time.Second

	// ownershipTimeout bounds one session-owner store call.
	ownershipTimeout = 2 * time.Second
)

var (
	// ErrUnsupportedOffer is returned when an SDP offer cannot be answered:
	// it is malformed, or it asks for something the media worker does not
	// support.
	ErrUnsupportedOffer = errors.New("mediaworker: unsupported offer")

	// ErrClosed is returned when the media worker has been closed.
	ErrClosed = errors.New("mediaworker: closed")

	// ErrUnknownSession is returned when a session ID does not exist on this
	// media worker.
	ErrUnknownSession = errors.New("mediaworker: unknown session")
)

// Config configures a media worker.
type Config struct {
	// ListenAddr is the local UDP address of the worker's media socket. All
	// sessions on the worker share this one socket, so the IP must be
	// specific (not 0.0.0.0 or ::). Without a relay, its address is the
	// single host candidate advertised in every answer and must be reachable
	// by callers; browsers such as Chrome do not gather loopback candidates,
	// so they need a LAN address. Behind a relay it is a private address
	// that only the relay sends to. Defaults to "127.0.0.1:0".
	ListenAddr string

	// Relay, when set, puts the worker behind a relay: answers advertise the
	// relay's public address, and every packet goes to and from the relay
	// over the relay leg. Nil means callers reach the worker's socket
	// directly.
	Relay *RelayConfig

	// LoggerFactory is used by the worker and by the Pion components it
	// drives. Defaults to Pion's default logger factory.
	LoggerFactory logging.LoggerFactory

	// ConnectTimeout bounds ICE nomination plus the DTLS handshake for a new
	// session. Defaults to 30 seconds.
	ConnectTimeout time.Duration

	// consentTimeout is RFC 7675's consent timeout, 30 seconds. It is not a
	// deployment setting; it is unexported so this package's tests can
	// shorten it.
	consentTimeout time.Duration

	// socket, set by Socket.NewWorker, is a media socket shared with other
	// workers; the worker attaches to it instead of listening on ListenAddr.
	socket *Socket
}

// RelayConfig puts a media worker behind a relay. A worker behind a relay
// owns its UDP socket; it cannot also share a Socket (moving calls between
// workers through the relay is later work).
type RelayConfig struct {
	// Addr is the relay's private relay-leg address. The worker sends every
	// packet there and accepts packets only from there.
	Addr netip.AddrPort

	// PublicAddr is the relay's public address: the single host candidate
	// in every answer.
	PublicAddr netip.AddrPort

	// Owners is the session-owner store the relay routes by. The worker
	// claims each new session, as owned by its own socket address, before it
	// returns the answer, and releases the session when it ends.
	Owners sessionstore.Owners
}

// Worker is a media worker. It owns one UDP socket, or shares one with other
// workers (see Socket), and the sessions that run over it.
type Worker struct {
	cfg  Config
	log  logging.LeveledLogger
	conn packetConn // its own UDP socket, a port on a shared Socket, or the relay leg (relayConn)
	// localAddr is the UDP socket the worker reads: its own or the shared
	// Socket's. mediaAddr is where callers send: localAddr, or behind a
	// relay the relay's public address.
	localAddr netip.AddrPort
	mediaAddr netip.AddrPort
	readDone  chan struct{}

	mu       sync.Mutex
	sessions map[string]*session         // by session ID, which is also the worker's ICE ufrag
	byAddr   map[netip.AddrPort]*session // caller addresses that passed an ICE check
	closed   bool
	running  sync.WaitGroup // session goroutines
}

// New starts a media worker listening on cfg.ListenAddr.
func New(cfg Config) (*Worker, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.LoggerFactory == nil {
		cfg.LoggerFactory = logging.NewDefaultLoggerFactory()
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	if cfg.consentTimeout <= 0 {
		cfg.consentTimeout = defaultConsentTimeout
	}
	if err := validateRelayConfig(cfg.Relay); err != nil {
		return nil, err
	}
	if cfg.socket != nil {
		if cfg.Relay != nil {
			return nil, errors.New("mediaworker: a worker on a shared Socket cannot also be behind a relay")
		}
		port, err := cfg.socket.attach()
		if err != nil {
			return nil, err
		}

		return start(cfg, port, cfg.socket.LocalAddr()), nil
	}

	addr, err := net.ResolveUDPAddr("udp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("mediaworker: resolve listen address: %w", err)
	}
	if addr.IP == nil || addr.IP.IsUnspecified() {
		return nil, fmt.Errorf("mediaworker: listen address %q must name a specific IP", cfg.ListenAddr)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("mediaworker: listen: %w", err)
	}
	udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()

		return nil, fmt.Errorf("mediaworker: unexpected local address %T", conn.LocalAddr())
	}
	localAddr := udpAddr.AddrPort()

	if cfg.Relay != nil {
		return start(cfg, newRelayConn(conn, cfg.Relay.Addr), localAddr), nil
	}

	return start(cfg, conn, localAddr), nil
}

// start runs a worker on conn.
func start(cfg Config, conn packetConn, localAddr netip.AddrPort) *Worker {
	localAddr = netip.AddrPortFrom(localAddr.Addr().Unmap(), localAddr.Port())
	mediaAddr := localAddr
	if cfg.Relay != nil {
		mediaAddr = cfg.Relay.PublicAddr
	}
	worker := &Worker{
		cfg:       cfg,
		log:       cfg.LoggerFactory.NewLogger("mediaworker"),
		conn:      conn,
		localAddr: localAddr,
		mediaAddr: mediaAddr,
		readDone:  make(chan struct{}),
		sessions:  make(map[string]*session),
		byAddr:    make(map[netip.AddrPort]*session),
	}
	go worker.readLoop()

	return worker
}

// MediaAddr returns the address callers send media to, which is the single
// host candidate in every answer: the worker's UDP socket (its own or a
// shared Socket), or, behind a relay, the relay's public address.
func (w *Worker) MediaAddr() netip.AddrPort {
	return w.mediaAddr
}

// LocalAddr returns the address of the UDP socket the worker reads: its own
// or a shared Socket's. Without a relay it is MediaAddr. Behind a relay it
// is private, and it is the address the worker claims its sessions with in
// the session-owner store.
func (w *Worker) LocalAddr() netip.AddrPort {
	return w.localAddr
}

// CreateSession answers a caller's SDP offer. It creates a session that
// starts answering the caller's ICE checks at once, and returns the session
// ID and the SDP answer. The answer advertises ICE-lite, BUNDLE, rtcp-mux and
// a single host candidate. Behind a relay, the session is claimed in the
// session-owner store before the answer is returned, so the relay can route
// the caller's first ICE check.
func (w *Worker) CreateSession(ctx context.Context, offerSDP string) (sessionID, answerSDP string, err error) {
	offer, err := parseOffer(offerSDP)
	if err != nil {
		return "", "", err
	}

	sess, err := newSession(w, offer)
	if err != nil {
		return "", "", err
	}

	answer, err := buildAnswer(offer, sess.answerParams())
	if err != nil {
		sess.close()

		return "", "", err
	}
	if err := w.claim(ctx, sess.id); err != nil {
		sess.close()

		return "", "", err
	}

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		sess.close()
		w.release(sess.id) // claimed above, never registered

		return "", "", ErrClosed
	}
	w.sessions[sess.id] = sess
	w.running.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.running.Done()
		sess.run()
	}()

	return sess.id, answer, nil
}

// EndSession hangs up a session.
func (w *Worker) EndSession(sessionID string) error {
	sess := w.session(sessionID)
	if sess == nil {
		return ErrUnknownSession
	}
	sess.close()

	return nil
}

// Close hangs up every session and closes the worker's socket.
func (w *Worker) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()

		return nil
	}
	w.closed = true
	sessions := make([]*session, 0, len(w.sessions))
	for _, sess := range w.sessions {
		sessions = append(sessions, sess)
	}
	w.mu.Unlock()

	// Sessions send close_notify, so the socket stays open until they end.
	for _, sess := range sessions {
		sess.close()
	}
	w.running.Wait()

	err := w.conn.Close()
	<-w.readDone

	return err
}

// readLoop reads the worker's socket. STUN goes to the ICE-lite responder;
// everything else goes to the session that owns the sender's address
// (RFC 7983 demultiplexing happens in the session). Behind a relay, the
// sender is the caller that the relay-leg header names.
func (w *Worker) readLoop() {
	defer close(w.readDone)

	buf := make([]byte, receiveMTU)
	for {
		n, from, err := w.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			w.log.Debugf("read: %v", err)

			continue
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		pkt := buf[:n]

		if stun.IsMessage(pkt) {
			w.handleSTUN(pkt, from)

			continue
		}
		if sess := w.sessionAt(from); sess != nil {
			sess.handlePacket(pkt)
		}
	}
}

func (w *Worker) session(id string) *session {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.sessions[id]
}

func (w *Worker) sessionAt(addr netip.AddrPort) *session {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.byAddr[addr]
}

func (w *Worker) mapAddr(addr netip.AddrPort, sess *session) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// A session that is no longer registered has been closed.
	if w.sessions[sess.id] == sess {
		w.byAddr[addr] = sess
	}
}

func (w *Worker) send(pkt []byte, to netip.AddrPort) (int, error) {
	return w.conn.WriteToUDPAddrPort(pkt, to)
}

// forget removes a closed session and its caller addresses. Behind a relay
// it also releases the session in the session-owner store, if it was the
// session registered under that ID (a duplicate that failed to register is
// ending, not the live one).
func (w *Worker) forget(sess *session) {
	w.mu.Lock()
	registered := w.sessions[sess.id] == sess
	if registered {
		delete(w.sessions, sess.id)
	}
	for addr, owner := range w.byAddr {
		if owner == sess {
			delete(w.byAddr, addr)
		}
	}
	w.mu.Unlock()

	if registered {
		w.release(sess.id)
	}
}

// claim records the worker as a session's owner, so the relay routes the
// session's caller here. Without a relay there is nothing to claim.
func (w *Worker) claim(ctx context.Context, sessionID string) error {
	if w.cfg.Relay == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, ownershipTimeout)
	defer cancel()
	if err := w.cfg.Relay.Owners.Claim(ctx, sessionID, w.localAddr); err != nil {
		return fmt.Errorf("mediaworker: claim session %s: %w", sessionID, err)
	}

	return nil
}

// release removes the worker's claim on a session it no longer serves.
func (w *Worker) release(sessionID string) {
	if w.cfg.Relay == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ownershipTimeout)
	defer cancel()
	if err := w.cfg.Relay.Owners.Release(ctx, sessionID, w.localAddr); err != nil {
		w.log.Warnf("session %s: release ownership: %v", sessionID, err)
	}
}

func validateRelayConfig(cfg *RelayConfig) error {
	switch {
	case cfg == nil:
		return nil
	case !cfg.Addr.IsValid():
		return errors.New("mediaworker: Relay.Addr is required")
	case !cfg.PublicAddr.IsValid() || cfg.PublicAddr.Addr().IsUnspecified():
		return errors.New("mediaworker: Relay.PublicAddr must name a specific IP and port")
	case cfg.Owners == nil:
		return errors.New("mediaworker: Relay.Owners is required")
	default:
		return nil
	}
}
