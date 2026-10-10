package relay

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/stun/v4"
	"github.com/relais/pkg/sessionstore"
)

// routeWrites coalesces by session and caller. A single off-path writer orders
// nomination, movement and deletion, bounds store concurrency, and gives each
// queued operation a deadline. Overflow is counted, never waited on by UDP.
type routeWrites struct {
	relay   *Relay
	queue   chan string
	mu      sync.Mutex
	pending map[string]*routeWrite
	count   int
}
type routeWrite struct {
	routes map[netip.AddrPort]routeSnapshot
	forget bool
}

func newRouteWrites(r *Relay) *routeWrites {
	p := &routeWrites{relay: r, queue: make(chan string, r.cfg.MaxQueuedLookups), pending: make(map[string]*routeWrite)}
	r.running.Add(1)
	go p.run()
	return p
}
func (p *routeWrites) enqueue(id string, snapshot *routeSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	task := p.pending[id]
	if task == nil {
		if len(p.pending) >= cap(p.queue) {
			p.relay.routeWritesDropped.Add(1)
			return
		}
		task = &routeWrite{routes: make(map[netip.AddrPort]routeSnapshot)}
		p.pending[id] = task
		p.queue <- id
	}
	if snapshot == nil {
		p.count -= len(task.routes)
		task.routes = make(map[netip.AddrPort]routeSnapshot)
		task.forget = true
		return
	}
	if _, ok := task.routes[snapshot.record.Caller]; !ok {
		if p.count >= p.relay.cfg.MaxQueuedLookups {
			p.relay.routeWritesDropped.Add(1)
			return
		}
		p.count++
	}
	// A pending nomination survives coalescing with its next renewal or move.
	previous := task.routes[snapshot.record.Caller]
	snapshot.nominated = snapshot.nominated || previous.nominated
	task.routes[snapshot.record.Caller] = *snapshot
}
func (p *routeWrites) run() {
	defer p.relay.running.Done()
	for {
		select {
		case <-p.relay.ctx.Done():
			return
		case id := <-p.queue:
			p.mu.Lock()
			task := p.pending[id]
			delete(p.pending, id)
			p.count -= len(task.routes)
			p.mu.Unlock()
			if task.forget {
				ctx, cancel := context.WithTimeout(p.relay.ctx, p.relay.cfg.OwnerLookupTimeout)
				err := p.relay.cfg.Routes.ForgetRoutes(ctx, id)
				cancel()
				if err != nil {
					p.relay.routeWritesFailed.Add(1)
				}
			}
			for _, snapshot := range task.routes {
				p.write(snapshot)
			}
		}
	}
}
func (p *routeWrites) write(snapshot routeSnapshot) {
	r := p.relay
	// Never relabel old worker evidence with a successor's epoch. A move queues
	// a fresh snapshot with that owner, retaining the original consent time.
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.OwnerLookupTimeout)
	defer cancel()
	lease, err := r.cfg.Routes.Get(ctx, snapshot.record.SessionID)
	if err != nil {
		if !errors.Is(err, sessionstore.ErrNotFound) {
			r.routeWritesFailed.Add(1)
		}
		return
	}
	if lease.Worker != snapshot.worker {
		return
	}
	current, ok := r.flows.forwardRoute(snapshot.record.Caller)
	if !ok || current.worker != snapshot.worker || current.session != snapshot.record.SessionID {
		return
	}
	snapshot.record.Generation = lease.Epoch
	err = r.cfg.Routes.PutRoute(ctx, snapshot.record, snapshot.nominated)
	if err != nil {
		if !errors.Is(err, sessionstore.ErrLeaseLost) && !errors.Is(err, sessionstore.ErrNotFound) {
			r.routeWritesFailed.Add(1)
		}
		return
	}
	r.routeWritesDone.Add(1)
}

func (r *Relay) persist(caller netip.AddrPort, force bool) {
	if r.persistence == nil {
		return
	}
	if snapshot, ok := r.flows.snapshot(caller, force, time.Now()); ok {
		r.persistence.enqueue(snapshot.record.SessionID, &snapshot)
	}
}
func (r *Relay) restoreRoutes() error {
	if r.cfg.Routes == nil || r.cfg.DisableRouteRestore {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.RouteRestoreTimeout)
	defer cancel()
	routes, err := r.cfg.Routes.LoadRoutes(ctx, r.cfg.MaxFlows)
	if err != nil {
		return fmt.Errorf("relay: load persisted routes: %w", err)
	}
	for _, route := range routes {
		lease, err := r.cfg.Routes.Get(ctx, route.SessionID)
		if err != nil && !errors.Is(err, sessionstore.ErrNotFound) {
			return fmt.Errorf("relay: restore owner: %w", err)
		}
		if errors.Is(err, sessionstore.ErrNotFound) || lease.Epoch != route.Generation || !time.Now().Before(route.ExpiresAt) {
			if err := r.cfg.Routes.DeleteRoute(ctx, route); err != nil {
				return fmt.Errorf("relay: delete stale route: %w", err)
			}
			continue
		}
		if r.flows.restore(route, unmap(lease.Worker), time.Now()) {
			r.routesRestored.Add(1)
		}
	}
	return nil
}

func isNomination(pkt []byte) bool {
	if !isSTUN(pkt) {
		return false
	}
	msg := &stun.Message{Raw: pkt}
	return msg.Decode() == nil && msg.Type == stun.BindingRequest && msg.Contains(stun.AttrUseCandidate)
}
