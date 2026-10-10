package callharness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/pion/logging"

	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

// The relay topology (Options.Relay): one or more media workers, each on its
// own loopback UDP socket, behind a relay. Every answer advertises the
// relay's public address, the workers send and receive only over the relay
// leg, and the configured session-owner store tells the relay which worker owns
// each session. The shared-socket topology for handovers is in handover.go.

// workerParam is the signaling query parameter that picks the worker for an
// offer when the relay topology runs more than one.
const workerParam = "worker"

// relayTopology is the relay in front of the workers and the session-owner
// store it routes by. The store outlives relay restarts, as a shared store
// outlives a relay process.
type relayTopology struct {
	owners        sessionstore.Store
	loggerFactory logging.LoggerFactory

	mu               sync.Mutex
	relay            *relay.Relay
	plane            *controlplane.Plane
	cancel           context.CancelFunc
	done             chan struct{}
	snapshotInterval time.Duration

	frames                              framecache.Store
	disableFrameCache, disableResumePLI bool
}

// startRelayedWorkers starts the relay and opts.Workers workers behind it.
func startRelayedWorkers(opts Options) (*workers, error) {
	count := max(opts.Workers, 1)
	store := opts.SessionStore
	if store == nil {
		store = sessionstore.NewMemory()
	}
	topology := &relayTopology{owners: store, loggerFactory: opts.WorkerLoggerFactory}
	topology.frames = framecache.NewMemory(framecache.Limits{})
	topology.disableFrameCache, topology.disableResumePLI = opts.DisableFrameCache, opts.DisableResumePLI
	r, err := relay.New(relay.Config{
		PublicAddr:    "127.0.0.1:0",
		WorkerAddr:    "127.0.0.1:0",
		Owners:        topology.owners,
		LoggerFactory: opts.WorkerLoggerFactory,
	})
	if err != nil {
		return nil, fmt.Errorf("callharness: start relay: %w", err)
	}
	topology.relay = r
	topology.plane = controlplane.NewWithConfig(r, topology.owners, controlplane.Config{FrameCache: topology.frames})
	topology.snapshotInterval = opts.SnapshotInterval
	ctx, cancel := context.WithCancel(context.Background())
	topology.cancel, topology.done = cancel, make(chan struct{})
	go func() { defer close(topology.done); _ = topology.plane.Run(ctx) }()

	ws := &workers{relay: topology}
	for range count {
		worker, err := mediaworker.New(mediaworker.Config{
			ListenAddr:             "127.0.0.1:0",
			FrameCache:             topology.frames,
			DisableFrameCache:      opts.DisableFrameCache,
			DisableResumePLI:       opts.DisableResumePLI,
			ReplayMaxBurstDuration: opts.ReplayMaxBurstDuration,
			LoggerFactory:          opts.WorkerLoggerFactory,
			SnapshotInterval:       opts.SnapshotInterval,
			Relay: &mediaworker.RelayConfig{
				Addr:       r.WorkerAddr(),
				PublicAddr: r.PublicAddr(),
				Owners:     topology.owners,
			},
		})
		if err != nil {
			return nil, errors.Join(err, ws.close())
		}
		ws.list = append(ws.list, worker)
		r.AddWorker(worker.LocalAddr())
		if err := topology.plane.Register(strconv.Itoa(len(ws.list)-1), worker.LocalAddr(), worker); err != nil {
			return nil, errors.Join(err, ws.close())
		}
	}
	ws.signaling = topology.plane.Handler()

	return ws, nil
}

// close stops the relay. The workers close first: their sessions' last
// packets go out through it.
func (t *relayTopology) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.cancel()
	<-t.done
	return t.relay.Close()
}

// offerURL is where an offer for a worker goes. Only the relay topology
// lets a call pick its worker; otherwise calls start on the first.
func (h *Harness) offerURL(worker int) (string, error) {
	if worker == -1 {
		return h.signalingURL, nil
	}
	if h.external != nil {
		if worker < -1 {
			return "", errors.New("callharness: negative worker index")
		}
		u, err := url.Parse(h.signalingURL)
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set(workerParam, strconv.Itoa(worker))
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	ws := h.workers
	switch {
	case worker < 0 || worker >= len(ws.list):
		return "", fmt.Errorf("callharness: no worker %d; the harness runs %d", worker, len(ws.list))
	case worker != 0 && ws.relay == nil:
		return "", errors.New("callharness: CallOptions.Worker needs Options.Relay; calls start on the first worker")
	case ws.relay == nil || len(ws.list) == 1:
		return h.signalingURL, nil
	default:
		return h.signalingURL + "?" + workerParam + "=" + strconv.Itoa(worker), nil
	}
}

// RelayAddr is the relay's public address, which every answer advertises;
// the zero value without Options.Relay.
func (h *Harness) RelayAddr() netip.AddrPort {
	if h.external != nil {
		return h.external.RelayAddr
	}
	t := h.workers.relay
	if t == nil {
		return netip.AddrPort{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.relay.PublicAddr()
}

// RestartRelay replaces the relay with a new one on the same public and
// relay-leg addresses, as a restarted relay process would come back: the
// flow table is lost, while the session-owner store and the workers carry
// on, and the new relay is configured with the same workers. It returns
// once the new relay is listening, with how long no relay was listening.
func (h *Harness) RestartRelay() (time.Duration, error) {
	t := h.workers.relay
	if t == nil {
		return 0, errors.New("callharness: no relay; start the harness with Options.Relay")
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	old := t.relay
	cfg := relay.Config{
		PublicAddr:    old.PublicAddr().String(),
		WorkerAddr:    old.WorkerAddr().String(),
		Owners:        t.owners,
		LoggerFactory: t.loggerFactory,
	}
	for _, worker := range h.workers.list {
		cfg.Workers = append(cfg.Workers, worker.LocalAddr())
	}
	down := time.Now()
	if err := old.Close(); err != nil {
		return 0, fmt.Errorf("callharness: close relay: %w", err)
	}
	r, err := relay.New(cfg)
	if err != nil {
		return 0, fmt.Errorf("callharness: restart relay: %w", err)
	}
	t.relay = r
	t.plane.SetRelay(r)

	return time.Since(down), nil
}

// SessionOwner returns the index of the worker that owns a session, as the
// relay's session-owner store records it (see Call.SessionID). It reports
// false without Options.Relay, or when no worker owns the session.
func (h *Harness) SessionOwner(sessionID string) (int, bool) {
	t := h.workers.relay
	if t == nil {
		return 0, false
	}
	owner, err := t.owners.Owner(context.Background(), sessionID)
	if err != nil {
		return 0, false
	}
	for i, worker := range h.workers.list {
		if worker.LocalAddr() == owner {
			return i, true
		}
	}

	return 0, false
}

// SessionID is the call's session ID as the caller sees it: the ICE ufrag in
// the answer, which the media worker sets to the session ID.
func (c *Call) SessionID() string {
	return c.remoteUfrag
}

// selectedRemoteAddr is the remote end of the caller's selected ICE
// candidate pair, or "" before ICE has selected one.
func (c *Call) selectedRemoteAddr() string {
	pair, err := c.pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Remote == nil {
		return ""
	}

	return net.JoinHostPort(pair.Remote.Address, strconv.Itoa(int(pair.Remote.Port)))
}
