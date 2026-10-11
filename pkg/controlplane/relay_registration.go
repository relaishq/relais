package controlplane

import (
	"context"
	"net/netip"

	"github.com/relais/pkg/relay"
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

// RelayWorkerRegistry is used by one polling loop. Successful registrations
// belong to one relay instance; late registrations and rejoins are discovered
// on every tick. Failed adds are retried without renewing leases or heartbeats.
type RelayWorkerRegistry struct {
	instance   string
	registered map[netip.AddrPort]bool
}

func (s *RelayWorkerRegistry) Sync(ctx context.Context, p *Plane, r interface {
	Status(context.Context) (relay.RelayStatus, error)
	AddWorker(context.Context, netip.AddrPort) error
}) error {
	status, err := r.Status(ctx)
	if err != nil {
		return err
	}
	if s.registered == nil || status.Instance != s.instance {
		s.instance = status.Instance
		s.registered = make(map[netip.AddrPort]bool)
	}
	p.mu.Lock()
	addresses := make([]netip.AddrPort, 0, len(p.workers))
	for _, w := range p.workers {
		if !w.dead && !w.recovering {
			addresses = append(addresses, w.addr)
		}
	}
	p.mu.Unlock()
	for _, address := range addresses {
		if !s.registered[address] {
			if err := r.AddWorker(ctx, address); err != nil {
				return err
			}
			s.registered[address] = true
		}
	}
	return nil
}
