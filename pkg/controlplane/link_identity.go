package controlplane

import (
	"context"
	"encoding/json"
	"net/netip"

	"github.com/relaishq/relais/pkg/mediaworker"
)

// linkRecord preserves optional process identity across bootstrap observation
// writes. Older inbound records keep their existing fields and encoding.
func (p *Plane) linkRecord(ctx context.Context, name string) (FixedWorker, error) {
	if p.restart == nil {
		return FixedWorker{}, nil
	}
	lease, err := p.controlLease()
	if err != nil {
		return FixedWorker{}, err
	}
	records, err := p.restart.store.ControlRecords(ctx, lease)
	if err != nil {
		return FixedWorker{}, err
	}
	var saved FixedWorker
	if data := records["worker:"+name]; len(data) != 0 {
		if err := json.Unmarshal(data, &saved); err != nil {
			return FixedWorker{}, err
		}
	}
	return saved, nil
}
func (l *WorkerLinks) restoreIdentity(ctx context.Context, name string, e *linkEntry) error {
	saved, err := l.plane.linkRecord(ctx, name)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.instance == "" && saved.Instance != "" {
		e.instance, e.activeInstance, e.sequence = saved.Instance, saved.Instance, saved.Sequence
	}
	e.mu.Unlock()
	// Admission can beat the background rebuild. Restore the same conservative
	// registration here, including any unfinished address activation.
	if l.plane.restart != nil && saved.Address.IsValid() {
		return l.plane.registerObservedWorker(ctx, FixedWorker{Name: name, URL: "dial-out"}, &saved, e.remote, mediaworker.WorkerStatus{}, ErrWorkerLinkUnavailable)
	}
	return nil
}
func (p *Plane) saveLinkRecord(ctx context.Context, name string, address, previous netip.AddrPort, hello *LinkHello, cycle uint64) error {
	if p.restart == nil {
		return nil
	}
	saved, err := p.linkRecord(ctx, name)
	if err != nil {
		return err
	}
	saved.Name, saved.URL, saved.Address = name, "dial-out", address
	if hello != nil {
		saved.Instance, saved.Sequence, saved.DeathCycle = hello.Instance, hello.Sequence, cycle
	}
	saved.PreviousAddress = nil
	if previous.IsValid() {
		saved.PreviousAddress = &previous
	}
	return p.controlRecord(ctx, "worker:"+name, saved)
}
