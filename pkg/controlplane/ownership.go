package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
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
	// Build the pending set once, instead of scanning workers for each call.
	pending := make(map[string]bool)
	for _, w := range p.workers {
		for id := range w.pending {
			pending[id] = true
		}
	}
	calls := make([]*call, 0, len(p.calls))
	for id, c := range p.calls {
		if !pending[id] {
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
				p.cleanupExpired(ctx, c)
				c.mu.Unlock()
			}
			continue
		}
		lease, err := p.store.Get(ctx, c.id)
		if err == nil {
			c.rememberLease(lease)
			continue
		}
		if (!errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrEnded)) || !c.mu.TryLock() {
			continue
		}
		p.expiredLocked(ctx, c)
		p.cleanupExpired(ctx, c)
		c.mu.Unlock()
	}
}

// The caller holds c.mu. Re-read under that lock: a stale status read must not
// declare a racing transfer lost. Moves, recovery and End share this lock.
func (p *Plane) expiredLocked(ctx context.Context, c *call) {
	if c.terminal || c.hungUp.Load() {
		return
	}
	lease, err := p.store.Get(ctx, c.id)
	if err == nil {
		c.rememberLease(lease)
		return
	}
	if !errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrEnded) {
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
	if errors.Is(err, sessionstore.ErrEnded) {
		// Explicit release is authoritative evidence of a normal worker-side end.
		c.hungUp.Store(true)
		c.terminal, c.lossLease, c.cleanupPending = true, lease, true
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
}

// The call lock protects lossLease; registry membership is retained on any
// unknown cleanup result. Retrying never increments terminal counters again.
func (p *Plane) cleanupExpired(ctx context.Context, c *call) {
	p.mu.Lock()
	pending := c.cleanupPending
	r := p.relay
	p.mu.Unlock()
	if !pending || time.Now().Before(c.cleanupAfter) {
		return
	}
	// One job per call, with capped backoff even when the store or relay stays down.
	if c.cleanupDelay == 0 {
		c.cleanupDelay = 250 * time.Millisecond
	} else {
		c.cleanupDelay = min(2*c.cleanupDelay, 30*time.Second)
	}
	c.cleanupAfter = time.Now().Add(c.cleanupDelay)
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lost := c.lossLease
	if err := p.store.Release(cleanupCtx, lost); err != nil {
		return
	}
	if relay, ok := r.(TenureCleanupRelay); ok {
		for _, token := range append([]sessionstore.Lease{lost}, c.cleanupLeases...) {
			if token.Epoch != 0 {
				if err := relay.ForgetSessionTenure(cleanupCtx, token); err != nil {
					return
				}
			}
		}
	} else if c.cleanupLegacy {
		// Legacy local/fake relays have no conditional API. Preserve their existing
		// terminal-loss cleanup only after verifying no successor owns the ID.
		current, err := p.store.Get(cleanupCtx, c.id)
		if err != nil && !errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrEnded) {
			return
		}
		if err != nil || sameTenure(current, lost) {
			r.ForgetSession(c.id)
			if c.cleanupPlanned {
				if _, err := r.ReleaseSession(c.id, netip.AddrPort{}); err != nil {
					return
				}
			}
		}
	}
	current, err := p.store.Get(cleanupCtx, c.id)
	if err != nil && !errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrEnded) {
		return
	}
	// A new claim after proven absence is not a resume of this lost call.
	if err == nil {
		c.rememberLease(current)
	} else {
		p.deleteFrames(c.id)
	}
	p.mu.Lock()
	c.cleanupPending = false
	p.mu.Unlock()
	p.finishIntent(c)
	p.mu.Lock()
	if p.restart != nil && c.intent != nil {
		// Keep the terminal call reachable until its durable intent is cleared.
		// Conditional lease/route cleanup is safe to retry after an unknown reply.
		c.cleanupPending = true
	} else if p.calls[c.id] == c {
		delete(p.calls, c.id)
	}
	p.mu.Unlock()
}

func sameTenure(a, b sessionstore.Lease) bool {
	return a.Worker == b.Worker && a.Epoch == b.Epoch
}
