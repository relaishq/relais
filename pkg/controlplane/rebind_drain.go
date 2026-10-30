package controlplane

import (
	"context"
	"net/netip"
)

func (p *Plane) saveRebindTarget(ctx context.Context, name string, target netip.AddrPort) error {
	if p.restart == nil {
		return nil
	}
	saved, err := p.linkRecord(ctx, name)
	if err != nil {
		return err
	}
	saved.RebindTarget = nil
	if target.IsValid() {
		saved.RebindTarget = &target
	}
	return p.controlRecord(ctx, "worker:"+name, saved)
}
func (p *Plane) beginRebind(ctx context.Context, w *registration, target netip.AddrPort) error {
	if err := p.saveRebindTarget(ctx, w.name, target); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workers[w.name] != w {
		return ErrWorkerLinkUnavailable
	}
	w.rebindTarget = target
	w.observation.markDraining()
	p.loadVersion++
	p.observationChangedLocked()
	return nil
}

// Caller serializes link admission. Operator intent is checked durably as well
// as in memory, since this path may arrive before the bootstrap rebuild.
func (p *Plane) clearRebind(ctx context.Context, w *registration, activate bool) error {
	records, err := p.linkRecords(ctx)
	if err != nil {
		return err
	}
	_, operator := records["drain:"+w.name]
	if err := p.saveRebindTarget(ctx, w.name, netip.AddrPort{}); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workers[w.name] != w {
		return ErrWorkerLinkUnavailable
	}
	w.rebindTarget = netip.AddrPort{}
	if activate {
		w.previousAddr = netip.AddrPort{}
	}
	w.operatorDraining = w.operatorDraining || operator
	w.observation.draining = w.operatorDraining
	p.loadVersion++
	p.observationChangedLocked()
	return nil
}

type legDrain struct {
	target netip.AddrPort
	cancel context.CancelFunc
}

func (l *WorkerLinks) cancelRebind(e *linkEntry) {
	l.jobsMu.Lock()
	defer l.jobsMu.Unlock()
	if e.drain != nil {
		e.drain.cancel()
	}
}
func (l *WorkerLinks) startRebindDrain(e *linkEntry, w *registration, target netip.AddrPort) error {
	l.jobsMu.Lock()
	if l.stopping.Load() {
		l.jobsMu.Unlock()
		return ErrWorkerLinkUnavailable
	}
	if e.drain != nil {
		if e.drain.target != target {
			e.drain.cancel()
		}
		l.jobsMu.Unlock()
		return nil
	}
	// Drain keeps its ordinary per-move/store bounds. The two-second report
	// request owns neither the drain budget nor its lifetime.
	ctx, cancel := context.WithCancel(l.ctx)
	job := &legDrain{target: target, cancel: cancel}
	e.drain = job
	l.jobs.Add(1)
	l.jobsMu.Unlock()
	go func() {
		defer l.jobs.Done()
		defer cancel()
		defer func() {
			l.jobsMu.Lock()
			if e.drain == job {
				e.drain = nil
			}
			l.jobsMu.Unlock()
		}()
		if _, err := l.plane.drain(ctx, w.name, w); err != nil {
			return
		}
		select {
		case e.admission <- struct{}{}:
		case <-ctx.Done():
			return
		}
		p := l.plane
		p.mu.Lock()
		ready := ctx.Err() == nil && w.rebindTarget == target && p.canReaddressLocked(w.name, w)
		p.mu.Unlock()
		e.mu.Lock()
		peer := e.peer
		e.mu.Unlock()
		<-e.admission
		if ready && peer != nil {
			peer.Close()
		}
	}()
	return nil
}

// A dead worker may be replaced before it acknowledges its new leg. Remove
// both abandoned endpoints before forgetting the pending cleanup identity.
func (l *WorkerLinks) cleanupReplacedRebind(ctx context.Context, w *registration, to netip.AddrPort) error {
	p := l.plane
	p.mu.Lock()
	previous, from := w.previousAddr, w.addr
	p.mu.Unlock()
	if !previous.IsValid() {
		return nil
	}
	for _, address := range []netip.AddrPort{previous, from} {
		if address == to {
			continue
		}
		leases, err := p.store.ListByWorker(ctx, address)
		if err != nil {
			return err
		}
		if len(leases) != 0 {
			return ErrWorkerLinkUnavailable
		}
		if err := l.relay.RemoveWorker(ctx, address); err != nil {
			return err
		}
	}
	return nil
}
