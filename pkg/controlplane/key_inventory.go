package controlplane

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
)

// KeyInventoryStatus is a sampled retirement aid, not a global transaction.
// Counts are null until a successful scan. Failed/stale samples retain their
// observation time and counts; they never become an authoritative zero.
type KeyInventoryStatus struct {
	Unverifiable uint64          `json:"unverifiable"`
	State        string          `json:"state"`
	Counts       map[byte]uint64 `json:"counts"`
	ObservedAt   time.Time       `json:"observed_at"`
	Error        string          `json:"error,omitempty"`
}
type keyInventoryCache struct {
	mu        sync.Mutex
	value     KeyInventoryStatus
	attempted time.Time
	loading   bool
	lifetime  context.Context
	jobs      sync.WaitGroup
}

const keyInventoryInterval = 5 * time.Second

func (p *Plane) scanKeyInventory(ctx context.Context) {
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	store := p.store.(interface {
		ComponentKeyInventory(context.Context) (map[byte]uint64, error)
	})
	counts, err := store.ComponentKeyInventory(bounded)
	p.inventory.mu.Lock()
	defer p.inventory.mu.Unlock()
	p.inventory.loading = false
	unverifiable, partial := err.(*sessionstore.InventoryUnverifiableError)
	if partial {
		p.inventory.value = KeyInventoryStatus{State: "unverifiable", Counts: maps.Clone(counts), ObservedAt: time.Now(), Unverifiable: unverifiable.Count}
		return
	}
	if err != nil {
		p.inventory.value.State = "failed"
		p.inventory.value.Error = "key inventory scan failed"
		return
	}
	if counts == nil {
		counts = map[byte]uint64{}
	}
	p.inventory.value = KeyInventoryStatus{State: "current", Counts: maps.Clone(counts), ObservedAt: time.Now()}
}

// startKeyInventory shares Run's lifetime and waits for bounded scans at exit.
func (p *Plane) startKeyInventory(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	p.inventory.mu.Lock()
	p.inventory.lifetime = ctx
	p.inventory.mu.Unlock()
	return func() {
		p.inventory.mu.Lock()
		p.inventory.lifetime = nil
		cancel()
		p.inventory.mu.Unlock()
		p.inventory.jobs.Wait()
	}
}

func (p *Plane) keyInventoryStatus(_ context.Context) KeyInventoryStatus {
	if _, ok := p.store.(interface {
		ComponentKeyInventory(context.Context) (map[byte]uint64, error)
	}); !ok {
		return KeyInventoryStatus{State: "unavailable"}
	}
	p.inventory.mu.Lock()
	defer p.inventory.mu.Unlock()
	first := p.inventory.attempted.IsZero()
	if p.inventory.value.State == "" {
		p.inventory.value.State = "not_yet_scanned"
	}
	ctx := p.inventory.lifetime
	if ctx != nil && ctx.Err() == nil && !p.inventory.loading && (first || time.Since(p.inventory.attempted) >= keyInventoryInterval) {
		p.inventory.loading = true
		p.inventory.attempted = time.Now()
		p.inventory.jobs.Add(1)
		go func() { defer p.inventory.jobs.Done(); p.scanKeyInventory(ctx) }()
	}
	value := p.inventory.value
	value.Counts = maps.Clone(value.Counts)
	if (value.State == "current" || value.State == "unverifiable") && time.Since(value.ObservedAt) >= keyInventoryInterval {
		value.State = "stale"
	}
	return value
}
