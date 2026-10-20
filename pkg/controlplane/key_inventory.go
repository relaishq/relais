package controlplane

import (
	"context"
	"maps"
	"sync"
	"time"
)

// KeyInventoryStatus is a sampled retirement aid, not a global transaction.
// Counts are null until a successful scan. Failed/stale samples retain their
// observation time and counts; they never become an authoritative zero.
type KeyInventoryStatus struct {
	State      string          `json:"state"`
	Counts     map[byte]uint64 `json:"counts"`
	ObservedAt time.Time       `json:"observed_at"`
	Error      string          `json:"error,omitempty"`
}
type keyInventoryCache struct {
	mu        sync.Mutex
	value     KeyInventoryStatus
	attempted time.Time
	loading   bool
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
func (p *Plane) keyInventoryStatus(ctx context.Context) KeyInventoryStatus {
	if _, ok := p.store.(interface {
		ComponentKeyInventory(context.Context) (map[byte]uint64, error)
	}); !ok {
		return KeyInventoryStatus{State: "unavailable"}
	}
	p.inventory.mu.Lock()
	first := p.inventory.attempted.IsZero()
	refresh := !p.inventory.loading && (first || time.Since(p.inventory.attempted) >= keyInventoryInterval)
	if refresh {
		p.inventory.loading = true
		p.inventory.attempted = time.Now()
	}
	if p.inventory.value.State == "" {
		p.inventory.value.State = "loading"
	}
	p.inventory.mu.Unlock()
	if refresh {
		if first {
			p.scanKeyInventory(ctx)
		} else {
			go p.scanKeyInventory(context.Background())
		}
	}
	p.inventory.mu.Lock()
	defer p.inventory.mu.Unlock()
	value := p.inventory.value
	value.Counts = maps.Clone(value.Counts)
	if value.State == "current" && time.Since(value.ObservedAt) >= keyInventoryInterval {
		value.State = "stale"
	}
	return value
}
