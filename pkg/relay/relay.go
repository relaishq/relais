// Package relay is the relay: the one public UDP address in front of the
// media workers. Every SDP answer advertises the relay's public address as
// its single host candidate, so callers send everything there, and the
// relay sends each caller's packets to the media worker that owns the
// caller's session.
//
// The relay reads only STUN. A STUN binding request carries the ICE
// USERNAME "worker ufrag:caller ufrag", and the worker's ufrag is the
// session ID, so the relay looks the session's owner up in the session-owner
// store (sessionstore.Owners) and remembers the caller's address in its flow
// table. Every other packet (DTLS, SRTP, SRTCP, or anything else) is
// forwarded untouched to the worker the flow table names for the address it
// came from; the relay never looks past the bytes that tell STUN apart from
// the rest (RFC 7983). A packet from an address with no flow is dropped.
//
// Media workers bind only private sockets. They reach callers through the
// relay: the relay leg (see header.go) carries each packet with the caller's
// address, so callers only ever see the relay's address.
//
// The flow table is a cache, not state. A restarted relay starts with an
// empty table and rebuilds each flow from the store on the caller's next
// STUN binding request (ICE consent checks arrive every few seconds); until
// then the caller's non-STUN packets are dropped. Worker-to-caller packets
// need no flow, since the relay leg names their destination.
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

const (
	// DefaultFlowTimeout drops a flow when its caller has sent nothing for
	// RFC 7675's consent timeout: by then the worker has given up on the
	// session too.
	DefaultFlowTimeout = 30 * time.Second

	// ownerLookupTimeout bounds one session-owner store lookup.
	ownerLookupTimeout = time.Second

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

	// Owners is the session-owner store the relay routes STUN binding
	// requests by. Required.
	Owners sessionstore.Owners

	// FlowTimeout drops a caller's flow after the caller has sent nothing for
	// this long. Defaults to DefaultFlowTimeout.
	FlowTimeout time.Duration

	// LoggerFactory defaults to Pion's default logger factory.
	LoggerFactory logging.LoggerFactory
}

// Stats counts what the relay has done since it started.
type Stats struct {
	// CallerPackets counts caller packets forwarded to a worker, and
	// WorkerPackets worker packets forwarded to a caller.
	CallerPackets uint64
	WorkerPackets uint64

	// STUNRouted counts STUN binding requests routed by the session-owner
	// store.
	STUNRouted uint64

	// UnknownSession counts binding requests for a session the store has no
	// owner for; Unroutable counts other caller packets dropped because their
	// address had no flow. Malformed counts relay-leg datagrams dropped for a
	// bad header.
	UnknownSession uint64
	Unroutable     uint64
	Malformed      uint64

	// Flows is the number of flows in the flow table now.
	Flows int
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

	ctx       context.Context
	cancel    context.CancelFunc
	running   sync.WaitGroup
	closeOnce sync.Once
	closeErr  error

	callerPackets  atomic.Uint64
	workerPackets  atomic.Uint64
	stunRouted     atomic.Uint64
	unknownSession atomic.Uint64
	unroutable     atomic.Uint64
	malformed      atomic.Uint64
}

// New starts a relay.
func New(cfg Config) (*Relay, error) {
	if cfg.Owners == nil {
		return nil, errors.New("relay: Config.Owners is required")
	}
	if cfg.PublicAddr == "" {
		cfg.PublicAddr = "127.0.0.1:0"
	}
	if cfg.WorkerAddr == "" {
		cfg.WorkerAddr = "127.0.0.1:0"
	}
	if cfg.FlowTimeout <= 0 {
		cfg.FlowTimeout = DefaultFlowTimeout
	}
	if cfg.LoggerFactory == nil {
		cfg.LoggerFactory = logging.NewDefaultLoggerFactory()
	}

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
		flows:      newFlowTable(cfg.FlowTimeout),
		ctx:        ctx,
		cancel:     cancel,
	}
	r.running.Add(3)
	go r.callerLoop()
	go r.workerLoop()
	go r.sweepFlows()

	return r, nil
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

// Stats returns the relay's counters.
func (r *Relay) Stats() Stats {
	return Stats{
		CallerPackets:  r.callerPackets.Load(),
		WorkerPackets:  r.workerPackets.Load(),
		STUNRouted:     r.stunRouted.Load(),
		UnknownSession: r.unknownSession.Load(),
		Unroutable:     r.unroutable.Load(),
		Malformed:      r.malformed.Load(),
		Flows:          r.flows.len(),
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
// then written in front of it in place.
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
		pkt := buf[MaxHeaderLen : MaxHeaderLen+n]

		worker, ok := r.route(pkt, from)
		if !ok {
			continue
		}

		start := MaxHeaderLen - HeaderLen(from)
		AppendHeader(buf[start:start], from)
		if _, err := r.workers.WriteToUDPAddrPort(buf[start:MaxHeaderLen+n], worker); err != nil {
			r.log.Debugf("forward to worker %s: %v", worker, err)

			continue
		}
		r.callerPackets.Add(1)
	}
}

// route picks the worker for a caller packet. A STUN binding request is
// routed by the session-owner store and (re)binds the caller's flow; every
// other packet follows the flow of the address it came from.
func (r *Relay) route(pkt []byte, from netip.AddrPort) (netip.AddrPort, bool) {
	now := time.Now()

	if isSTUN(pkt) {
		sessionID, ok := bindingRequestSession(pkt)
		if ok {
			worker, err := r.owner(sessionID)
			switch {
			case err == nil:
				r.stunRouted.Add(1)
				if r.flows.bind(from, worker, sessionID, now) {
					r.log.Infof("session %s: caller %s -> worker %s", sessionID, from, worker)
				}

				return worker, true
			case errors.Is(err, sessionstore.ErrNotFound):
				r.unknownSession.Add(1)
				r.log.Debugf("drop binding request from %s: no owner for session %s", from, sessionID)

				return netip.AddrPort{}, false
			default:
				// The store is unavailable: an existing flow still routes.
				r.log.Warnf("session %s: look up owner: %v", sessionID, err)
			}
		}
	}

	worker, ok := r.flows.lookup(from, now)
	if !ok {
		r.unroutable.Add(1)
	}

	return worker, ok
}

func (r *Relay) owner(sessionID string) (netip.AddrPort, error) {
	ctx, cancel := context.WithTimeout(r.ctx, ownerLookupTimeout)
	defer cancel()

	return r.cfg.Owners.Owner(ctx, sessionID)
}

// workerLoop reads the relay leg and sends each worker packet to the caller
// its header names, from the public socket.
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

		caller, pkt, err := ParseHeader(buf[:n])
		if err != nil {
			r.malformed.Add(1)
			r.log.Debugf("drop datagram from worker %s: %v", from, err)

			continue
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

	ticker := time.NewTicker(r.cfg.FlowTimeout / 2)
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
