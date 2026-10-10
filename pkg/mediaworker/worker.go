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
// The worker does not use webrtc.PeerConnection on purpose. It snapshots
// session state (ICE credentials, DTLS connection state, SRTP rollover
// counters, outbound track counters) to a fenced session store and resumes
// it on another worker after a planned move or crash takeover. A PeerConnection keeps that state private, so each
// session here keeps it in one plain value; see sessionState.
//
// # Cached video recovery
//
// Config.FrameCache is shared outside disposable workers. Only complete VP8
// frames (partition-zero start, contiguous packets, final marker) are appended,
// after releasing the session lock. An incomplete keyframe cannot replace the
// current group. Planned moves never replay. Crash resumes (SequenceMargin>0)
// default to cache plus PLI; DisableFrameCache and DisableResumePLI select each
// independently. Nil FrameCache leaves caching unconfigured. Relay-mode cache
// deletion belongs to the control plane, which knows the terminal call state;
// a stale worker's consent expiry or Close cannot erase a successor's frames.
//
// Current reads have their own configurable 150 ms timeout. Failure is a miss,
// and groups beyond either ReplayMaxDuration (1 s) or ReplayMaxBytes (256 KiB)
// are skipped whole. The 256 KiB and 1024-packet limits are hard ceilings.
// ReplayMaxBurstDuration defaults to one source frame interval (33 ms for a
// one-frame group). Admission uses half that deadline's nominal wire budget,
// including RTP/SRTP overhead, to leave headroom for timer and scheduling jitter.
// Criterion 4 depends on keyframe size: a keyframe larger than that budget is
// skipped in favour of PLI. Replay additionally allows less
// than margin minus the 5500-index staleness allowance and 65-index guard.
// Reservations must also preserve the number of remaining takeover margins.
// Missing, oversized, or unavailable cached media uses the PLI path.
//
// Before any replay ciphertext leaves, ResumeSession reserves R+1+64 indexes
// in HighestSentIndex, SeqOffset and AdvanceSinceSend and synchronously stores
// the snapshot outside the packet lock. The persisted ReplayFloor also rejects
// source packets rewriting at or below the burst. Subsequent resumes advance
// it to the snapshot high water mark, protecting live indexes too, especially
// on margin-zero moves. Reservation failure suppresses replay. Version 5 snapshots carry
// this floor and reject older versions. Encryption happens before adoption;
// a worker-tracked background task paces bytes at ReplayBitrate (10 Mbps),
// outside the session lock and shared UDP reader. Audio, other sessions and
// STUN continue. ResumeSession returns after adoption, releasing the takeover
// slot and call lock. The default deadline is one frame interval, checked at
// frame boundaries. A started frame finishes before deadline truncation, so a
// complete keyframe is never cut by the deadline. Every exit releases the gate.
// All skips and truncations are logged and counted in Worker.ReplayStats. PLI is requested both
// at adoption and after replay, since a response during replay could be gated.
// Shared-socket handovers skip replay and defer PLI until routed video arrives.
//
// The replay begins at last cached EchoTimestamp plus arrival age, clamped to
// [0,2 s], and at least one source frame interval ahead. Multi-frame groups
// supply their last source spacing; a one-frame group uses 3000 ticks (30 fps).
// Only an already-sent track applies the snapshot timestamp floor, avoiding a
// random unanchored timestamp being compared with zero. Frames are compressed
// to one RTP tick apart. The first live frame advances by the time spent sending
// replay and waiting for live media, then restores source timestamp spacing.
// This maps a paced 90 kHz source clock; a remote cache must retain wall arrival
// times and account for host clock differences.
//
// In this one-worker echo topology, cached frames were already seen by the
// caller. Replaying them restores a decodable older picture; it cannot recreate
// frames lost during the outage or make dependent live interframes decodable.
// A fresh live keyframe is still needed. The harness reports first decoded
// output and first decoded live output from the same kill instant, plus the
// one-frame-interval criterion measured from resumed media and observable
// PictureID/payload attribution. Its natural keyframe interval is normally 1 s.
// Its PLI responder rewinds an already encoded keyframe on the next 33 ms tick,
// with no encoder delay; real browser timing is measured separately by #13.
package mediaworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v4"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/framecache"
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

	// ErrRejoinRequired asks a returned worker to discard its old sessions
	// before acknowledging a second heartbeat. Until that acknowledgment the
	// control plane keeps it dead and excludes it from selections.
	ErrRejoinRequired = errors.New("mediaworker: rejoin requires empty sessions")
)

// Heartbeats is the worker-to-control-plane liveness boundary. The private
// worker address identifies a registration; later this can be a network call.
type Heartbeats interface{ Heartbeat(netip.AddrPort) error }

// Config configures a media worker.
type Config struct {
	// FrameCache lives outside workers. Share one Store across takeover targets.
	// Nil leaves caching unconfigured; the harness supplies shared memory by
	// default. Neither worker construction nor worker death owns this store.
	FrameCache framecache.Store
	// CacheReadTimeout defaults to 150 ms. Failure is a cache miss.
	CacheReadTimeout time.Duration
	// ReplayMaxDuration and ReplayMaxBytes default to 1 s and 256 KiB.
	// Groups above either limit are skipped whole; bytes cannot exceed 256 KiB.
	ReplayMaxDuration time.Duration
	ReplayMaxBytes    int
	// ReplayMaxBurstDuration limits paced wire time and live-video gating.
	// Default: one source frame interval, or 33 ms for a one-frame group.
	// Admission uses half this nominal wire-byte budget for scheduling headroom.
	// Deadline truncation occurs at a frame boundary, finishing a started frame.
	ReplayMaxBurstDuration time.Duration
	// ReplayBitrate caps the encrypted replay burst in bits/s. Default: 10 Mbps.
	ReplayBitrate int
	// DisableFrameCache disables writes and crash replay. Default: enabled.
	DisableFrameCache bool
	// DisableResumePLI disables takeover keyframe requests. Caller feedback
	// requests are still relayed normally. Default: request on takeover.
	DisableResumePLI bool

	// SnapshotInterval defaults to 100 ms. Encoding and storage run outside
	// the session lock, on a background goroutine, never on the packet path.
	SnapshotInterval time.Duration
	// HeartbeatInterval defaults to 100 ms; StartHeartbeats sets the receiver.
	HeartbeatInterval time.Duration

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
// owns its UDP socket; it cannot also share a Socket.
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
	Owners sessionstore.Store

	// LeaseTTL bounds ownership without renewal. Defaults to three seconds.
	LeaseTTL time.Duration
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
	localAddr        netip.AddrPort
	mediaAddr        netip.AddrPort
	readDone         chan struct{}
	stopRenew        chan struct{}
	paused           atomic.Bool
	heartbeatStarted bool

	mu                        sync.Mutex
	sessions                  map[string]*session         // by session ID, which is also the worker's ICE ufrag
	byAddr                    map[netip.AddrPort]*session // caller addresses that passed an ICE check
	closed                    bool
	running                   sync.WaitGroup // session goroutines
	replayStatsMu             sync.Mutex
	replayStats               ReplayStats
	cacheQueueMu              sync.Mutex
	cacheQueues               [cacheConsumers]chan cacheAppend
	cacheLastWarning          time.Time
	cacheQueueClosed          bool
	cachePending, cacheBytes  int
	cacheDropped, cacheErrors uint64
}

// New starts a media worker listening on cfg.ListenAddr.
func New(cfg Config) (*Worker, error) {
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = 100 * time.Millisecond
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 100 * time.Millisecond
	}
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
	if cfg.Relay != nil {
		relayCfg := *cfg.Relay
		if relayCfg.LeaseTTL <= 0 {
			relayCfg.LeaseTTL = 3 * time.Second
		}
		cfg.Relay = &relayCfg
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
		stopRenew: make(chan struct{}),
		sessions:  make(map[string]*session),
		byAddr:    make(map[netip.AddrPort]*session),
	}
	if cfg.Relay != nil {
		workerprobe.Register(localAddr, worker.captureZombie)
		workerprobe.RegisterLifecycle(localAddr, workerprobe.Lifecycle{
			Kill: worker.kill, Pause: worker.paused.Store,
			SnapshotReady: func(id string) bool { s := worker.session(id); return s != nil && s.snapshotStored.Load() },
		})
	}
	for i := range worker.cacheQueues {
		worker.cacheQueues[i] = make(chan cacheAppend, cacheQueueFrames)
		worker.running.Add(1)
		go worker.runCacheAppends(worker.cacheQueues[i])
	}
	go worker.readLoop()
	if cfg.Relay != nil {
		worker.running.Add(1)
		go worker.renewLeases()
	}

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
	if err := w.claim(ctx, sess); err != nil {
		sess.close()

		return "", "", err
	}

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		sess.close()
		w.release(sess) // claimed above, never registered

		return "", "", ErrClosed
	}
	w.sessions[sess.id] = sess
	w.running.Add(2)
	w.mu.Unlock()
	go func() { defer w.running.Done(); sess.snapshotLoop() }()

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
	if w.cfg.Relay != nil {
		workerprobe.Remove(w.localAddr)
	}
	w.closed = true
	close(w.stopRenew)
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
		if w.paused.Load() {
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
// ending, not the live one). Exported sessions retain their lease for the
// control plane to transfer; a stale lease can never release a successor.
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

	if registered && !sess.fenced.Load() {
		w.release(sess)
	}
}

// claim records the worker as a session's owner, so the relay routes the
// session's caller here. Without a relay there is nothing to claim.
func (w *Worker) claim(ctx context.Context, sess *session) error {
	if w.cfg.Relay == nil {
		return nil
	}

	// Keep Close's wait count nonzero while a claim can enqueue cleanup.
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	w.running.Add(1)
	w.mu.Unlock()
	defer w.running.Done()
	ctx, cancel := context.WithTimeout(ctx, ownershipTimeout)
	defer cancel()

	lease, err := w.cfg.Relay.Owners.Claim(ctx, sess.id, w.localAddr, w.cfg.Relay.LeaseTTL)
	if err != nil {
		var transient *sessionstore.TransientError
		if errors.As(err, &transient) && transient.Candidate != nil {
			candidate := *transient.Candidate
			w.running.Go(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), ownershipTimeout)
				defer cancel()
				// Settlement fences a delayed claim as well as finding a committed one.
				// Conditional release can never remove a successor's tenure.
				if resolver, ok := w.cfg.Relay.Owners.(sessionstore.TransitionResolver); ok {
					settled, committed, settleErr := resolver.Settle(cleanup, candidate)
					if settleErr == nil && !committed {
						return
					}
					if committed {
						candidate = settled
					}
				}
				if releaseErr := w.cfg.Relay.Owners.Release(cleanup, candidate); releaseErr != nil {
					w.log.Warnf("session %s: uncertain claim cleanup: %v", sess.id, releaseErr)
				}
			})
		}
		return fmt.Errorf("mediaworker: claim session %s: %w", sess.id, err)
	}

	sess.lease = lease
	return nil
}

func (w *Worker) release(sess *session) {
	if w.cfg.Relay == nil {
		return
	}

	sess.mu.Lock()
	lease := sess.lease
	sess.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), ownershipTimeout)
	defer cancel()

	if err := w.cfg.Relay.Owners.Release(ctx, lease); err != nil {
		w.log.Warnf("session %s: release lease: %v", sess.id, err)
	}
}

// renewLeases keeps live sessions owned. A lost token fences silently, so
// closing the old transport cannot end the call running on its successor.
func (w *Worker) renewLeases() {
	defer w.running.Done()

	ticker := time.NewTicker(max(w.cfg.Relay.LeaseTTL/3, time.Millisecond))
	defer ticker.Stop()

	for {
		select {
		case <-w.stopRenew:
			return
		case <-ticker.C:
			if w.paused.Load() {
				continue
			}
			w.mu.Lock()
			sessions := make([]*session, 0, len(w.sessions))
			for _, sess := range w.sessions {
				sessions = append(sessions, sess)
			}
			w.mu.Unlock()
			var renewals sync.WaitGroup
			if refresher, ok := w.cfg.Relay.Owners.(sessionstore.WorkerIndexRefresher); ok && len(sessions) > 0 {
				// Exactly one non-droppable index refresh per worker renewal tick.
				renewals.Go(func() {
					ctx, cancel := context.WithTimeout(context.Background(), ownershipTimeout)
					defer cancel()
					if err := refresher.RefreshWorkerIndex(ctx, w.localAddr, w.cfg.Relay.LeaseTTL); err != nil {
						w.log.Warnf("refresh worker index: %v", err)
					}
				})
			}
			// At most 32 lease renewals in flight. A slow session must not
			// serialize every other lease behind its ownership timeout.
			slots := make(chan struct{}, 32)
			for _, sess := range sessions {
				slots <- struct{}{}
				renewals.Go(func() {
					defer func() { <-slots }()
					w.renewLease(sess)
				})
			}
			renewals.Wait()
		}
	}
}

func (w *Worker) renewLease(sess *session) {
	sess.mu.Lock()
	lease, fenced := sess.lease, sess.fenced.Load()
	sess.mu.Unlock()
	if fenced {
		return
	}

	ctx, cancel := context.WithTimeout(sess.ctx, ownershipTimeout)
	renewed, err := w.cfg.Relay.Owners.Renew(ctx, lease, w.cfg.Relay.LeaseTTL)
	cancel()
	sess.mu.Lock()
	if err == nil {
		sess.lease = renewed
	}

	lost := errors.Is(err, sessionstore.ErrLeaseLost) && !sess.fenced.Load()
	if lost {
		sess.fenced.Store(true)
	}
	sess.mu.Unlock()
	if lost {
		w.log.Warnf("session %s: lease lost; fenced", sess.id)
		sess.close()
	} else if err != nil && sess.ctx.Err() == nil {
		w.log.Warnf("session %s: renew lease: %v", sess.id, err)
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

// StartHeartbeats connects this worker to the control plane once registered.
// Calling it twice or after Close is harmless. Closing/killing the worker
// stops heartbeats alongside lease renewal.
func (w *Worker) StartHeartbeats(receiver Heartbeats) {
	w.mu.Lock()
	if w.closed || w.heartbeatStarted {
		w.mu.Unlock()
		return
	}
	w.heartbeatStarted = true
	w.running.Add(1)
	w.mu.Unlock()
	w.heartbeat(receiver)
	go func() {
		defer w.running.Done()
		ticker := time.NewTicker(w.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.stopRenew:
				return
			case <-ticker.C:
				if !w.paused.Load() {
					w.heartbeat(receiver)
				}
			}
		}
	}()
}

// kill is reachable only through the internal harness probe. Close the
// snapshot writers first, then close the socket and silently discard every
// session. No export, final put,
// release or close_notify reaches the caller, as with process death.
func (w *Worker) kill() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.stopRenew)
	sessions := make([]*session, 0, len(w.sessions))
	for _, s := range w.sessions {
		sessions = append(sessions, s)
	}
	w.mu.Unlock()
	for _, s := range sessions {
		s.fenced.Store(true)
		s.cancel() // releases gates and cancels in-flight puts before socket close
	}
	// Synchronize with each writer so no put can slip past socket shutdown.
	for _, s := range sessions {
		s.snapshotMu.Lock()
		s.snapshotMu.Unlock() //nolint:staticcheck // Synchronize with a cancelled writer; no mutation is needed.
	}
	err := w.conn.Close()
	for _, s := range sessions {
		s.close()
	}
	w.running.Wait()
	<-w.readDone
	workerprobe.Remove(w.localAddr)
	return err
}

// heartbeat performs the rejoin acknowledgment before becoming selectable.
// A returning worker's old sessions may not have observed their lost leases
// yet. Fence them synchronously; a subsequent move back to this address must
// never encounter their old contexts or errSessionExists.
func (w *Worker) heartbeat(receiver Heartbeats) {
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if closed {
		return
	}
	if !errors.Is(receiver.Heartbeat(w.localAddr), ErrRejoinRequired) {
		return
	}
	w.mu.Lock()
	sessions := make([]*session, 0, len(w.sessions))
	for _, s := range w.sessions {
		sessions = append(sessions, s)
	}
	w.mu.Unlock()
	for _, s := range sessions {
		s.fenced.Store(true)
		s.cancel()
		s.close()
	}
	w.mu.Lock()
	closed = w.closed
	w.mu.Unlock()
	if !closed {
		_ = receiver.Heartbeat(w.localAddr)
	}
}

func (c Config) cacheEnabled() bool { return c.FrameCache != nil && !c.DisableFrameCache }
