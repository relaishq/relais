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
			if prior := w.observed.Load(); prior != nil && *prior != address {
				return address, errors.New("mediaworker: relay address changed; drain required")
			}
			w.observed.Store(&address)
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
			valid := false
			if s != nil {
				s.mu.Lock()
				valid = err == nil && epoch == s.lease.Epoch && !s.fenced.Load()
				s.mu.Unlock()
			}
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
