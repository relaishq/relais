package callharness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/pion/logging"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
)

// The harness topology: one or more media workers on loopback UDP sockets,
// and with Options.Relay a relay in front of them. Behind the relay every
// answer advertises the relay's public address, the workers send and receive
// only over the relay leg, and an in-memory session-owner store tells the
// relay which worker owns each session.

// workerParam is the signaling query parameter that picks the worker for an
// offer when the harness runs more than one.
const workerParam = "worker"

// relayTopology is the relay in front of the workers and the session-owner
// store it routes by. The store outlives relay restarts, as a shared store
// outlives a relay process.
type relayTopology struct {
	owners        *sessionstore.Memory
	loggerFactory logging.LoggerFactory

	mu    sync.Mutex
	relay *relay.Relay
}

func (h *Harness) startTopology(opts Options) error {
	workers := opts.Workers
	if workers == 0 {
		workers = 1
	}
	if workers < 0 {
		return fmt.Errorf("callharness: %d workers", workers)
	}

	var relayConfig *mediaworker.RelayConfig
	if opts.Relay {
		topology := &relayTopology{owners: sessionstore.NewMemory(), loggerFactory: opts.WorkerLoggerFactory}
		r, err := relay.New(relay.Config{
			PublicAddr:    "127.0.0.1:0",
			WorkerAddr:    "127.0.0.1:0",
			Owners:        topology.owners,
			LoggerFactory: opts.WorkerLoggerFactory,
		})
		if err != nil {
			return fmt.Errorf("callharness: start relay: %w", err)
		}
		topology.relay = r
		h.relay = topology
		relayConfig = &mediaworker.RelayConfig{
			Addr:       r.WorkerAddr(),
			PublicAddr: r.PublicAddr(),
			Owners:     topology.owners,
		}
	}

	for range workers {
		worker, err := mediaworker.New(mediaworker.Config{
			ListenAddr:    "127.0.0.1:0",
			LoggerFactory: opts.WorkerLoggerFactory,
			Relay:         relayConfig,
		})
		if err != nil {
			return errors.Join(err, h.closeTopology())
		}
		h.workers = append(h.workers, worker)
	}

	return nil
}

// closeTopology closes the workers, which hang up their calls, then the
// relay.
func (h *Harness) closeTopology() error {
	var errs []error
	for _, worker := range h.workers {
		errs = append(errs, worker.Close())
	}
	if h.relay != nil {
		h.relay.mu.Lock()
		errs = append(errs, h.relay.relay.Close())
		h.relay.mu.Unlock()
	}

	return errors.Join(errs...)
}

// signalingHandler is the WHIP-style signaling endpoint. With one worker it
// is that worker's own endpoint. With more, a small front stands in for the
// control plane: an offer goes to the worker that CallOptions.Worker names
// (the "worker" query parameter), and a hang-up goes to whichever worker has
// the session.
func (h *Harness) signalingHandler() http.Handler {
	if len(h.workers) == 1 {
		return h.workers[0].SignalingHandler()
	}

	endpoints := make([]http.Handler, len(h.workers))
	for i, worker := range h.workers {
		endpoints[i] = worker.SignalingHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+mediaworker.CallsPath, func(rw http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(r.URL.Query().Get(workerParam))
		if err != nil || i < 0 || i >= len(endpoints) {
			http.Error(rw, "unknown worker", http.StatusBadRequest)

			return
		}
		endpoints[i].ServeHTTP(rw, r)
	})
	mux.HandleFunc("DELETE "+mediaworker.CallsPath+"/{id}", func(rw http.ResponseWriter, r *http.Request) {
		for _, worker := range h.workers {
			if worker.EndSession(r.PathValue("id")) == nil {
				rw.WriteHeader(http.StatusOK)

				return
			}
		}
		http.Error(rw, mediaworker.ErrUnknownSession.Error(), http.StatusNotFound)
	})

	return mux
}

// offerURL is where an offer for a worker goes.
func (h *Harness) offerURL(worker int) (string, error) {
	if worker < 0 || worker >= len(h.workers) {
		return "", fmt.Errorf("callharness: no worker %d; the harness runs %d", worker, len(h.workers))
	}
	if len(h.workers) == 1 {
		return h.signalingURL, nil
	}

	return h.signalingURL + "?" + workerParam + "=" + strconv.Itoa(worker), nil
}

// RelayAddr is the relay's public address, which every answer advertises;
// the zero value without Options.Relay.
func (h *Harness) RelayAddr() netip.AddrPort {
	if h.relay == nil {
		return netip.AddrPort{}
	}
	h.relay.mu.Lock()
	defer h.relay.mu.Unlock()

	return h.relay.relay.PublicAddr()
}

// RestartRelay replaces the relay with a new one on the same public and
// relay-leg addresses, as a restarted relay process would come back: the
// flow table is lost, while the session-owner store and the workers carry
// on. It returns once the new relay is listening, with how long no relay
// was listening.
func (h *Harness) RestartRelay() (time.Duration, error) {
	if h.relay == nil {
		return 0, errors.New("callharness: no relay; start the harness with Options.Relay")
	}
	h.relay.mu.Lock()
	defer h.relay.mu.Unlock()

	old := h.relay.relay
	cfg := relay.Config{
		PublicAddr:    old.PublicAddr().String(),
		WorkerAddr:    old.WorkerAddr().String(),
		Owners:        h.relay.owners,
		LoggerFactory: h.relay.loggerFactory,
	}
	down := time.Now()
	if err := old.Close(); err != nil {
		return 0, fmt.Errorf("callharness: close relay: %w", err)
	}
	r, err := relay.New(cfg)
	if err != nil {
		return 0, fmt.Errorf("callharness: restart relay: %w", err)
	}
	h.relay.relay = r

	return time.Since(down), nil
}

// SessionOwner returns the index of the worker that owns a session, as the
// relay's session-owner store records it (see Call.SessionID). It reports
// false without Options.Relay, or when no worker owns the session.
func (h *Harness) SessionOwner(sessionID string) (int, bool) {
	if h.relay == nil {
		return 0, false
	}
	owner, err := h.relay.owners.Owner(context.Background(), sessionID)
	if err != nil {
		return 0, false
	}
	for i, worker := range h.workers {
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
