package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
)

// TenureCleanupRelay must compare ownership before clearing a route. Legacy
// relays cannot safely implement missing-ownership cleanup by session ID alone.
type TenureCleanupRelay interface {
	ForgetSessionTenure(context.Context, sessionstore.Lease) error
}

func (c *call) rememberLease(lease sessionstore.Lease) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if lease.Epoch >= c.lease.Epoch {
		c.lease = lease
	}
}

// Run drives this independently of HTTP polling and worker heartbeats. Store
// errors retain the registry; only authoritative ErrNotFound proves a loss.
func (p *Plane) reconcileOwnership(ctx context.Context) {
	p.mu.Lock()
	calls := make([]*call, 0, len(p.calls))
	for id, c := range p.calls {
		pending := false
		for _, w := range p.workers {
			if w.pending[id] != nil {
				pending = true
				break
			}
		}
		if !pending {
			calls = append(calls, c)
		}
	}
	p.mu.Unlock()
	for _, c := range calls {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		pendingCleanup := c.cleanupPending
		p.mu.Unlock()
		if pendingCleanup {
			if c.mu.TryLock() {
				p.expiredLocked(ctx, c)
				c.mu.Unlock()
			}
			continue
		}
		lease, err := p.store.Get(ctx, c.id)
		if err == nil {
			c.rememberLease(lease)
			continue
		}
		if !errors.Is(err, sessionstore.ErrNotFound) || !c.mu.TryLock() {
			continue
		}
		p.expiredLocked(ctx, c)
		c.mu.Unlock()
	}
}

// The caller holds c.mu. Re-read under that lock: a stale status read must not
// declare a racing transfer lost. Moves, recovery and End share this lock.
func (p *Plane) expiredLocked(ctx context.Context, c *call) {
	if c.hungUp.Load() {
		return
	}
	if c.terminal {
		p.mu.Lock()
		pending := c.cleanupPending
		p.mu.Unlock()
		if pending {
			p.cleanupExpired(ctx, c)
		}
		return
	}
	lease, err := p.store.Get(ctx, c.id)
	if err == nil {
		c.rememberLease(lease)
		return
	}
	if !errors.Is(err, sessionstore.ErrNotFound) {
		return
	}
	c.stateMu.Lock()
	lost := c.lease
	c.stateMu.Unlock()
	p.mu.Lock()
	tracked := p.calls[c.id] == c
	for _, w := range p.workers {
		if w.pending[c.id] != nil {
			tracked = false
			break
		}
	}
	if !tracked {
		p.mu.Unlock()
		return
	}
	from := ""
	if owner := p.owner(lost.Worker); owner != nil {
		from = owner.name
	}
	// Accounting does not wait for a relay or store cleanup reply. Keep the
	// exact lost token in the registry until conditional cleanup can finish.
	c.terminal, c.lossLease, c.cleanupPending = true, lost, true
	now := time.Now()
	res := MoveResult{Kind: "takeover", ID: c.id, From: from, Start: now, End: now,
		Lost: true, LossCause: "ownership_expired", Error: "controlplane: ownership expired",
		Result: mediaworker.HandoverResult{SessionID: c.id}}
	p.lostCount++
	p.lossesByCause[res.LossCause]++
	p.recordTakeover(res)
	p.mu.Unlock()
	p.cleanupExpired(ctx, c)
}

// The call lock protects lossLease; registry membership is retained on any
// unknown cleanup result. Retrying never increments terminal counters again.
func (p *Plane) cleanupExpired(ctx context.Context, c *call) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lost := c.lossLease
	if err := p.store.Release(cleanupCtx, lost); err != nil {
		return
	}
	p.mu.Lock()
	r := p.relay
	p.mu.Unlock()
	if relay, ok := r.(TenureCleanupRelay); ok {
		if err := relay.ForgetSessionTenure(cleanupCtx, lost); err != nil {
			return
		}
	}
	current, err := p.store.Get(cleanupCtx, c.id)
	if err != nil && !errors.Is(err, sessionstore.ErrNotFound) {
		return
	}
	// A new claim after proven absence is not a resume of this lost call.
	// Preserve its state, including shared frames, without reviving the old call.
	if err == nil {
		c.rememberLease(current)
	} else {
		p.deleteFrames(c.id)
	}
	p.mu.Lock()
	c.cleanupPending = false
	if p.calls[c.id] == c {
		delete(p.calls, c.id)
	}
	p.mu.Unlock()
}
