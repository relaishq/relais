package controlplane

import (
	"context"
	"maps"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
)

// PoolSample is one pool observation. Demand is absent until an authoritative
// lease listing succeeds. Failed refreshes retain the last observation and its
// timestamp; age and stale distinguish it from a current zero-demand pool.
type PoolSample struct {
	Demand        *int              `json:"demand"`
	Cap           int               `json:"worker_call_cap"`
	Ready         int               `json:"ready_workers"`
	Draining      int               `json:"draining_workers"`
	Unreachable   int               `json:"unreachable_workers"`
	NeverAnswered int               `json:"never_answered_workers"`
	Pending       int               `json:"pending_workers"`
	Reservations  int               `json:"reservations"`
	Refusals      map[string]uint64 `json:"refusals"`
	OverCapCalls  int               `json:"over_cap_calls"`
	SampledAt     time.Time         `json:"sampled_at"`
	AgeSeconds    float64           `json:"age_seconds"`
	Stale         bool              `json:"stale"`
}

// BeginBootstrap records the fixed configured pool before serving requests.
// Local planes do not need it. Relay readiness is explicit for process control.
func (p *Plane) BeginBootstrap(names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bootstrap, p.relayReady = true, false
	for _, name := range names {
		p.pendingWorkers[name] = true
	}
	p.loadVersion++
}

// SetRelayReady gates creates on a successful relay allowlist sync.
func (p *Plane) SetRelayReady(ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.relayReady = ready
}

func (p *Plane) readyLocked() bool {
	if !p.mutationReady() {
		return false
	}
	if p.bootstrap && !p.relayReady {
		return false
	}
	for _, w := range p.workers {
		if !w.dead && !w.draining && !w.recovering && !w.relayPending && !w.livenessUnknown {
			return true
		}
	}
	return false
}

// SamplePool reads leases without holding mu and publishes the whole sample
// under one lock only if registry/reservation changes did not race the reads.
// Failure never replaces a previous demand with zero. The bounded retry leaves
// a stale sample under sustained churn instead of inventing a current sample.
func (p *Plane) SamplePool(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			p.mu.Lock()
			p.poolFailed = true
			p.mu.Unlock()
		}
	}()
	for range 8 {
		p.mu.Lock()
		version := p.loadVersion
		workers := make([]*registration, 0, len(p.workers))
		for _, w := range p.workers {
			workers = append(workers, w)
		}
		p.mu.Unlock()
		sampledAt := time.Now()
		loads := make(map[*registration]int, len(workers))
		for _, w := range workers {
			leases, listErr := p.store.ListByWorker(ctx, w.addr)
			if listErr != nil {
				return listErr
			}
			loads[w] = len(leases)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		if version != p.loadVersion {
			p.mu.Unlock()
			continue
		}
		demand := 0
		sample := PoolSample{Cap: p.config.WorkerCallCap, Pending: len(p.pendingWorkers), SampledAt: sampledAt, NeverAnswered: len(p.pendingWorkers)}
		for _, w := range workers {
			demand += loads[w] + w.reserved
			sample.Reservations += w.reserved
			if sample.Cap > 0 {
				sample.OverCapCalls += max(0, loads[w]-sample.Cap)
			}
			switch {
			case w.relayPending:
				sample.Pending++
			case w.dead || w.recovering:
				sample.Unreachable++
			case w.livenessUnknown:
				sample.Pending++
			case w.draining:
				sample.Draining++
			default:
				sample.Ready++
			}
		}
		if len(p.pendingWorkers) == 0 {
			sample.Demand = &demand
		}
		p.pool, p.poolFailed = &sample, false
		p.mu.Unlock()
		return nil
	}
	return sessionstore.ErrTransient
}

func (p *Plane) poolLocked(now time.Time) PoolSample {
	if p.pool == nil {
		return PoolSample{Cap: p.config.WorkerCallCap, NeverAnswered: len(p.pendingWorkers), Refusals: maps.Clone(p.refusals), Stale: true}
	}
	sample := *p.pool
	sample.Refusals = maps.Clone(p.refusals)
	sample.AgeSeconds = max(0, now.Sub(sample.SampledAt).Seconds())
	sample.Stale = sample.Demand == nil || p.poolFailed || now.Before(sample.SampledAt) || now.Sub(sample.SampledAt) > p.config.PoolSampleMaxAge
	return sample
}

// PoolSnapshot returns the cached observation without touching the store. It
// remains available in HTTP error responses when ownership reads also fail.
func (p *Plane) PoolSnapshot() PoolSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.poolLocked(time.Now())
}
