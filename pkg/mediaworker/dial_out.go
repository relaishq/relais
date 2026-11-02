package mediaworker

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/privateapi"

	"github.com/relaishq/relais/internal/relayleg"
)

type legObservation struct {
	address netip.AddrPort
	token   string
}

// BindRelay sends first from the actual media socket. The read loop validates
// the reply's relay address and token. No call is accepted before this succeeds.
func (w *Worker) BindRelay(ctx context.Context, token string) (netip.AddrPort, error) {
	c, ok := w.conn.(*relayConn)
	if !ok {
		return netip.AddrPort{}, errors.New("mediaworker: dial-out requires relay")
	}
	c.bindingMu.Lock()
	c.bindingToken = token
	c.bindingMu.Unlock()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := c.conn.WriteToUDPAddrPort(relayleg.Binding(relayleg.Hello, token, netip.AddrPort{}), c.relay); err != nil {
			return netip.AddrPort{}, err
		}
		select {
		case observed := <-c.observed:
			if observed.token != token {
				continue
			}
			address := observed.address
			if w.observed.Load() == nil {
				w.observed.Store(&address)
			}
			return address, nil
		case <-ctx.Done():
			return netip.AddrPort{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// KeepRelayBinding keeps the NAT mapping alive even with no calls. It never
// supplies a heartbeat or changes the lease identity.
func (w *Worker) KeepRelayBinding(ctx context.Context) {
	c, ok := w.conn.(*relayConn)
	if !ok {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopRenew:
			return
		case <-ticker.C:
			c.bindingMu.Lock()
			token := c.bindingToken
			c.bindingMu.Unlock()
			if token != "" {
				_, _ = c.conn.WriteToUDPAddrPort(relayleg.Binding(relayleg.Keepalive, token, netip.AddrPort{}), c.relay)
			}
		}
	}
}

var ErrStaleTenure = errors.New("mediaworker: stale command tenure")

// LinkHandler preserves the private HTTP semantics and rejects a command for
// a different installed tenure before dispatching it to the existing handler.
func (w *Worker) LinkHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sessions/") {
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/")[0]
			epoch, err := strconv.ParseUint(r.Header.Get("X-Relais-Epoch"), 10, 64)
			s := w.session(id)
			if s == nil {
				// Preserve the existing unknown-session/idempotent-end handler outcome.
				handler.ServeHTTP(rw, r)
				return
			}
			s.mu.Lock()
			valid := err == nil && epoch == s.lease.Epoch && !s.fenced.Load()
			s.mu.Unlock()
			if !valid {
				privateapi.Error(rw, ErrStaleTenure, RemoteErrors)
				return
			}
		}
		handler.ServeHTTP(rw, r)
	})
}

// StatusSnapshot carries the same complete inventory as the private status API.
func (w *Worker) StatusSnapshot() WorkerStatus {
	status := WorkerStatus{Address: w.LocalAddr(), Sessions: w.SessionCount(), Replay: w.ReplayStats(), Agents: w.AgentStats(), InventoryKnown: true}
	status.Draining, status.Inventory, status.Truncated = w.installedSessions()
	return status
}

// AdoptRelayAddress runs only after control has drained the old identity and
// acknowledged registration. Heartbeats retain the old address until then.
func (w *Worker) AdoptRelayAddress(address netip.AddrPort) error {
	if old := w.observed.Load(); old != nil && *old != address && w.SessionCount() != 0 {
		return errors.New("mediaworker: rebind still has sessions")
	}
	w.observed.Store(&address)
	return nil
}
func (w *Worker) WaitRelayObservation(ctx context.Context) (netip.AddrPort, error) {
	c := w.conn.(*relayConn)
	for {
		select {
		case value := <-c.observed:
			c.bindingMu.Lock()
			current := value.token == c.bindingToken
			c.bindingMu.Unlock()
			if current {
				return value.address, nil
			}
		case <-ctx.Done():
			return netip.AddrPort{}, ctx.Err()
		}
	}
}
