package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/netip"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerlink"
	"github.com/relaishq/relais/pkg/relay"
)

// LinkReady keeps heartbeats on the old identity until drain/registration have
// finished. Activation is acknowledged separately, before restoring placement.
type LinkReady struct {
	Address  netip.AddrPort `json:"address"`
	Activate bool           `json:"activate,omitempty"`
}

func (r *RemoteRelay) prepareRebind(ctx context.Context, from, to netip.AddrPort, token string) error {
	return privateapi.Do(ctx, remoteClient(r.Client), r.URL, http.MethodPost, "/workers/rebind", relay.RebindRequest{From: from, To: to, Token: token}, nil, relay.RemoteErrors)
}
func (l *WorkerLinks) legHandler(name string, e *linkEntry, peer **workerlink.Peer, token string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/leg-address" && r.URL.Path != "/leg-ready") {
			http.NotFound(rw, r)
			return
		}
		var body LinkAddress
		if !privateapi.Read(rw, r, &body) || !body.Address.IsValid() {
			return
		}
		select {
		case e.admission <- struct{}{}:
			defer func() { <-e.admission }()
		case <-r.Context().Done():
			return
		}
		e.mu.Lock()
		current := e.peer == *peer
		e.mu.Unlock()
		if !current || l.stopping.Load() || !l.plane.controlAuthority() {
			privateapi.Error(rw, ErrWorkerLinkUnavailable, RemoteErrors)
			return
		}
		p := l.plane
		p.mu.Lock()
		w := p.workers[name]
		previous := netip.AddrPort{}
		rebindTarget := netip.AddrPort{}
		if w != nil {
			previous = w.previousAddr
			rebindTarget = w.rebindTarget
		}
		pending := previous.IsValid()
		p.mu.Unlock()
		if w == nil {
			http.NotFound(rw, r)
			return
		}
		if r.URL.Path == "/leg-address" {
			if body.Address != w.addr {
				if err := l.relay.prepareRebind(r.Context(), w.addr, body.Address, token); err != nil {
					privateapi.Error(rw, err, RemoteErrors)
					return
				}
				if err := p.beginRebind(r.Context(), w, body.Address); err != nil {
					privateapi.Error(rw, err, RemoteErrors)
					return
				}
				if err := l.startRebindDrain(e, w, body.Address); err != nil {
					privateapi.Error(rw, err, RemoteErrors)
					return
				}
			} else if !pending && rebindTarget.IsValid() {
				l.cancelRebind(e)
				if err := l.relay.AddBoundWorker(r.Context(), w.addr, token); err != nil {
					privateapi.Error(rw, err, RemoteErrors)
					return
				}
				if err := p.clearRebind(r.Context(), w, false); err != nil {
					privateapi.Error(rw, err, RemoteErrors)
					return
				}
			}
		} else if pending {
			if err := p.requireMutationReady(); err != nil {
				privateapi.Error(rw, err, RemoteErrors)
				return
			}
			if body.Address != w.addr {
				privateapi.Error(rw, ErrWorkerCommandNotSent, RemoteErrors)
				return
			}
			if err := l.relay.RemoveWorker(r.Context(), previous); err != nil {
				privateapi.Error(rw, err, RemoteErrors)
				return
			}
			if err := p.saveLinkRecord(r.Context(), name, w.addr, netip.AddrPort{}, nil, 0); err != nil {
				privateapi.Error(rw, err, RemoteErrors)
				return
			}
			if err := p.clearRebind(r.Context(), w, true); err != nil {
				privateapi.Error(rw, err, RemoteErrors)
				return
			}
		}
		rw.WriteHeader(http.StatusNoContent)
	})
}

// readdress changes no lease. Only a live, drained registration without incoming
// reservations or installed sessions may adopt the freshly bound endpoint.
func (p *Plane) readdress(ctx context.Context, name string, old *registration, to netip.AddrPort) error {
	p.mu.Lock()
	safe := p.canReaddressLocked(name, old)
	p.mu.Unlock()
	if !safe {
		return ErrWorkerLinkUnavailable
	}
	leases, err := p.store.ListByWorker(ctx, old.addr)
	if err != nil {
		return err
	}
	if len(leases) != 0 {
		return errors.New("controlplane: rebind still owns sessions")
	}
	if err := p.saveLinkRecord(ctx, name, to, old.addr, nil, 0); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.canReaddressLocked(name, old) {
		return ErrWorkerLinkUnavailable
	}
	if other := p.owner(to); other != nil && other != old {
		return errors.New("controlplane: duplicate rebind address")
	}
	next := &registration{name: old.name, addr: to, previousAddr: old.addr, worker: old.worker, observation: old.observation, rebindTarget: old.rebindTarget, operatorDraining: old.operatorDraining}
	next.observation.linkDown = true
	p.workers[name] = next
	p.loadVersion++
	p.observationChangedLocked()
	return nil
}

// Addresses remain immutable to concurrent move/recovery readers. Replacing a
// registration is safe only after all work keyed by the old pointer has settled.
func (p *Plane) canReaddressLocked(name string, w *registration) bool {
	return p.workers[name] == w && w.observation.draining && !w.observation.dead && !w.observation.recovering &&
		w.reserved == 0 && w.transfers == 0 && w.outgoingMoves == 0 && w.rollbackSlots == 0 && !w.retryingMoves &&
		len(w.preservedIntents) == 0 && !p.recovery.hasWorkLocked(w, recoveryAll)
}
