package controlplane

import (
	"context"
	"net/netip"
)

// RegisterRelayWorkers restores a restarted relay's private-leg allowlist.
// It snapshots only live registrations and does not affect heartbeat/rejoin or
// lease ownership. Network work never holds the plane's registry lock.
func (p *Plane) RegisterRelayWorkers(ctx context.Context, relay interface {
	AddWorker(context.Context, netip.AddrPort) error
}) error {
	p.mu.Lock()
	addresses := make([]netip.AddrPort, 0, len(p.workers))
	for _, worker := range p.workers {
		if !worker.dead && !worker.recovering {
			addresses = append(addresses, worker.addr)
		}
	}
	p.mu.Unlock()
	for _, address := range addresses {
		if err := relay.AddWorker(ctx, address); err != nil {
			return err
		}
	}
	return nil
}
