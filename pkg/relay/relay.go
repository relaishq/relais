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
// the rest (RFC 7983). A packet from an address without a confirmed flow is
// dropped.
//
// Store lookups run off the packet path (see lookups.go): a binding request
// on an established flow is forwarded at once, and the lookup only checks
// that the owner has not changed. A new flow is pending until its worker
// answers the caller, which the worker does only after checking the
// request's ICE credentials; the flow table is bounded (see flows.go).
//
// Media workers bind only private sockets. They reach callers through the
// relay: the relay leg (see header.go) carries each packet with the caller's
// address, so callers only ever see the relay's address. The relay accepts
// relay-leg datagrams only from registered workers (Config.Workers,
// AddWorker), and sends a worker's packet only to a caller whose flow that
// worker owns.
//
// The flow table is a cache, not state. A restarted relay starts with an
// empty table and rebuilds each flow from the store on the caller's next
// STUN binding request (ICE consent checks arrive every few seconds); until
// then the caller's packets, and the workers' packets to it, are dropped.
package relay

import (
	"context"
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

	"github.com/relais/pkg/sessionstore"
)

// Defaults for Config.
const (
	// DefaultFlowTimeout drops a confirmed flow when its caller has sent
	// nothing for RFC 7675's consent timeout: by then the worker has given
	// up on the session too.
	DefaultFlowTimeout = 30 * time.Second

	// DefaultPendingFlowTimeout drops a pending flow its worker has not
	// answered. A worker answers a valid binding request within
	// milliseconds.
	DefaultPendingFlowTimeout = 3 * time.Second

	DefaultMaxFlows        = 65536
	DefaultMaxPendingFlows = 4096

	DefaultOwnerLookups       = 16
	DefaultMaxQueuedLookups   = 1024
	DefaultOwnerLookupTimeout = time.Second
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

	// FlowTimeout drops a confirmed flow after its caller has sent nothing
	// for this long; PendingFlowTimeout drops a pending flow its worker has
	// not answered. Default DefaultFlowTimeout and DefaultPendingFlowTimeout.
	FlowTimeout        time.Duration
	PendingFlowTimeout time.Duration

	// MaxFlows bounds the flow table, and MaxPendingFlows the pending flows
	// in it. Default DefaultMaxFlows and DefaultMaxPendingFlows.
	MaxFlows        int
	MaxPendingFlows int

	// OwnerLookups is how many store lookups run at once; MaxQueuedLookups
	// bounds the sessions waiting for or undergoing one; OwnerLookupTimeout
	// bounds each. Default DefaultOwnerLookups, DefaultMaxQueuedLookups and
	// DefaultOwnerLookupTimeout.
	OwnerLookups       int
	MaxQueuedLookups   int
	OwnerLookupTimeout time.Duration

	// LoggerFactory defaults to Pion's default logger factory.
	LoggerFactory logging.LoggerFactory
}

// Stats counts what the relay has done since it started.
type Stats struct {
	// CallerPackets counts caller packets forwarded to a worker, and
	// WorkerPackets worker packets forwarded to a caller.
	CallerPackets uint64
	WorkerPackets uint64

	// STUNRouted counts binding requests forwarded to a session's owner.
	STUNRouted uint64

	// Binding requests dropped: UnknownSession for a session the store has
	// no owner for, LookupsDropped because owner lookups were saturated, and
	// LookupsFailed because the store lookup failed. Unroutable counts other
	// caller packets dropped for want of a confirmed flow.
	UnknownSession uint64
	LookupsDropped uint64
	LookupsFailed  uint64
	Unroutable     uint64

	// Relay-leg datagrams dropped: UnknownWorker from an unregistered
	// address, WorkerNoFlow for a caller whose flow the sending worker does
	// not own, and Malformed for a bad header.
	UnknownWorker uint64
	WorkerNoFlow  uint64
	Malformed     uint64

	// Flows and PendingFlows are the flow table's size now; FlowsEvicted
	// counts flows its limits have evicted.
	Flows        int
	PendingFlows int
	FlowsEvicted uint64
}

// Relay is a running relay.
type Relay struct {
	cfg        Config
	log        logging.LeveledLogger
	public     *net.UDPConn
	workers    *net.UDPConn
	publicAddr netip.AddrPort
	workerAddr netip.AddrPort
	flows      *flowTable
	lookups    *ownerLookups

	registryMu sync.RWMutex
	registry   map[netip.AddrPort]struct{}

	ctx       context.Context
	cancel    context.CancelFunc
	running   sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	callerPackets  atomic.Uint64
	workerPackets  atomic.Uint64
	stunRouted     atomic.Uint64
	unknownSession atomic.Uint64
	lookupsDropped atomic.Uint64
	lookupsFailed  atomic.Uint64
	unroutable     atomic.Uint64
	unknownWorker  atomic.Uint64
	workerNoFlow   atomic.Uint64
	malformed      atomic.Uint64
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
		cfg:        cfg,
		log:        cfg.LoggerFactory.NewLogger("relay"),
		public:     public,
		workers:    workers,
		publicAddr: publicAddr,
		workerAddr: workerAddr,
		flows: newFlowTable(flowLimits{
			idleTimeout:    cfg.FlowTimeout,
			pendingTimeout: cfg.PendingFlowTimeout,
			maxFlows:       cfg.MaxFlows,
			maxPending:     cfg.MaxPendingFlows,
		}),
		registry: make(map[netip.AddrPort]struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	for _, worker := range cfg.Workers {
		r.AddWorker(worker)
	}
	r.lookups = newOwnerLookups(r, cfg.OwnerLookups, cfg.MaxQueuedLookups)
	r.running.Add(3)
	go r.callerLoop()
	go r.workerLoop()
	go r.sweepFlows()

	return r, nil
}

func applyDefaults(cfg *Config) {
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = "127.0.0.1:0"
	}
	if cfg.WorkerAddr == "" {
		cfg.WorkerAddr = "127.0.0.1:0"
	}
	if cfg.FlowTimeout <= 0 {
		cfg.FlowTimeout = DefaultFlowTimeout
	}
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
	if cfg.OwnerLookupTimeout <= 0 {
		cfg.OwnerLookupTimeout = DefaultOwnerLookupTimeout
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
	flows, pending, evicted := r.flows.counts()

	return Stats{
		CallerPackets:  r.callerPackets.Load(),
		WorkerPackets:  r.workerPackets.Load(),
		STUNRouted:     r.stunRouted.Load(),
		UnknownSession: r.unknownSession.Load(),
		LookupsDropped: r.lookupsDropped.Load(),
		LookupsFailed:  r.lookupsFailed.Load(),
		Unroutable:     r.unroutable.Load(),
		UnknownWorker:  r.unknownWorker.Load(),
		WorkerNoFlow:   r.workerNoFlow.Load(),
		Malformed:      r.malformed.Load(),
		Flows:          flows,
		PendingFlows:   pending,
		FlowsEvicted:   evicted,
	}
}

// Close stops the relay and closes both of its sockets. The flow table goes
// with it; the session-owner store is not touched.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		r.closeErr = errors.Join(r.public.Close(), r.workers.Close())
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

// route picks the worker for a caller packet. A binding request on a flow
// for its session follows the flow, and a store lookup checks the owner in
// the background; any other binding request waits for its lookup (and is
// forwarded by the lookup, not here). Every other packet needs a confirmed
// flow.
func (r *Relay) route(pkt []byte, from netip.AddrPort) (netip.AddrPort, bool) {
	now := time.Now()

	if isSTUN(pkt) {
		if sessionID, ok := bindingRequestSession(pkt); ok {
			if worker, ok := r.flows.routeSTUN(from, sessionID, now); ok {
				r.stunRouted.Add(1)
				r.lookups.refresh(sessionID, from)

				return worker, true
			}
			if !r.lookups.resolve(sessionID, from, pkt) {
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
	start := MaxHeaderLen - HeaderLen(caller)
	AppendHeader(datagram[start:start], caller)
	if _, err := r.workers.WriteToUDPAddrPort(datagram[start:], worker); err != nil {
		r.log.Debugf("forward to worker %s: %v", worker, err)

		return
	}
	r.callerPackets.Add(1)
}

func (r *Relay) owner(sessionID string) (netip.AddrPort, error) {
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.OwnerLookupTimeout)
	defer cancel()

	return r.cfg.Owners.Owner(ctx, sessionID)
}

// resolved acts on a session's owner lookup for the callers waiting on it:
// it admits each waiting binding request's caller and forwards the request,
// or moves an established flow to a new owner.
func (r *Relay) resolved(sessionID string, owner netip.AddrPort, err error, waiters []waiter) {
	now := time.Now()
	switch {
	case err == nil:
		for _, w := range waiters {
			if w.datagram == nil {
				if r.flows.reroute(w.caller, owner, sessionID, now) {
					r.log.Infof("session %s: owner is now worker %s; caller %s follows", sessionID, owner, w.caller)
				}

				continue
			}
			r.flows.admit(w.caller, owner, sessionID, now)
			r.stunRouted.Add(1)
			r.forward(w.datagram, w.caller, owner)
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
}

// workerLoop reads the relay leg and sends each worker packet to the caller
// its header names, from the public socket: only from a registered worker,
// and only to a caller whose flow that worker owns.
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

		caller, pkt, err := ParseHeader(buf[:n])
		if err != nil {
			r.malformed.Add(1)
			r.log.Debugf("drop datagram from worker %s: %v", from, err)

			continue
		}
		allowed, confirmed := r.flows.answer(caller, from, time.Now())
		if !allowed {
			r.workerNoFlow.Add(1)

			continue
		}
		if confirmed != "" {
			r.log.Infof("session %s: caller %s <-> worker %s", confirmed, caller, from)
		}
		if _, err := r.public.WriteToUDPAddrPort(pkt, caller); err != nil {
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

// bindingRequestSession returns the session a STUN binding request is for:
// the first half of its USERNAME, which is the worker's ICE ufrag. Message
// integrity is not checked here; the worker checks it.
func bindingRequestSession(pkt []byte) (string, bool) {
	msg := &stun.Message{Raw: pkt}
	if err := msg.Decode(); err != nil || msg.Type != stun.BindingRequest {
		return "", false
	}
	var username stun.Username
	if err := username.GetFrom(msg); err != nil {
		return "", false
	}
	sessionID, _, ok := strings.Cut(username.String(), ":")

	return sessionID, ok && sessionID != ""
}

func unmap(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}
