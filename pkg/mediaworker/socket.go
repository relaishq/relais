package mediaworker

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v4"
)

// packetConn is a worker's media transport: its own UDP socket, or a port on
// a Socket shared with other workers. The worker's read loop reads one packet
// at a time and finishes processing it before it reads the next; a Socket's
// handover relies on that to drain a port (see socketPort.drain).
type packetConn interface {
	ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	Close() error
}

const (
	// portQueueSize bounds the packets waiting for a worker on a shared
	// socket, like a socket receive buffer. A full queue drops packets.
	portQueueSize = 4096

	// maxHeldPackets bounds the caller packets a socket holds for a session
	// while it moves between workers.
	maxHeldPackets = 4096

	// drainTimeout bounds how long a handover waits for the old owner to
	// finish the packets it was given before the move.
	drainTimeout = 2 * time.Second
)

var (
	// ErrHandoverInProgress is returned when a session is already moving.
	ErrHandoverInProgress = errors.New("mediaworker: session is already being handed over")

	errNotOnSocket = errors.New("mediaworker: worker is not on this socket")
	errSameOwner   = errors.New("mediaworker: session is already owned by that worker")
	errDrain       = errors.New("mediaworker: old owner did not finish its packets in time")
)

// SocketConfig configures a shared media socket.
type SocketConfig struct {
	// ListenAddr is the socket's local UDP address; see Config.ListenAddr.
	// Defaults to "127.0.0.1:0".
	ListenAddr string

	// LoggerFactory defaults to Pion's default logger factory.
	LoggerFactory logging.LoggerFactory
}

// Socket is a UDP media socket shared by several media workers: the socket
// owner. Its address is the single host candidate in every answer the workers
// give, so a caller never sees which worker serves it, and a session can move
// between the workers without the caller's address, keys or connection
// changing (Handover).
//
// The socket routes like the relay in issue #5 will: a STUN binding request
// goes to the worker that owns the session named by the worker half of its
// USERNAME (the ICE ufrag is the session ID). When that worker has
// authenticated the request (MESSAGE-INTEGRITY with the session's ICE
// password), the request's source address becomes a flow of the session.
// Every other packet goes to the owner of its source address's flow,
// unparsed. A request that fails authentication changes no flow, so it
// cannot redirect an established caller's media. A worker may send only to
// flows of sessions it owns; anything else is dropped. That is the fence that
// keeps a session's old owner from reaching the caller after a move.
type Socket struct {
	conn      *net.UDPConn
	localAddr netip.AddrPort
	log       logging.LeveledLogger
	readDone  chan struct{}

	// mu guards the routing tables. Writes to the UDP socket hold it for
	// reading, so a route change waits for writes in progress: once it
	// returns, the old owner has no packet on its way out.
	mu     sync.RWMutex
	ports  map[*socketPort]struct{}
	routes map[string]*route // by session ID
	flows  map[netip.AddrPort]string
	closed bool
}

// route is where a session's packets go.
type route struct {
	owner *socketPort // nil while the session is between owners
	held  bool        // a handover holds the caller's packets in queue
	queue []portPacket
}

// ListenSocket opens a shared media socket.
func ListenSocket(cfg SocketConfig) (*Socket, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:0"
	}
	if cfg.LoggerFactory == nil {
		cfg.LoggerFactory = logging.NewDefaultLoggerFactory()
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
	local := udpAddr.AddrPort()

	s := &Socket{
		conn:      conn,
		localAddr: netip.AddrPortFrom(local.Addr().Unmap(), local.Port()),
		log:       cfg.LoggerFactory.NewLogger("mediasocket"),
		readDone:  make(chan struct{}),
		ports:     make(map[*socketPort]struct{}),
		routes:    make(map[string]*route),
		flows:     make(map[netip.AddrPort]string),
	}
	go s.readLoop()

	return s, nil
}

// LocalAddr is the socket's address, the host candidate of its workers.
func (s *Socket) LocalAddr() netip.AddrPort {
	return s.localAddr
}

// NewWorker starts a media worker on the socket. cfg.ListenAddr is ignored.
func (s *Socket) NewWorker(cfg Config) (*Worker, error) {
	cfg.socket = s
	worker, err := New(cfg)
	if err != nil {
		return nil, err
	}
	port, ok := worker.conn.(*socketPort)
	if !ok {
		_ = worker.Close()

		return nil, errNotOnSocket
	}
	s.mu.Lock()
	port.worker = worker
	s.mu.Unlock()

	return worker, nil
}

// Owner returns the worker that owns a session, or nil (unknown session, or
// one between owners during a handover).
func (s *Socket) Owner(sessionID string) *Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r := s.routes[sessionID]; r != nil && r.owner != nil {
		return r.owner.worker
	}

	return nil
}

// EndSession hangs up a session on whichever worker owns it.
func (s *Socket) EndSession(sessionID string) error {
	owner := s.Owner(sessionID)
	if owner == nil {
		return ErrUnknownSession
	}

	return owner.EndSession(sessionID)
}

// SignalingHandler is the WHIP-style signaling endpoint (see
// Worker.SignalingHandler) for the workers on the socket: offers create
// sessions on newCalls, and a hangup reaches the session's current owner.
func (s *Socket) SignalingHandler(newCalls *Worker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+CallsPath, newCalls.handleOffer)
	mux.HandleFunc("DELETE "+CallsPath+"/{id}", func(rw http.ResponseWriter, r *http.Request) {
		if err := s.EndSession(r.PathValue("id")); err != nil {
			http.Error(rw, err.Error(), http.StatusNotFound)

			return
		}
		rw.WriteHeader(http.StatusOK)
	})

	return mux
}

// Close closes the socket. Close its workers first: their sessions send
// close_notify through it.
func (s *Socket) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return nil
	}
	s.closed = true
	ports := make([]*socketPort, 0, len(s.ports))
	for port := range s.ports {
		ports = append(ports, port)
	}
	s.mu.Unlock()

	for _, port := range ports {
		_ = port.Close()
	}
	err := s.conn.Close()
	<-s.readDone

	return err
}

func (s *Socket) attach() (*socketPort, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	port := &socketPort{
		socket:  s,
		packets: make(chan portPacket, portQueueSize),
		closed:  make(chan struct{}),
	}
	s.ports[port] = struct{}{}

	return port, nil
}

func (s *Socket) detach(port *socketPort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ports, port)
}

// claim routes a session to port unless it already has a route (for example,
// a handover in progress that will route it).
func (s *Socket) claim(sessionID string, port *socketPort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.routes[sessionID]; !ok && !s.closed {
		s.routes[sessionID] = &route{owner: port}
	}
}

// release forgets a session that port owned and has ended. A session that
// is being handed over keeps its route: the handover decides where it goes.
func (s *Socket) release(sessionID string, port *socketPort) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.routes[sessionID]
	if r == nil || r.owner != port || r.held {
		return
	}
	delete(s.routes, sessionID)
	for addr, id := range s.flows {
		if id == sessionID {
			delete(s.flows, addr)
		}
	}
}

func (s *Socket) readLoop() {
	defer close(s.readDone)

	buf := make([]byte, receiveMTU)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Debugf("read: %v", err)

			continue
		}
		s.dispatch(buf[:n], netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
	}
}

// dispatch routes one packet from a caller.
func (s *Socket) dispatch(pkt []byte, from netip.AddrPort) {
	isSTUN := stun.IsMessage(pkt)
	sessionID := ""
	if isSTUN {
		sessionID = stunSessionID(pkt)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !isSTUN {
		sessionID = s.flows[from]
	}
	r := s.routes[sessionID]
	if r == nil {
		return
	}
	// A STUN request changes no flow here: the owner records one only once
	// it has authenticated the request (learnFlow).

	packet := portPacket{data: bytes.Clone(pkt), from: from}
	switch {
	case r.held:
		if len(r.queue) < maxHeldPackets {
			r.queue = append(r.queue, packet)
		}
	case r.owner != nil:
		r.owner.deliver(packet)
	}
}

// learnFlow makes from a flow of a session after the session's owner, on
// port, has authenticated an ICE check from that address. Only the current
// owner can: a worker that is not (or no longer) the owner changes nothing.
func (s *Socket) learnFlow(port *socketPort, from netip.AddrPort, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.routes[sessionID]; r != nil && r.owner == port {
		s.flows[from] = sessionID
	}
}

// stunSessionID returns the session a STUN binding request is for: the
// worker's ufrag, the part of USERNAME before the colon.
func stunSessionID(raw []byte) string {
	msg := &stun.Message{Raw: append([]byte(nil), raw...)}
	if err := msg.Decode(); err != nil || msg.Type != stun.BindingRequest {
		return ""
	}
	var username stun.Username
	if err := username.GetFrom(msg); err != nil {
		return ""
	}
	local, _, ok := strings.Cut(username.String(), ":")
	if !ok {
		return ""
	}

	return local
}

// write sends a worker's packet to a caller if the worker owns the session
// of that caller's flow. Holding mu for reading makes the check and the
// write one step with respect to route changes.
func (s *Socket) write(port *socketPort, pkt []byte, to netip.AddrPort) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if r := s.routes[s.flows[to]]; r == nil || r.owner != port {
		// Fenced: the sender does not own this caller (any more). Dropped
		// like a lost datagram, so a DTLS close_notify fails silently too.
		return len(pkt), nil
	}

	return s.conn.WriteToUDPAddrPort(pkt, to)
}

// HandoverResult describes a completed planned handover.
type HandoverResult struct {
	SessionID string

	// StateBytes is the size of the exported session state, the only thing
	// that passed from the old worker to the new one.
	StateBytes int

	// HeldPackets counts caller packets that arrived during the move. The
	// socket held them and delivered them to the new owner, in order.
	HeldPackets int

	// RolledBack is set when the new owner could not resume the session
	// and the old owner resumed it from the same bytes instead. Handover
	// then returns an error, but the call goes on.
	RolledBack bool

	// Duration runs from holding the caller's packets to delivering them to
	// the new owner. Drain, Export and Resume are its parts: the old owner
	// finishing the packets it already had, its export of the session, and
	// the new owner's resume.
	Duration time.Duration
	Drain    time.Duration
	Export   time.Duration
	Resume   time.Duration
}

// Handover moves a live session to another worker on this socket, as a
// planned handover. Only the exported bytes pass between the workers:
//
//  1. Hold: the socket stops delivering the session's packets and queues
//     them instead.
//  2. Drain: the old owner finishes the packets it was already given.
//  3. Export: the old owner freezes the session, snapshots it (atomically
//     with its counters) and drops it without telling the caller.
//  4. Fence: the socket stops accepting the old owner's packets for the
//     caller.
//  5. Resume: the new owner rebuilds the session from the bytes.
//  6. Route: the socket delivers the held packets, then everything else, to
//     the new owner.
//
// No caller packet is processed by both workers. If the new owner cannot
// resume the session, the old owner resumes it from the same bytes.
func (s *Socket) Handover(sessionID string, to *Worker, opts ResumeOptions) (HandoverResult, error) {
	start := time.Now()
	result := HandoverResult{SessionID: sessionID}

	toPort, ok := to.conn.(*socketPort)
	if !ok || toPort.socket != s {
		return result, errNotOnSocket
	}
	from, err := s.hold(sessionID, toPort)
	if err != nil {
		return result, err
	}

	if err := from.drain(drainTimeout); err != nil {
		s.route(sessionID, from)

		return result, err
	}
	drained := time.Now()

	state, err := from.worker.ExportSession(sessionID)
	if errors.Is(err, ErrUnknownSession) {
		s.drop(sessionID) // the call ended while the handover held it

		return result, err
	}
	if err != nil {
		s.route(sessionID, from)

		return result, err
	}
	exported := time.Now()
	s.route(sessionID, nil)

	result.StateBytes = len(state)
	if _, err := to.ResumeSession(state, opts); err != nil {
		if _, rollbackErr := from.worker.ResumeSession(state, ResumeOptions{}); rollbackErr != nil {
			s.drop(sessionID)

			return result, errors.Join(err, fmt.Errorf("mediaworker: roll back to the old owner: %w", rollbackErr))
		}
		result.RolledBack = true
		result.HeldPackets = s.route(sessionID, from)

		return result, fmt.Errorf("mediaworker: new owner could not resume the session, old owner resumed it: %w", err)
	}
	resumed := time.Now()
	result.HeldPackets = s.route(sessionID, toPort)

	result.Duration = time.Since(start)
	result.Drain = drained.Sub(start)
	result.Export = exported.Sub(drained)
	result.Resume = resumed.Sub(exported)
	s.log.Infof("session %s: handed over in %s (state %d bytes, %d packets held)",
		sessionID, result.Duration, result.StateBytes, result.HeldPackets)

	return result, nil
}

// hold starts a handover: the session's packets are queued from now on.
func (s *Socket) hold(sessionID string, to *socketPort) (*socketPort, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.routes[sessionID]
	switch {
	case r == nil:
		return nil, ErrUnknownSession
	case r.held || r.owner == nil:
		return nil, ErrHandoverInProgress
	case r.owner == to:
		return nil, errSameOwner
	case r.owner.worker == nil:
		return nil, errNotOnSocket
	}
	r.held = true

	return r.owner, nil
}

// route gives a session to owner and, unless owner is nil, ends the hold:
// the held packets go to owner first, in order. It returns how many there
// were.
func (s *Socket) route(sessionID string, owner *socketPort) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.routes[sessionID]
	if r == nil {
		return 0
	}
	r.owner = owner
	if owner == nil {
		return 0
	}
	held := len(r.queue)
	for _, packet := range r.queue {
		owner.deliver(packet)
	}
	r.queue = nil
	r.held = false

	return held
}

// drop forgets a session that no worker owns any more.
func (s *Socket) drop(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.routes, sessionID)
	for addr, id := range s.flows {
		if id == sessionID {
			delete(s.flows, addr)
		}
	}
}

// socketPort is one worker's view of a shared Socket. It implements
// packetConn: reads return the packets the socket routes to the worker, and
// writes go out through the socket's fence.
type socketPort struct {
	socket    *Socket
	worker    *Worker // set by Socket.NewWorker; guarded by socket.mu
	packets   chan portPacket
	closed    chan struct{}
	closeOnce sync.Once
}

// portPacket is a caller packet for a worker, or a drain barrier.
type portPacket struct {
	data    []byte
	from    netip.AddrPort
	barrier chan struct{}
}

// deliver queues a packet for the worker. It never blocks: a full queue
// drops the packet, as a full socket buffer would.
func (p *socketPort) deliver(packet portPacket) {
	select {
	case p.packets <- packet:
	default:
	}
}

// drain waits until the worker has processed every packet delivered to it
// before the call. It queues a barrier behind them; the worker's read loop
// reaches it only after it has finished the packet before it.
func (p *socketPort) drain(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	done := make(chan struct{})
	select {
	case p.packets <- portPacket{barrier: done}:
	case <-p.closed:
		return ErrClosed
	case <-timer.C:
		return errDrain
	}
	select {
	case <-done:
		return nil
	case <-p.closed:
		return ErrClosed
	case <-timer.C:
		return errDrain
	}
}

func (p *socketPort) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	for {
		select {
		case packet := <-p.packets:
			if packet.barrier != nil {
				close(packet.barrier)

				continue
			}

			return copy(b, packet.data), packet.from, nil
		case <-p.closed:
			return 0, netip.AddrPort{}, net.ErrClosed
		}
	}
}

func (p *socketPort) WriteToUDPAddrPort(b []byte, to netip.AddrPort) (int, error) {
	select {
	case <-p.closed:
		return 0, net.ErrClosed
	default:
	}

	return p.socket.write(p, b, to)
}

// Close detaches the port from the socket; the socket stays open.
func (p *socketPort) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.socket.detach(p)
	})

	return nil
}

// claimSession routes a new or resumed session to this worker when the
// worker is on a shared socket.
func (w *Worker) claimSession(sessionID string) {
	if port, ok := w.conn.(*socketPort); ok {
		port.socket.claim(sessionID, port)
	}
}

// learnFlow tells a shared socket that an ICE check from from has
// authenticated for one of this worker's sessions.
func (w *Worker) learnFlow(from netip.AddrPort, sessionID string) {
	if port, ok := w.conn.(*socketPort); ok {
		port.socket.learnFlow(port, from, sessionID)
	}
}

// releaseSession removes an ended session's route when the worker is on a
// shared socket, unless the worker still runs a session with that ID (a
// duplicate that failed to register is ending, not the live one).
func (w *Worker) releaseSession(sessionID string) {
	if port, ok := w.conn.(*socketPort); ok && w.session(sessionID) == nil {
		port.socket.release(sessionID, port)
	}
}
