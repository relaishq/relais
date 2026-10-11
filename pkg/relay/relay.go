// Package relay is the relay: the one public UDP address in front of the
// media workers. Every SDP answer advertises the relay's public address as
// its single host candidate, so callers send everything there, and the
// relay sends each caller's packets to the media worker that owns the
// caller's session.
//
// The relay reads only STUN. A STUN binding request carries the ICE
// USERNAME "worker ufrag:caller ufrag", and the worker's ufrag is the
// session ID, so the relay looks the session's owner up in the session-owner
// store (sessionstore.Owners) and admits the caller's address to its flow
// table. Every other packet (DTLS, SRTP, SRTCP, or anything else) is
// forwarded untouched to the worker the flow table names for the address it
// came from; the relay never looks past the bytes that tell STUN apart from
// the rest (RFC 7983). A packet from an address without a confirmed route is
// dropped.
//
// Except for a trusted MoveSession, routing changes only after the worker
// authenticates a caller's binding request (see flows.go). The relay cannot
// check ICE credentials, so a binding request it routes by the store only
// proposes a pending candidate for the caller, which carries that session's
// binding requests and nothing else; the caller's confirmed route, if any,
// is untouched. The candidate becomes the confirmed route when its worker
// sends the caller a STUN binding success for one of those requests, which
// the worker does only after checking the credentials. The relay reads STUN
// headers on the worker leg for that, and nothing else.
//
// A confirmed route sticks to its session while the session is active: its
// worker has answered one of the session's own binding requests from the
// caller that arrived within Config.RouteStickinessWindow. Until then the
// relay drops a binding request for another session from that address,
// even one with valid credentials, so spoofing an active caller's address
// cannot take its media. Media and unanswered requests keep nothing
// active. The session itself may always move, and MoveSession and
// ForgetSession are unaffected.
//
// A planned move holds caller packets before the old worker exports. A
// private-leg barrier drains what the old worker already received. After
// transfer, reroute and resume, ReleaseSession forwards the bounded queue
// in arrival order. A short BarrierTimeout aborts before export if the source
// never acknowledges; the longer HoldTimeout backstop starts after that
// acknowledgement. Either timeout releases to the current route. Overflow is
// dropped and counted. Worker-to-caller traffic is never held.
//
// Store lookups run off the packet path (see lookups.go): a binding request
// on an established route is forwarded at once, and the lookup only checks
// that the owner has not changed. Lookups, their queues and the flow table
// are all bounded.
//
// Media workers bind only private sockets. They reach callers through the
// relay: the relay leg (see header.go) carries each packet with the caller's
// address, so callers only ever see the relay's address. The relay accepts
// relay-leg datagrams only from registered workers (Config.Workers,
// AddWorker), and sends a worker's packet only to a caller whose confirmed
// route is that worker, or, for the binding success that confirms it, whose
// candidate is.
//
// Optional persisted routing evidence is restored before packet loops start.
// It retains the authenticated consent time and uses the current lease owner;
// the relay holds no media keys or resumable snapshots. See persistence.go.
package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v4"

	"github.com/relais/internal/relayleg"
	"github.com/relais/pkg/sessionstore"
)

// Defaults for Config.
const (
	// DefaultFlowTimeout drops a confirmed flow when its caller has sent
	// nothing for RFC 7675's consent timeout: by then the worker has given
	// up on the session too.
	DefaultFlowTimeout = 30 * time.Second

	// DefaultRouteStickinessWindow is the workers' consent timeout (RFC
	// 7675): a route sticks to its session for as long as the session's
	// worker keeps the call alive without a fresh consent check.
	DefaultRouteStickinessWindow = 30 * time.Second

	// DefaultPendingFlowTimeout drops a pending candidate its worker has not
	// confirmed. A worker answers a valid binding request within
	// milliseconds.
	DefaultPendingFlowTimeout = 3 * time.Second

	DefaultMaxFlows        = 65536
	DefaultMaxPendingFlows = 4096

	DefaultOwnerLookups         = 16
	DefaultMaxQueuedLookups     = 1024
	DefaultMaxQueuedLookupBytes = 1 << 20
	DefaultOwnerLookupTimeout   = time.Second

	// DefaultBarrierTimeout aborts a missing drain acknowledgement after one
	// second: enough scheduling headroom on the private leg, while staying
	// below the multi-second ICE connectivity failure window. No export or
	// ownership change occurs during this wait.
	DefaultBarrierTimeout = time.Second

	// DefaultHoldTimeout bounds abandoned cross-process coordination below
	// the shortest (5 s) caller connectivity window. Healthy loopback moves
	// finish in milliseconds. Slow coordination and worst-case rollback can
	// exceed 3 s; expiry bounds buffering, not successful recovery.
	DefaultHoldTimeout       = 3 * time.Second
	DefaultMaxHeldSessions   = 1024
	DefaultMaxHeldPackets    = 256
	DefaultMaxHeldBytes      = 1 << 20
	DefaultMaxTotalHeldBytes = 8 << 20
)

const (
	// maxDatagram is the largest UDP payload.
	maxDatagram = 65535

	// socketBufferSize is requested for both relay sockets (best effort):
	// every call on the relay shares them.
	socketBufferSize = 4 << 20
)

// Config configures a relay.
type Config struct {
	// InstanceID links private status to the fenced lease holder. Empty creates
	// a fresh token for embedded relays.
	InstanceID string
	// ForwardingAllowed is a nonblocking tenure check on every packet send.
	ForwardingAllowed func() bool
	Continuity        *ContinuityStatus

	// Routes enables asynchronous confirmed-route persistence and eager restore.
	// Nil preserves the address-only relay API for embedders.
	Routes sessionstore.Routes
	// DisableRouteRestore measures the consent-check recovery baseline. Writes
	// remain enabled so the comparison changes only restart restoration.
	DisableRouteRestore bool
	// RouteRestoreTimeout bounds startup store work (default 5 seconds).
	RouteRestoreTimeout time.Duration

	// PublicAddr is the relay's public UDP address: callers send everything
	// there, and every answer advertises it as the single host candidate, so
	// the IP must be specific (not 0.0.0.0 or ::) and reachable by callers.
	// Defaults to "127.0.0.1:0", which suits in-process callers.
	PublicAddr string

	// WorkerAddr is the relay's private UDP address for the relay leg: media
	// workers send to it and receive from it. Defaults to "127.0.0.1:0".
	WorkerAddr string

	// Workers are the media workers' relay-leg addresses (their private
	// socket addresses). The relay accepts relay-leg datagrams only from
	// registered workers; AddWorker registers more.
	Workers []netip.AddrPort

	// Owners is the session-owner store the relay routes STUN binding
	// requests by. Required.
	Owners sessionstore.Owners

	// FlowTimeout drops a confirmed route after its caller has sent nothing
	// for this long, though never while the route is active (see
	// RouteStickinessWindow); PendingFlowTimeout drops a pending candidate
	// its worker has not confirmed. Default DefaultFlowTimeout and
	// DefaultPendingFlowTimeout.
	FlowTimeout        time.Duration
	PendingFlowTimeout time.Duration

	// RouteStickinessWindow keeps a confirmed route on its session for this
	// long after the arrival of the last of the session's binding requests
	// from the caller that its worker answered: until then, no other
	// session can take the caller's address. Media and unanswered requests
	// do not extend it. It is at most FlowTimeout, and a longer value is
	// clamped to it: otherwise idle routes would outlive FlowTimeout and the
	// expiry scan would walk them. Default DefaultRouteStickinessWindow (or
	// FlowTimeout, if shorter); there is no way to turn it off.
	RouteStickinessWindow time.Duration

	// MaxFlows bounds confirmed routes and pending candidates together, and
	// MaxPendingFlows the candidates. Default DefaultMaxFlows and
	// DefaultMaxPendingFlows.
	MaxFlows        int
	MaxPendingFlows int

	// OwnerLookups is how many store lookups run at once; MaxQueuedLookups
	// bounds the sessions waiting for or undergoing one, and
	// MaxQueuedLookupBytes the binding requests copied while they wait;
	// OwnerLookupTimeout bounds each lookup. Default DefaultOwnerLookups,
	// DefaultMaxQueuedLookups, DefaultMaxQueuedLookupBytes and
	// DefaultOwnerLookupTimeout.
	OwnerLookups         int
	MaxQueuedLookups     int
	MaxQueuedLookupBytes int
	OwnerLookupTimeout   time.Duration

	// BarrierTimeout bounds the pre-export drain acknowledgement wait. On
	// expiry the move aborts and held packets replay to the current route.
	// Defaults to DefaultBarrierTimeout.
	BarrierTimeout time.Duration

	// HoldTimeout is the coordination backstop after acknowledgement; every
	// normal move exit releases explicitly. Defaults to DefaultHoldTimeout.
	// The packet and byte limits apply per session; total bytes and session count
	// bound all concurrent holds. Overflow drops the newest caller packet.
	HoldTimeout       time.Duration
	MaxHeldSessions   int
	MaxHeldPackets    int
	MaxHeldBytes      int
	MaxTotalHeldBytes int

	// LoggerFactory defaults to Pion's default logger factory.
	LoggerFactory logging.LoggerFactory

	// beforeApply, when set, runs after a session's owner has been read and
	// before the result is applied. Tests use it to hold a result back.
	beforeApply func(sessionID string)
}

// Stats counts what the relay has done since it started.
type Stats struct {
	SelfFences uint64
	Workers    int

	RoutesRestored       uint64
	RoutesRestoreSkipped uint64
	RoutesRestoreFailed  uint64
	RouteWrites          uint64
	RouteWritesDropped   uint64
	RouteWritesFailed    uint64

	// Holds, HeldPackets and HeldBytes are the current bounded queues.
	Holds       int
	HeldPackets int
	HeldBytes   int
	// BarrierTimeouts counts pre-export acknowledgement failures.
	BarrierTimeouts uint64
	// HoldDrops counts overflow, HoldTimeouts post-acknowledgement release, and
	// HoldSendFailures queued packets that could not be forwarded.
	HoldDrops        uint64
	HoldTimeouts     uint64
	HoldSendFailures uint64
	// CallerPackets counts caller packets forwarded to a worker, and
	// WorkerPackets worker packets forwarded to a caller.
	CallerPackets uint64
	WorkerPackets uint64

	// STUNRouted counts binding requests forwarded to a session's owner.
	STUNRouted uint64

	// Binding requests dropped: UnknownSession for a session the store has
	// no owner for, LookupsDropped because owner lookups were saturated (or
	// the request was too large to hold), LookupsFailed because the store
	// lookup failed, and FlowsRejected because confirmed routes filled the
	// flow table or the caller's route is active for another session.
	// Unroutable counts other caller packets dropped for want of a confirmed
	// route.
	UnknownSession uint64
	LookupsDropped uint64
	LookupsFailed  uint64
	FlowsRejected  uint64
	Unroutable     uint64

	// Relay-leg datagrams dropped: UnknownWorker from an unregistered
	// address, WorkerNoFlow for a caller the sending worker holds neither the
	// confirmed route of nor a candidate its binding success confirms, and
	// Malformed for a bad header.
	UnknownWorker uint64
	WorkerNoFlow  uint64
	Malformed     uint64

	// Flows counts confirmed routes now and PendingFlows pending candidates.
	// FlowsPromoted counts candidates their worker confirmed, and
	// FlowsEvicted candidates the limits evicted.
	Flows         int
	PendingFlows  int
	FlowsPromoted uint64
	FlowsEvicted  uint64
}

// Relay is a running relay.
type Relay struct {
	instance        string
	workerForwardMu sync.Mutex
	stopOnce        sync.Once
	selfFences      atomic.Uint64
	cfg             Config
	log             logging.LeveledLogger
	public          *net.UDPConn
	workers         *net.UDPConn
	publicAddr      netip.AddrPort
	workerAddr      netip.AddrPort
	flows           *flowTable
	lookups         *ownerLookups
	persistence     *routeWrites

	// routeMu serializes generation validation with route application only.
	routeMu sync.Mutex

	// forwardMu orders caller sends, holds and private drain barriers. No
	// socket write takes routeMu. Queued releases precede new caller sends.
	forwardMu        sync.Mutex
	holds            map[string]*sessionHold
	heldPackets      int
	heldBytes        int
	holdDrops        atomic.Uint64
	barrierTimeouts  atomic.Uint64
	holdTimeouts     atomic.Uint64
	holdSendFailures atomic.Uint64

	registryMu sync.RWMutex
	registry   map[netip.AddrPort]struct{}

	ctx       context.Context
	cancel    context.CancelFunc
	running   sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	routesRestored       atomic.Uint64
	routesRestoreSkipped atomic.Uint64
	routesRestoreFailed  atomic.Uint64
	routeWritesDone      atomic.Uint64
	routeWritesDropped   atomic.Uint64
	routeWritesFailed    atomic.Uint64
	callerPackets        atomic.Uint64
	workerPackets        atomic.Uint64
	stunRouted           atomic.Uint64
	unknownSession       atomic.Uint64
	lookupsDropped       atomic.Uint64
	lookupsFailed        atomic.Uint64
	unroutable           atomic.Uint64
	unknownWorker        atomic.Uint64
	workerNoFlow         atomic.Uint64
	malformed            atomic.Uint64
}

// New starts a relay.
func New(cfg Config) (*Relay, error) {
	if cfg.Owners == nil {
		return nil, errors.New("relay: Config.Owners is required")
	}
	applyDefaults(&cfg)

	public, publicAddr, err := listen("public", cfg.PublicAddr)
	if err != nil {
		return nil, err
	}
	workers, workerAddr, err := listen("worker", cfg.WorkerAddr)
	if err != nil {
		_ = public.Close()

		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &Relay{
		instance:   cfg.InstanceID,
		cfg:        cfg,
		log:        cfg.LoggerFactory.NewLogger("relay"),
		public:     public,
		workers:    workers,
		publicAddr: publicAddr,
		workerAddr: workerAddr,
		flows: newFlowTable(flowLimits{
			stickinessWindow: cfg.RouteStickinessWindow,
			idleTimeout:      cfg.FlowTimeout,
			pendingTimeout:   cfg.PendingFlowTimeout,
			maxFlows:         cfg.MaxFlows,
			maxPending:       cfg.MaxPendingFlows,
		}),
		registry: make(map[netip.AddrPort]struct{}),
		holds:    make(map[string]*sessionHold),
		ctx:      ctx,
		cancel:   cancel,
	}
	if r.instance == "" {
		r.instance = rand.Text()
	}
	for _, worker := range cfg.Workers {
		r.AddWorker(worker)
	}
	r.restoreRoutes()
	r.lookups = newOwnerLookups(r, cfg)
	if cfg.Routes != nil {
		r.persistence = newRouteWrites(r)
	}
	r.running.Add(3)
	go r.callerLoop()
	go r.workerLoop()
	go r.sweepFlows()

	return r, nil
}

func applyDefaults(cfg *Config) {
	if cfg.RouteRestoreTimeout <= 0 {
		cfg.RouteRestoreTimeout = 5 * time.Second
	}
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = "127.0.0.1:0"
	}
	if cfg.WorkerAddr == "" {
		cfg.WorkerAddr = "127.0.0.1:0"
	}
	if cfg.RouteStickinessWindow <= 0 {
		cfg.RouteStickinessWindow = DefaultRouteStickinessWindow
	}
	if cfg.FlowTimeout <= 0 {
		cfg.FlowTimeout = DefaultFlowTimeout
	}
	// Stickiness never outlasts the idle timeout (see RouteStickinessWindow).
	cfg.RouteStickinessWindow = min(cfg.RouteStickinessWindow, cfg.FlowTimeout)
	if cfg.PendingFlowTimeout <= 0 {
		cfg.PendingFlowTimeout = DefaultPendingFlowTimeout
	}
	if cfg.MaxFlows <= 0 {
		cfg.MaxFlows = DefaultMaxFlows
	}
	if cfg.MaxPendingFlows <= 0 {
		cfg.MaxPendingFlows = DefaultMaxPendingFlows
	}
	if cfg.OwnerLookups <= 0 {
		cfg.OwnerLookups = DefaultOwnerLookups
	}
	if cfg.MaxQueuedLookups <= 0 {
		cfg.MaxQueuedLookups = DefaultMaxQueuedLookups
	}
	if cfg.MaxQueuedLookupBytes <= 0 {
		cfg.MaxQueuedLookupBytes = DefaultMaxQueuedLookupBytes
	}
	if cfg.OwnerLookupTimeout <= 0 {
		cfg.OwnerLookupTimeout = DefaultOwnerLookupTimeout
	}
	if cfg.BarrierTimeout <= 0 {
		cfg.BarrierTimeout = DefaultBarrierTimeout
	}
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = DefaultHoldTimeout
	}
	if cfg.MaxHeldSessions <= 0 {
		cfg.MaxHeldSessions = DefaultMaxHeldSessions
	}
	if cfg.MaxHeldPackets <= 0 {
		cfg.MaxHeldPackets = DefaultMaxHeldPackets
	}
	if cfg.MaxHeldBytes <= 0 {
		cfg.MaxHeldBytes = DefaultMaxHeldBytes
	}
	if cfg.MaxTotalHeldBytes <= 0 {
		cfg.MaxTotalHeldBytes = DefaultMaxTotalHeldBytes
	}
	if cfg.LoggerFactory == nil {
		cfg.LoggerFactory = logging.NewDefaultLoggerFactory()
	}
}

func listen(name, addr string) (*net.UDPConn, netip.AddrPort, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, netip.AddrPort{}, fmt.Errorf("relay: resolve %s address: %w", name, err)
	}
	if udpAddr.IP == nil || udpAddr.IP.IsUnspecified() {
		return nil, netip.AddrPort{}, fmt.Errorf("relay: %s address %q must name a specific IP", name, addr)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, netip.AddrPort{}, fmt.Errorf("relay: listen on %s address: %w", name, err)
	}
	_ = conn.SetReadBuffer(socketBufferSize)
	_ = conn.SetWriteBuffer(socketBufferSize)

	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()

		return nil, netip.AddrPort{}, fmt.Errorf("relay: unexpected local address %T", conn.LocalAddr())
	}

	return conn, unmap(local.AddrPort()), nil
}

// PublicAddr is the relay's public UDP address: the single host candidate
// every answer advertises.
func (r *Relay) PublicAddr() netip.AddrPort {
	return r.publicAddr
}

// WorkerAddr is the relay's private relay-leg address, which media workers
// send to.
func (r *Relay) WorkerAddr() netip.AddrPort {
	return r.workerAddr
}

// AddWorker registers a media worker's relay-leg address, so the relay
// accepts its datagrams.
func (r *Relay) AddWorker(worker netip.AddrPort) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	r.registry[unmap(worker)] = struct{}{}
}

// MoveSession is a trusted control-plane notification after a lease transfer.
// It immediately fences the old relay leg, preserves all confirmed callers,
// and invalidates lookups that started before the move. It admits no caller
// addresses: those still require the worker's authenticated binding answer.
// A zero from repairs an uncertain prior route, re-pointing every session route.
func (r *Relay) MoveSession(sessionID string, from, to netip.AddrPort) error {
	from, to = unmap(from), unmap(to)
	if sessionID == "" || from.IsValid() && !r.registered(from) || !r.registered(to) {
		return errors.New("relay: move needs a session and registered workers")
	}
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	if r.ctx.Err() != nil {
		return errors.New("relay: closed")
	}
	r.lookups.mu.Lock()
	if lookup := r.lookups.pending[sessionID]; lookup != nil {
		lookup.generation++
	}
	r.lookups.mu.Unlock()
	r.flows.moveSession(sessionID, from, to)
	for _, caller := range r.flows.sessionCallers(sessionID) {
		r.persist(caller, true)
	}
	if h := r.holds[sessionID]; h != nil {
		h.worker = to
	}
	return nil
}

// RemoveWorker stops accepting a media worker's datagrams.
func (r *Relay) RemoveWorker(worker netip.AddrPort) {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	delete(r.registry, unmap(worker))
}

func (r *Relay) registered(worker netip.AddrPort) bool {
	r.registryMu.RLock()
	defer r.registryMu.RUnlock()
	_, ok := r.registry[worker]

	return ok
}

// Stats returns the relay's counters.
func (r *Relay) Stats() Stats {
	flows := r.flows.counts()
	r.forwardMu.Lock()
	holds, packets, bytes := len(r.holds), r.heldPackets, r.heldBytes
	r.forwardMu.Unlock()

	r.registryMu.RLock()
	workers := len(r.registry)
	r.registryMu.RUnlock()
	return Stats{
		SelfFences: r.selfFences.Load(), Workers: workers,
		RoutesRestored: r.routesRestored.Load(), RoutesRestoreSkipped: r.routesRestoreSkipped.Load(), RoutesRestoreFailed: r.routesRestoreFailed.Load(), RouteWrites: r.routeWritesDone.Load(), RouteWritesDropped: r.routeWritesDropped.Load(), RouteWritesFailed: r.routeWritesFailed.Load(),
		BarrierTimeouts: r.barrierTimeouts.Load(),
		Holds:           holds, HeldPackets: packets, HeldBytes: bytes, HoldDrops: r.holdDrops.Load(), HoldTimeouts: r.holdTimeouts.Load(), HoldSendFailures: r.holdSendFailures.Load(),
		CallerPackets:  r.callerPackets.Load(),
		WorkerPackets:  r.workerPackets.Load(),
		STUNRouted:     r.stunRouted.Load(),
		UnknownSession: r.unknownSession.Load(),
		LookupsDropped: r.lookupsDropped.Load(),
		LookupsFailed:  r.lookupsFailed.Load(),
		FlowsRejected:  flows.rejected,
		Unroutable:     r.unroutable.Load(),
		UnknownWorker:  r.unknownWorker.Load(),
		WorkerNoFlow:   r.workerNoFlow.Load(),
		Malformed:      r.malformed.Load(),
		Flows:          flows.routes,
		PendingFlows:   flows.pending,
		FlowsPromoted:  flows.promoted,
		FlowsEvicted:   flows.evicted,
	}
}

// Close stops the relay and closes both of its sockets. The flow table goes
// with it; the session-owner store is not touched.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		r.stopSockets()
		r.forwardMu.Lock()
		for id, h := range r.holds {
			h.timer.Stop()
			close(h.released)
			delete(r.holds, id)
		}
		r.heldPackets, r.heldBytes = 0, 0
		r.forwardMu.Unlock()
		r.running.Wait()
	})

	return r.closeErr
}

// callerLoop reads the public socket and forwards each caller packet to its
// worker. The packet is read past room for the relay-leg header, which is
// then written in front of it in place. Nothing here waits for the store.
func (r *Relay) callerLoop() {
	defer r.running.Done()

	buf := make([]byte, MaxHeaderLen+maxDatagram)
	for {
		n, from, err := r.public.ReadFromUDPAddrPort(buf[MaxHeaderLen:])
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.Debugf("read from callers: %v", err)

			continue
		}
		from = unmap(from)

		if worker, ok := r.route(buf[MaxHeaderLen:MaxHeaderLen+n], from); ok {
			r.forward(buf[:MaxHeaderLen+n], from, worker)
		}
	}
}

// route picks the worker for a caller packet. A binding request for the
// session of the caller's candidate or route follows it, and a store lookup
// checks the owner in the background; any other binding request waits for
// its lookup (and is forwarded by the lookup, not here). Every other packet
// needs a confirmed route.
func (r *Relay) route(pkt []byte, from netip.AddrPort) (netip.AddrPort, bool) {
	now := time.Now()

	if isSTUN(pkt) {
		if sessionID, txID, ok := parseBindingRequest(pkt); ok {
			if worker, ok := r.flows.routeSTUN(from, sessionID, txID, now); ok {
				if isNomination(pkt) {
					r.flows.markNomination(from, txID)
				}
				r.stunRouted.Add(1)
				r.lookups.refresh(sessionID, from)

				return worker, true
			}
			if !r.lookups.resolve(sessionID, from, pkt, txID) {
				r.lookupsDropped.Add(1)
			}

			return netip.AddrPort{}, false
		}
	}

	worker, ok := r.flows.route(from, now)
	if !ok {
		r.unroutable.Add(1)
	}

	return worker, ok
}

// forward sends a caller packet to a worker. datagram is MaxHeaderLen bytes
// of room for the relay-leg header followed by the packet.
func (r *Relay) forward(datagram []byte, caller, worker netip.AddrPort) {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()
	packet := datagram[MaxHeaderLen:]
	var session string
	var tx [stun.TransactionIDSize]byte
	var stunRequest bool
	if isSTUN(packet) {
		session, tx, stunRequest = parseBindingRequest(packet)
	}
	if !stunRequest {
		f, ok := r.flows.forwardRoute(caller)
		if !ok {
			return
		}
		session, worker = f.session, f.worker
	}
	if h := r.holds[session]; h != nil {
		// Only authenticated callers for this session may spend its queue.
		// Other addresses' checks can retry after the hold; pending candidates
		// are insufficient, even if their USERNAME names the held session.
		confirmed, ok := r.flows.forwardRoute(caller)
		if !ok || confirmed.session != session {
			return
		}
		r.enqueue(h, caller, packet)
		return
	}
	if stunRequest {
		current, ok := r.flows.routeSTUN(caller, session, tx, time.Now())
		if !ok {
			return
		}
		if isNomination(packet) {
			r.flows.markNomination(caller, tx)
		}
		worker = current
	}
	r.sendCaller(datagram, caller, worker)
}

// sendCaller runs under forwardMu; routeMu is never held during a write.
func (r *Relay) sendCaller(datagram []byte, caller, worker netip.AddrPort) bool {
	if !r.forwardingAllowed() {
		return false
	}
	start := MaxHeaderLen - HeaderLen(caller)
	AppendHeader(datagram[start:start], caller)
	if _, err := r.workers.WriteToUDPAddrPort(datagram[start:], worker); err != nil {
		r.log.Debugf("forward to worker %s: %v", worker, err)
		return false
	}
	r.callerPackets.Add(1)
	return true
}

func (r *Relay) owner(sessionID string) (netip.AddrPort, error) {
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.OwnerLookupTimeout)
	defer cancel()

	return r.cfg.Owners.Owner(ctx, sessionID)
}

// resolved acts on a session's owner lookup for the callers waiting on it:
// it proposes each waiting binding request's caller to the owner and
// returns admitted requests for forwarding after routeMu is unlocked, or
// has an established route follow a new owner
// (through a candidate the owner must confirm).
func (r *Relay) resolved(sessionID string, owner netip.AddrPort, err error, waiters []waiter) []waiter {
	var forwards []waiter
	now := time.Now()
	switch {
	case err == nil:
		for _, w := range waiters {
			if w.datagram == nil {
				if r.flows.reroute(w.caller, owner, sessionID, now) {
					r.log.Infof("session %s: owner is now worker %s; caller %s moves once it answers",
						sessionID, owner, w.caller)
				}

				continue
			}
			if !r.flows.admit(w.caller, owner, sessionID, w.txID, now) {
				continue // counted as FlowsRejected
			}
			if isNomination(w.datagram[MaxHeaderLen:]) {
				r.flows.markNomination(w.caller, w.txID)
			}
			r.stunRouted.Add(1)
			forwards = append(forwards, w)
		}
	case errors.Is(err, sessionstore.ErrNotFound):
		for _, w := range waiters {
			if w.datagram == nil {
				r.flows.forget(w.caller, sessionID)

				continue
			}
			r.unknownSession.Add(1)
			r.log.Debugf("drop binding request from %s: no owner for session %s", w.caller, sessionID)
		}
	default:
		r.log.Warnf("session %s: look up owner: %v", sessionID, err)
		for _, w := range waiters {
			if w.datagram != nil {
				r.lookupsFailed.Add(1)
			}
		}
	}
	return forwards
}

// workerLoop reads the relay leg and sends each worker packet to the caller
// its header names, from the public socket: only from a registered worker,
// and only to a caller whose confirmed route is that worker, or whose
// candidate it confirms with a binding success.
func (r *Relay) workerLoop() {
	defer r.running.Done()

	buf := make([]byte, maxDatagram)
	for {
		n, from, err := r.workers.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.log.Debugf("read from workers: %v", err)

			continue
		}
		from = unmap(from)
		if !r.registered(from) {
			r.unknownWorker.Add(1)

			continue
		}

		if id, ack, ok := relayleg.ParseBarrier(buf[:n]); ok {
			if ack {
				r.acknowledgeBarrier(id, from)
			}
			continue
		}
		caller, pkt, err := ParseHeader(buf[:n])
		if err != nil {
			r.malformed.Add(1)
			r.log.Debugf("drop datagram from worker %s: %v", from, err)

			continue
		}
		allowed, promoted := r.flows.answer(caller, from, pkt, time.Now())
		if !allowed {
			r.workerNoFlow.Add(1)

			continue
		}
		if _, success := parseBindingSuccess(pkt); success {
			r.persist(caller, promoted != "")
		}
		if promoted != "" {
			r.log.Infof("session %s: caller %s <-> worker %s", promoted, caller, from)
		}
		r.workerForwardMu.Lock()
		if !r.forwardingAllowed() {
			r.workerForwardMu.Unlock()
			continue
		}
		_, err = r.public.WriteToUDPAddrPort(pkt, caller)
		r.workerForwardMu.Unlock()
		if err != nil {
			r.log.Debugf("forward to caller %s: %v", caller, err)

			continue
		}
		r.workerPackets.Add(1)
	}
}

func (r *Relay) sweepFlows() {
	defer r.running.Done()

	ticker := time.NewTicker(min(r.cfg.FlowTimeout, r.cfg.PendingFlowTimeout) / 2)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-ticker.C:
			r.flows.sweep(now)
		}
	}
}

// isSTUN tells STUN apart from everything else on a WebRTC transport: a STUN
// message starts with a byte from 0 to 3 (RFC 7983) and carries the magic
// cookie (RFC 5389). DTLS, RTP and RTCP start with 20-63 and 128-191, so
// they are never mistaken for STUN, whatever else they contain.
func isSTUN(pkt []byte) bool {
	return len(pkt) > 0 && pkt[0] <= 3 && stun.IsMessage(pkt)
}

// parseBindingRequest returns the session a STUN binding request is for (the
// first half of its USERNAME, which is the worker's ICE ufrag) and its
// transaction ID. Message integrity is not checked here; the worker checks
// it.
func parseBindingRequest(pkt []byte) (string, [stun.TransactionIDSize]byte, bool) {
	msg := &stun.Message{Raw: pkt}
	if err := msg.Decode(); err != nil || msg.Type != stun.BindingRequest {
		return "", msg.TransactionID, false
	}
	var username stun.Username
	if err := username.GetFrom(msg); err != nil {
		return "", msg.TransactionID, false
	}
	sessionID, _, ok := strings.Cut(username.String(), ":")

	return sessionID, msg.TransactionID, ok && sessionID != ""
}

func unmap(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

// ForgetSession fences all cached routes for a call whose lease was released.
// Pending lookups are invalidated too, so an old store answer cannot resurrect
// the lost call. Call this after releasing the lease.
func (r *Relay) ForgetSession(id string) {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	r.lookups.mu.Lock()
	if lookup := r.lookups.pending[id]; lookup != nil {
		lookup.generation++
	}
	r.lookups.mu.Unlock()
	r.flows.forgetSession(id)
	if r.persistence != nil {
		r.persistence.enqueue(id, nil)
	}
}

// Fence stops both packet paths synchronously. Close later joins background
// work; fencing itself never waits for Redis or route persistence.
func (r *Relay) Fence() {
	r.forwardMu.Lock()
	defer r.forwardMu.Unlock()
	r.workerForwardMu.Lock()
	defer r.workerForwardMu.Unlock()
	r.selfFences.Add(1)
	r.stopSockets()
}
func (r *Relay) stopSockets() {
	r.stopOnce.Do(func() { r.cancel(); r.closeErr = errors.Join(r.public.Close(), r.workers.Close()) })
}
func (r *Relay) forwardingAllowed() bool {
	return r.ctx.Err() == nil && (r.cfg.ForwardingAllowed == nil || r.cfg.ForwardingAllowed())
}
