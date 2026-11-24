package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// relayPairLocked is traffic eligibility. Draining blocks new placement, but
// existing calls can still move workers or roll back through that slot.
func (p *Plane) relayPairLocked(w *registration, slot string) bool {
	b, ok := p.relaySet[slot]
	return w != nil && ok && !b.pending && !b.lost && b.Epoch != 0 && b.pairs[w].epoch == b.Epoch && b.pairs[w].instance == w.instance
}
func (p *Plane) workerRelayReadyLocked(w *registration) bool {
	if len(p.relaySet) == 0 {
		return true
	}
	for id := range p.relaySet {
		if !p.relaySet[id].draining && p.relayPairLocked(w, id) {
			return true
		}
	}
	return false
}
func (p *Plane) workerAtRelayLocked(w *registration, slot string) bool {
	if slot != "" {
		return p.relayPairLocked(w, slot)
	}
	return p.workerRelayReadyLocked(w)
}

func (p *Plane) callRelayPairLocked(c *call, slot string) bool {
	c.stateMu.Lock()
	addr := c.lease.Worker
	c.stateMu.Unlock()
	return p.relayPairLocked(p.owner(addr), slot)
}
func (p *Plane) relayAuthority() bool {
	if !p.controlAuthority() {
		return false
	}
	if p.restart == nil {
		return true
	}
	lease, err := p.controlLease()
	if err != nil {
		return false
	}
	p.mu.Lock()
	ready := p.relayControlEpoch == lease.Epoch
	p.mu.Unlock()
	return ready
}

func (p *Plane) restoreRelayCutoffs(records map[string]json.RawMessage, term uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, data := range records {
		id, cutoff := strings.CutPrefix(key, "relay-cutoff:")
		if !cutoff {
			var joined bool
			id, joined = strings.CutPrefix(key, "relay-joined:")
			if !joined {
				continue
			}
		}
		var epoch uint64
		if err := json.Unmarshal(data, &epoch); err != nil {
			return err
		}
		binding, known := p.relaySet[id]
		if !known {
			return mediaworker.ErrRelayUnsupported
		}
		if cutoff {
			binding.cutoff = max(binding.cutoff, epoch)
		} else {
			binding.joinedEpoch = max(binding.joinedEpoch, epoch)
		}
		p.relaySet[id] = binding
	}
	for id, binding := range p.relaySet {
		binding.lost = binding.cutoff > 0 && binding.cutoff >= binding.joinedEpoch
		if binding.lost {
			binding.pending, binding.cutoffDone = true, false
			binding.pairs = map[*registration]relayPairAck{}
		}
		p.relaySet[id] = binding
	}
	p.relayControlEpoch = term
	return nil
}

// relaySlotEmpty uses the durable assignments, including leases not yet cached
// as calls. Drain settlement is checked before clearing the drain mark.
func (p *Plane) relaySlotEmpty(ctx context.Context, slot string) (bool, error) {
	p.mu.Lock()
	workers := make([]netip.AddrPort, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w.addr)
	}
	ids := make(map[string]bool, len(p.calls))
	for id := range p.calls {
		ids[id] = true
	}
	p.mu.Unlock()
	for _, addr := range workers {
		leases, err := p.store.ListByWorker(ctx, addr)
		if err != nil {
			return false, err
		}
		for _, l := range leases {
			ids[l.SessionID] = true
		}
	}
	store := p.store.(sessionstore.RelayAssignments)
	for id := range ids {
		_, a, err := store.RelayOwner(ctx, id)
		if errors.Is(err, sessionstore.ErrNotFound) || errors.Is(err, sessionstore.ErrEnded) {
			continue
		}
		if err != nil {
			return false, err
		}
		if a.Slot == slot || a.Pending == slot {
			return false, nil
		}
		confirmations, ok := p.store.(sessionstore.RelayConfirmations)
		if !ok {
			return false, mediaworker.ErrRelayUnsupported
		}
		_, rec, err := confirmations.RelayConfirmation(ctx, id)
		if err != nil {
			return false, err
		}
		t := rec.Attempt
		if t != nil && t.Kind == "planned" && t.From == slot && (t.Phase == sessionstore.RelayPhasePending || t.Phase == sessionstore.RelayPhaseSwitched) && !rec.Aborted() && (rec.Episode == nil || rec.Episode.Outcome != sessionstore.RelayEpisodeEnded) {
			return false, nil
		}
	}
	return true, nil
}

func (p *Plane) syncRelaySlot(ctx context.Context, id string, remote interface {
	Status(context.Context) (relay.RelayStatus, error)
	AddWorker(context.Context, netip.AddrPort) error
}) error {
	if !p.relayAuthority() {
		return ErrNotReady
	}
	p.mu.Lock()
	binding, known := p.relaySet[id]
	p.mu.Unlock()
	if !known {
		return mediaworker.ErrRelayUnsupported
	}
	binding.syncMu.Lock()
	defer binding.syncMu.Unlock()
	status, err := remote.Status(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	changed := p.relaySet[id]
	if changed.instance != "" && changed.instance != status.Instance {
		changed.pending, changed.pairs = true, map[*registration]relayPairAck{}
		p.relaySet[id] = changed
		p.loadVersion++
	}
	p.mu.Unlock()
	leases, ok := p.store.(sessionstore.RelayLeases)
	if !ok {
		return mediaworker.ErrRelayUnsupported
	}
	lease, err := leases.GetRelay(ctx, id)
	if err != nil {
		return err
	}
	if status.Continuity == nil || status.Instance == "" || status.Instance != lease.Holder.Owner || status.Continuity.Epoch != lease.Epoch || lease.Expired {
		return mediaworker.ErrRelayUnsupported
	}
	if !status.Public.IsValid() || status.Public.Port() == 0 || !status.Private.IsValid() || status.Private.Port() == 0 {
		return mediaworker.ErrRelayUnsupported
	}
	p.mu.Lock()
	b := p.relaySet[id]
	if lease.Epoch <= b.cutoff {
		p.mu.Unlock()
		return mediaworker.ErrRelayUnsupported
	}
	rejoin := b.lost || b.draining
	if b.draining && (b.drainEpoch >= lease.Epoch || b.drainActive) {
		p.mu.Unlock()
		return ErrMoveInProgress
	}
	if b.lost && !b.cutoffDone {
		p.mu.Unlock()
		return ErrNotReady
	}
	p.mu.Unlock()
	if rejoin {
		if !p.mutationReady() {
			return ErrNotReady
		}
		empty, err := p.relaySlotEmpty(ctx, id)
		if err != nil {
			return err
		}
		if !empty {
			return ErrMoveInProgress
		}
	}
	slot := mediaworker.RelaySlot{ID: id, Leg: status.Private, Public: status.Public}
	p.relayPlacement.Lock()
	p.mu.Lock()
	b = p.relaySet[id]
	if b.Epoch != lease.Epoch || b.instance != status.Instance {
		b.pending, b.pairs = true, map[*registration]relayPairAck{}
	}
	b.Epoch, b.instance, b.Slot = lease.Epoch, status.Instance, slot
	p.relaySet[id] = b
	for w := range b.pairs {
		if p.workers[w.name] != w {
			delete(b.pairs, w)
		}
	}
	var workers []*registration
	for _, w := range p.workers {
		if w.observation.knowledge().relayEligible {
			workers = append(workers, w)
		}
	}
	p.mu.Unlock()
	p.relayPlacement.Unlock()
	var failures error
	for _, w := range workers {
		p.mu.Lock()
		current := p.relaySet[id]
		expectedInstance := w.instance
		joined := current.pairs[w].epoch == lease.Epoch && current.pairs[w].instance == w.instance
		p.mu.Unlock()
		if joined {
			continue
		}
		if err := remote.AddWorker(ctx, w.addr); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		worker, ok := w.worker.(relayChangeWorker)
		if !ok {
			failures = errors.Join(failures, mediaworker.ErrRelayUnsupported)
			continue
		}
		req := mediaworker.RelaySetRequest{Slots: []mediaworker.RelaySlot{slot}, Epochs: map[string]uint64{id: lease.Epoch}}
		if err := worker.ConfigureRelays(ctx, req); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		latest, err := leases.GetRelay(ctx, id)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		p.relayPlacement.Lock()
		p.mu.Lock()
		needsRecord := p.relaySet[id].joinedEpoch < lease.Epoch
		p.mu.Unlock()
		if needsRecord {
			if err := p.controlRecord(ctx, "relay-joined:"+id, lease.Epoch); err != nil {
				p.relayPlacement.Unlock()
				failures = errors.Join(failures, err)
				continue
			}
		}
		p.mu.Lock()
		current = p.relaySet[id]
		if !p.controlAuthority() || p.workers[w.name] != w || w.instance != expectedInstance || latest.Expired || latest.Epoch != lease.Epoch || latest.Holder.Owner != status.Instance || current.Epoch != lease.Epoch || current.cutoff >= lease.Epoch || current.draining && (current.drainEpoch >= lease.Epoch || current.drainActive) || current.lost && !current.cutoffDone {
			failures = errors.Join(failures, ErrNotReady)
		} else {
			current.joinedEpoch = max(current.joinedEpoch, lease.Epoch)
			current.pairs[w] = relayPairAck{epoch: lease.Epoch, instance: expectedInstance}
			current.pending, current.lost, current.draining = false, false, false
			current.lostAt = time.Time{}
			p.relaySet[id] = current
			p.loadVersion++
			p.observationChangedLocked()
		}
		p.mu.Unlock()
		p.relayPlacement.Unlock()
	}
	return failures
}
