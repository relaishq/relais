package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

// observeHTTPInstance retains the predecessor's identity until its leases have
// settled and the returning process acknowledges the existing rejoin token.
func (p *Plane) observeHTTPInstance(ctx context.Context, name string, address netip.AddrPort, instance, token string) error {
	if instance == "" {
		return nil
	}
	p.mu.Lock()
	w := p.workers[name]
	p.mu.Unlock()
	if w == nil {
		return errors.New("controlplane: unknown worker instance")
	}
	w.instanceMu.Lock()
	defer w.instanceMu.Unlock()
	p.mu.Lock()
	if p.workers[name] != w || w.addr != address {
		p.mu.Unlock()
		return errors.New("controlplane: worker instance address changed")
	}
	if w.instance == instance {
		p.mu.Unlock()
		return nil
	}
	if w.instance != "" && !w.observation.dead {
		p.workerInstanceDiedLocked(w)
	}
	if w.observation.dead && (!w.observation.recovered || token == "" || token != w.observation.rejoinToken) {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	if err := p.saveRegistration(ctx, name, address, w.worker, instance); err != nil {
		return err
	}
	p.mu.Lock()
	w.instance = instance
	p.mu.Unlock()
	return nil
}

func (p *Plane) workerInstanceDiedLocked(w *registration) {
	w.observation.detect(time.Now(), 0)
	w.observation.recoveryFinished(false)
	w.observation.deathTransferIssued = w.transfers > 0
	p.metrics.detections.Add(1)
	p.loadVersion++
	p.observationChangedLocked()
}
