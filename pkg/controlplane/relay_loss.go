package controlplane

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/relaylease"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
)

const relayLossAfter = 2 * time.Second
const relayRecoveryLimit = 15 * time.Second

// Each slot is observed independently: a failed store poll never adds time,
// and a slow lookup cannot stall the other slots or worker death detection.
func (p *Plane) runRelayLoss(ctx context.Context) {
	leases, ok := p.store.(sessionstore.RelayLeases)
	if !ok {
		return
	}
	tick := time.NewTicker(relaylease.DefaultPoll)
	defer tick.Stop()
	started := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !p.controlAuthority() {
				continue
			}
			p.mu.Lock()
			var ids []string
			for id := range p.relaySet {
				if !started[id] {
					ids = append(ids, id)
					started[id] = true
				}
			}
			p.mu.Unlock()
			for _, id := range ids {
				wg.Go(func() {
					var observation relaylease.Observation
					poll := time.NewTicker(relaylease.DefaultPoll)
					defer poll.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-poll.C:
							if !p.controlAuthority() {
								continue
							}
							readCtx, cancel := context.WithTimeout(ctx, relaylease.DefaultPoll)
							lease, err := leases.GetRelay(readCtx, id)
							cancel()
							if observation.Observe(lease, err, time.Now(), relayLossAfter) {
								// Retries retain the lost mark. No placement or planned change can
								// use the slot while a worker's cut-off acknowledgement is pending.
								if p.loseRelay(ctx, id) == nil {
									return
								}
							}
						}
					}
				})
			}
		}
	}
}

func (p *Plane) loseRelay(ctx context.Context, slot string) error {
	p.relayPlacement.Lock()
	p.mu.Lock()
	binding := p.relaySet[slot]
	if binding.lostAt.IsZero() {
		binding.lostAt = time.Now()
	}
	if binding.lossUnaffected == nil {
		binding.lossUnaffected = make(map[string]bool)
	}
	binding.lost = true
	p.relaySet[slot] = binding
	var slots []mediaworker.RelaySlot
	for _, b := range p.relaySet {
		if !b.lost {
			slots = append(slots, b.Slot)
		}
	}
	var workers []relayChangeWorker
	for _, w := range p.workers {
		if w.observation.canReplace(w.reserved) {
			continue // Settled death has fenced every old lease; replacement enrolls afresh.
		}
		worker, ok := w.worker.(relayChangeWorker)
		if !ok {
			p.mu.Unlock()
			p.relayPlacement.Unlock()
			return mediaworker.ErrRelayUnsupported
		}
		workers = append(workers, worker)
	}
	var calls []*call
	for _, c := range p.calls {
		calls = append(calls, c)
	}
	p.mu.Unlock()
	p.relayPlacement.Unlock()
	var wg sync.WaitGroup
	out := make(chan error, len(workers))
	for _, w := range workers {
		wg.Go(func() {
			cutCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			out <- w.ConfigureRelays(cutCtx, slots)
		})
	}
	wg.Wait()
	close(out)
	var err error
	for e := range out {
		err = errors.Join(err, e)
	}
	if err != nil {
		// Only calls without a loaded episode need store work on a cut-off retry.
		epoch := p.relayControlEpoch()
		var affected []*call
		for _, c := range calls {
			c.stateMu.Lock()
			capable := c.relayCapable
			c.stateMu.Unlock()
			if capable {
				continue
			}
			c.relayEpisodeMu.Lock()
			e := c.relayEpisode
			loaded := e != nil && e.loaded && e.controlEpoch == epoch
			c.relayEpisodeMu.Unlock()
			if loaded {
				continue
			}
			p.mu.Lock()
			unaffected := binding.lossUnaffected[c.id]
			p.mu.Unlock()
			if unaffected {
				continue
			}
			_, assignment, readErr := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.id)
			if errors.Is(readErr, sessionstore.ErrNotFound) || errors.Is(readErr, sessionstore.ErrEnded) || readErr == nil && assignment.Slot != slot && assignment.Pending != slot {
				p.mu.Lock()
				binding.lossUnaffected[c.id] = true
				p.mu.Unlock()
				continue
			}
			err = errors.Join(err, readErr)
			if readErr == nil && (assignment.Slot == slot || assignment.Pending == slot) {
				affected = append(affected, c)
			}
		}
		if len(affected) == 0 {
			return err
		}
		snapshotEpoch := p.relayControlEpoch()
		records, readErr := p.store.(sessionstore.ControlStore).ControlIntents(ctx)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		journal := make(map[string]sessionstore.ControlIntent, len(records))
		for _, record := range records {
			journal[record.ID] = record
		}
		for _, c := range affected {
			_, e := p.relayRecoveryEpisode(ctx, c, binding.lostAt, relayIntentSnapshot{epoch: snapshotEpoch, records: journal})
			err = errors.Join(err, e)
		}
		return err
	}
	// Cut-off needs no call/store lookups. Once acknowledged, share one journal
	// snapshot and bound initialization I/O; recovery drivers still start together.
	snapshotEpoch := p.relayControlEpoch()
	records, err := p.store.(sessionstore.ControlStore).ControlIntents(ctx)
	if err != nil {
		return err
	}
	journal := make(map[string]sessionstore.ControlIntent, len(records))
	for _, record := range records {
		journal[record.ID] = record
	}
	jobs := make(chan *call)
	callErrors := make(chan error, len(calls))
	for range min(len(calls), p.config.TakeoverParallelism) {
		wg.Go(func() {
			for c := range jobs {
				_, assignment, e := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.id)
				if errors.Is(e, sessionstore.ErrNotFound) || errors.Is(e, sessionstore.ErrEnded) {
					continue
				}
				if e == nil && (assignment.Slot == slot || assignment.Pending == slot) {
					if _, e = p.relayRecoveryEpisode(ctx, c, binding.lostAt, relayIntentSnapshot{epoch: snapshotEpoch, records: journal}); e == nil {
						c.controlMu.Lock()
						if c.relayCancel != nil && (!c.relayUnplanned || c.relayTarget == slot) {
							c.relayCancel()
						}
						c.controlMu.Unlock()
						e = p.startRelayRecovery(ctx, c, false)
					}
					p.mu.Lock()
					terminal := c.terminal || c.hungUp.Load()
					p.mu.Unlock()
					if terminal {
						e = nil
					}
				}
				if e != nil {
					callErrors <- e
				}
			}
		})
	}
	for _, c := range calls {
		select {
		case jobs <- c:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	close(callErrors)
	for e := range callErrors {
		err = errors.Join(err, e)
	}
	return err
}

func (p *Plane) endRelayFailure(c *call, lease sessionstore.Lease, reason string) {
	c.stateMu.Lock()
	c.relayEndReason = reason
	c.stateMu.Unlock()
	p.mu.Lock()
	if c.terminal || c.hungUp.Load() {
		p.mu.Unlock()
		return
	}
	c.terminal = true
	p.lostCount++
	p.lossesByCause[reason]++
	owner := p.owner(lease.Worker)
	p.mu.Unlock()
	if owner != nil {
		_ = owner.worker.EndSession(c.id)
	}
	p.lose(c, lease, false)
}
