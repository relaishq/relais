package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/relaishq/relais/pkg/framecache"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// Config bounds crash detection and recovery. Zero values select defaults.
// The application drives Run with its lifetime context; registration connects
// in-process heartbeats (100 ms by default). Death after 400 ms excludes the
// worker from placement. Recovery takes leases without waiting for expiry,
// gates caller media when the relay cache is enabled, and resumes snapshots.
//
// Transient errors preserve retry state, including already transferred leases.
// Terminal missing-state/exhausted-target outcomes release with a fresh bounded
// context. Status retains 256 recent events plus lifetime per-call counts.
// Returning liveness does not grant eligibility. Recovery either confirms an
// unfenced live source or settles and requires the existing drop/rejoin ACK.
// Preservation is one decision for the whole worker death cycle (ADR 0013).
type Config struct {
	// WorkerCallCap limits calls plus incoming reservations; zero is unlimited.
	WorkerCallCap int
	// PoolSampleMaxAge marks cached telemetry stale (default five seconds).
	PoolSampleMaxAge time.Duration
	// StoreOutageBudget must match the workers (zero selects 30 seconds).
	StoreOutageBudget time.Duration
	// LeaseTTL overrides transfer lifetime, including outage headroom.
	LeaseTTL time.Duration
	// CheckpointEnvelope must match the workers' source-rate contract.
	CheckpointEnvelope mediaworker.CheckpointEnvelope
	// FrameCache is shared with the workers, for hangup/lost-call cleanup.
	FrameCache          framecache.Store
	DeadAfter           time.Duration // 400 ms without a heartbeat
	CheckInterval       time.Duration // 50 ms: detection adds at most one tick
	TakeoverParallelism int           // 16; never one goroutine per unbounded call set
	SequenceMargin      uint16        // 8192, below RTP's half sequence space
	SRTCPIndexMargin    uint32        // 128
}

func (c Config) defaults() Config {
	c.CheckpointEnvelope = c.CheckpointEnvelope.Defaults()
	if c.PoolSampleMaxAge <= 0 {
		c.PoolSampleMaxAge = 5 * time.Second
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = sessionstore.WorkerLeaseTTL(c.StoreOutageBudget)
	}
	if c.DeadAfter <= 0 {
		c.DeadAfter = 400 * time.Millisecond
	}
	if c.CheckInterval <= 0 {
		c.CheckInterval = 50 * time.Millisecond
	}
	if c.TakeoverParallelism <= 0 {
		c.TakeoverParallelism = 16
	}
	if c.SequenceMargin == 0 {
		c.SequenceMargin = defaultSequenceMargin
	}
	if c.SRTCPIndexMargin == 0 {
		c.SRTCPIndexMargin = defaultSRTCPIndexMargin
	}
	return c
}

// Heartbeat records liveness. A declared-dead worker cannot rejoin until its
// old leases have been handled or an unfenced live source has been confirmed.
// Ordinary rejoin still asks the worker to fence/drop its old sessions first.
// A remote heartbeat implementation must honor ErrRejoinRequired too.
// A heartbeat does not clear an operator's draining flag.
func (p *Plane) Heartbeat(addr netip.AddrPort) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.owner(addr)
	if w == nil {
		return errors.New("controlplane: unknown worker heartbeat")
	}
	before := w.observation.knowledge()
	returned := w.observation.deathReturned
	_, changed, err := w.observation.heartbeat(workerHeartbeat{}, time.Time{})
	if changed {
		p.loadVersion++
	}
	if before.eligible != w.observation.knowledge().eligible || !returned && w.observation.deathReturned {
		p.observationChangedLocked()
	}
	return err
}

// Run detects failed workers until ctx ends, and waits for its bounded
// recovery jobs before returning. Register connects real workers' heartbeats;
// remote implementations can call Heartbeat through their own transport.
func (p *Plane) Run(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return errors.New("controlplane: detector already running")
	}
	p.running = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.running = false; p.mu.Unlock() }()
	stopInventory := p.startKeyInventory(ctx)
	defer stopInventory()
	ticker := time.NewTicker(p.config.CheckInterval)
	defer ticker.Stop()
	var wg sync.WaitGroup
	var reconciling bool
	var nextReconcile time.Time
	var reconcileMu sync.Mutex
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !p.controlAuthority() || p.restart != nil && p.restart.stopping.Load() {
				continue
			}
			now := time.Now()
			reconcileMu.Lock()
			if !reconciling && !now.Before(nextReconcile) {
				reconciling = true
				nextReconcile = now.Add(min(p.ttl/2, 1500*time.Millisecond))
				wg.Go(func() {
					p.reconcileOwnership(ctx)
					_ = p.SamplePool(ctx)
					reconcileMu.Lock()
					reconciling = false
					reconcileMu.Unlock()
				})
			}
			reconcileMu.Unlock()
			p.mu.Lock()
			dead := []*registration{}
			planned := []*registration{}
			for _, w := range p.workers {
				detection := w.observation.detect(now, p.config.DeadAfter)
				if detection.recover {
					if detection.newDeath {
						p.metrics.detections.Add(1)
						w.observation.deathTransferIssued = w.transfers > 0
						p.observationChangedLocked()
					}
					dead = append(dead, w)
				} else if w.observation.knowledge().retryPlanned && !w.retryingMoves && p.recovery.hasWorkLocked(w, recoveryAll) {
					w.retryingMoves = true
					planned = append(planned, w)
				}
			}
			p.mu.Unlock()
			for _, w := range planned {
				wg.Go(func() {
					p.retryPlannedMoves(ctx, w, now)
					p.mu.Lock()
					w.retryingMoves = false
					p.mu.Unlock()
				})
			}
			for _, w := range dead {
				p.mu.Lock()
				cycle := w.observation.deathCycle
				p.mu.Unlock()
				wg.Go(func() {
					recovered := p.recoverWorker(ctx, w, now, cycle)
					p.mu.Lock()
					if w.observation.deathCycle == cycle {
						w.observation.recoveryFinished(recovered)
					}
					p.mu.Unlock()
				})
			}
		}
	}
}

func (p *Plane) recoverWorker(ctx context.Context, w *registration, detected time.Time, cycles ...uint64) bool {
	p.mu.Lock()
	cycle := w.observation.deathCycle
	if len(cycles) > 0 && cycles[0] != cycle {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()
	unknown := p.unknownCreateReservations(w)
	p.mu.Lock()
	incoming := w.reserved > unknown
	p.mu.Unlock()
	// An already selected create/resume may still publish a lease or call.
	// Keep the registration dead and retry after its reservation settles.
	if incoming {
		return false
	}
	leases, err := p.store.ListByWorker(ctx, w.addr)
	if err != nil {
		return false
	}
	// Include leases already transferred to a target that never adopted.
	p.mu.Lock()
	seen := make(map[string]bool, len(leases))
	for _, lease := range leases {
		seen[lease.SessionID] = true
	}
	for _, pending := range p.recovery.workLocked(w, recoveryAll) {
		if !seen[pending.SessionID] {
			leases = append(leases, pending)
		}
	}
	p.mu.Unlock()
	jobs := make(chan sessionstore.Lease)
	var wg sync.WaitGroup
	for range min(len(leases), p.config.TakeoverParallelism) {
		wg.Go(func() {
			for lease := range jobs {
				p.recovery.recover(ctx, w, lease, detected, cycle)
			}
		})
	}
	for _, lease := range leases {
		jobs <- lease
	}
	close(jobs)
	wg.Wait()
	p.mu.Lock()
	preserved := w.observation.deathPreserved
	p.mu.Unlock()
	if preserved {
		p.recovery.finishPreservedIntents(ctx, w)
		return true
	}
	remaining, err := p.store.ListByWorker(ctx, w.addr)
	unknown = p.unknownCreateReservations(w)
	p.mu.Lock()
	noIncoming := w.reserved <= unknown && !p.recovery.hasWorkLocked(w, recoveryAll)
	p.mu.Unlock()
	return err == nil && len(remaining) == 0 && noIncoming
}

// ErrMoveBudgetExhausted marks terminal recovery loss after bounded worker
// memory pressure retries. It is neither an agent fault nor incompatibility.
var ErrMoveBudgetExhausted = errors.New("controlplane: move budget exhausted")

// Margins cover 100 ms between snapshots + 400 ms without heartbeat + a
// 50 ms detector tick. At 10,000 RTP packets/s, 5500 indexes could be used;
// 8192 adds 49% headroom (another 269 ms for scheduling/routing). At 100
// feedback packets/s, 128 SRTCP indexes exceed twice the baseline budget.
// Reserve another 10,000 indexes for the caller's own outage packets (2 s
// at 5,000 packets/s per track). The snapshot's retained advance reduces the
// retry budget: fresh state admits two default margins, not three. Higher
// configured margins or already silent tracks allow fewer attempts.
// CheckpointEnvelope enforces longer store stalls with scaled margins or loss.
const (
	recentTakeoverLimit     = 256
	takeoverBudget          = 1500 * time.Millisecond // leaves detection headroom below 2 s
	maxResumeAttempts       = 3                       // also limited by the snapshot's remaining sequence budget
	defaultSequenceMargin   = 8192
	defaultSRTCPIndexMargin = 128
)

// recordTakeover and recentTakeovers run under p.mu. The ring bounds both
// retained event memory and the work of every /status response.
func (p *Plane) recordTakeover(res MoveResult) {
	p.events[p.eventNext] = res
	p.eventNext = (p.eventNext + 1) % recentTakeoverLimit
	p.eventCount = min(p.eventCount+1, recentTakeoverLimit)
}

func (p *Plane) recentTakeovers() []MoveResult {
	out := make([]MoveResult, 0, p.eventCount)
	start := (p.eventNext - p.eventCount + recentTakeoverLimit) % recentTakeoverLimit
	for i := range p.eventCount {
		out = append(out, p.events[(start+i)%recentTakeoverLimit])
	}
	return out
}

func (p *Plane) lose(c *call, lease sessionstore.Lease, planned bool, routed ...sessionstore.Lease) {
	c.lossLease = lease
	c.cleanupLeases = nil
	for _, old := range routed {
		if old.Epoch != 0 && !sameTenure(old, lease) {
			c.cleanupLeases = append(c.cleanupLeases, old)
		}
	}
	// Also preserve unconditional local cleanup for legacy/fake relays, after
	// verifying there is no successor. Built-in relays always compare tenures.
	c.cleanupLegacy, c.cleanupPlanned = true, planned
	p.mu.Lock()
	c.cleanupPending = true
	p.mu.Unlock()
}

// Replace swaps a dead, fully recovered registration for a fresh worker.
// The replacement starts empty; old addresses must be removed from the relay
// registry by the application after every old lease has been handled.
func (p *Plane) Replace(name string, addr netip.AddrPort, worker Worker) error {
	if !addr.IsValid() || worker == nil {
		return errors.New("controlplane: invalid replacement")
	}
	p.mu.Lock()
	old := p.workers[name]
	if old == nil || !old.observation.canReplace(old.reserved) {
		p.mu.Unlock()
		return errors.New("controlplane: replacement requires a recovered dead worker")
	}
	for otherName, w := range p.workers {
		if otherName != name && w.addr == addr {
			p.mu.Unlock()
			return errors.New("controlplane: duplicate worker address")
		}
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leases, err := p.store.ListByWorker(ctx, old.addr)
	if err != nil || len(leases) != 0 {
		return errors.New("controlplane: replacement still has leases")
	}
	if err := p.saveRegistration(ctx, name, addr, worker); err != nil {
		return err
	}
	if err := p.controlRecord(ctx, "drain:"+name, nil); err != nil {
		return err
	}
	p.mu.Lock()
	if p.workers[name] != old || !old.observation.canReplace(old.reserved) {
		p.mu.Unlock()
		return errors.New("controlplane: replacement raced with rejoin or recovery")
	}
	for otherName, w := range p.workers {
		if otherName != name && w.addr == addr {
			p.mu.Unlock()
			return errors.New("controlplane: duplicate worker address")
		}
	}
	observation := newWorkerObservation(time.Now(), false)
	observation.linkDown = p.workerUsesLink(name, worker)
	p.workers[name] = &registration{name: name, addr: addr, worker: worker, observation: observation}
	p.mu.Unlock()
	if w, ok := worker.(interface{ StartHeartbeats(mediaworker.Heartbeats) }); ok {
		w.StartHeartbeats(p)
	}
	return nil
}

func (p *Plane) retryPlannedMoves(ctx context.Context, w *registration, now time.Time) {
	p.mu.Lock()
	leases := p.recovery.workLocked(w, recoveryPlanned)
	p.mu.Unlock()
	jobs := make(chan sessionstore.Lease)
	var wg sync.WaitGroup
	for range min(len(leases), p.config.TakeoverParallelism) {
		wg.Go(func() {
			for lease := range jobs {
				p.recovery.recover(ctx, w, lease, now)
			}
		})
	}
	for _, lease := range leases {
		jobs <- lease
	}
	close(jobs)
	wg.Wait()
}

// deleteFrames applies only to a terminal call, never to the old worker's
// lost lease: the successor still needs that same shared cache.
func (p *Plane) deleteFrames(id string) {
	if p.config.FrameCache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = p.config.FrameCache.DeleteSession(ctx, id)
}

// resetRecoveredDrain leaves the registration and its death/rejoin cycle
// intact. Never use Replace for recycling a remote process at the same address.
func (p *Plane) resetRecoveredDrain(name string) error {
	p.mu.Lock()
	old := p.workers[name]
	if old == nil || !old.observation.canReplace(old.reserved) {
		p.mu.Unlock()
		return errors.New("controlplane: drain reset requires a recovered dead worker")
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leases, err := p.store.ListByWorker(ctx, old.addr)
	if err != nil || len(leases) != 0 {
		return errors.New("controlplane: drain reset still has leases")
	}
	if err := p.controlRecord(ctx, "drain:"+name, nil); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.workers[name] != old || !old.observation.canReplace(old.reserved) {
		return errors.New("controlplane: drain reset raced with rejoin or recovery")
	}
	old.observation.resetDrain()
	old.operatorDraining = false
	old.observation.draining = old.rebindTarget.IsValid()
	return nil
}
