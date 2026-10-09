package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
)

// Config bounds crash detection and recovery. Zero values select defaults.
// The application drives Run with its lifetime context; registration connects
// in-process heartbeats (100 ms by default). Death after 400 ms excludes the
// worker from placement. Recovery takes leases without waiting for expiry,
// reroutes immediately without a hold, and resumes non-destructive snapshots.
//
// Transient errors preserve retry state, including already transferred leases.
// Terminal missing-state/exhausted-target outcomes release with a fresh bounded
// context. Status retains 256 recent events plus lifetime per-call counts.
// A returning worker stays dead until recovery settles and it acknowledges
// fencing/dropping its old sessions. Remote workers need an explicit token
// for this acknowledgment; the in-process contract uses the next heartbeat.
type Config struct {
	// FrameCache is shared with the workers, for hangup/lost-call cleanup.
	FrameCache          framecache.Store
	DeadAfter           time.Duration // 400 ms without a heartbeat
	CheckInterval       time.Duration // 50 ms: detection adds at most one tick
	TakeoverParallelism int           // 16; never one goroutine per unbounded call set
	SequenceMargin      uint16        // 8192, below RTP's half sequence space
	SRTCPIndexMargin    uint32        // 128
}

func (c Config) defaults() Config {
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
// old leases have been handled. The first returning heartbeat asks the
// worker to fence/drop its old sessions; its acknowledgment makes it healthy.
// A remote heartbeat implementation must honor ErrRejoinRequired too.
// A heartbeat does not clear an operator's draining flag.
func (p *Plane) Heartbeat(addr netip.AddrPort) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.owner(addr)
	if w == nil {
		return errors.New("controlplane: unknown worker heartbeat")
	}
	if w.recovering || w.dead && !w.recovered {
		return errors.New("controlplane: worker takeover in progress")
	}
	if w.dead && !w.rejoinReady {
		w.rejoinReady = true
		return mediaworker.ErrRejoinRequired
	}
	w.dead = false
	w.lastHeartbeat = time.Now()
	return nil
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
	ticker := time.NewTicker(p.config.CheckInterval)
	defer ticker.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			now := time.Now()
			p.mu.Lock()
			dead := []*registration{}
			for _, w := range p.workers {
				if !w.recovering && ((!w.dead && now.Sub(w.lastHeartbeat) >= p.config.DeadAfter) || (w.dead && !w.recovered)) {
					w.dead, w.recovering, w.recovered, w.rejoinReady = true, true, false, false
					dead = append(dead, w)
				}
			}
			p.mu.Unlock()
			for _, w := range dead {
				wg.Go(func() {
					recovered := p.recoverWorker(ctx, w, now)
					p.mu.Lock()
					w.recovered = recovered
					w.recovering = false
					p.mu.Unlock()
				})
			}
		}
	}
}

func (p *Plane) recoverWorker(ctx context.Context, w *registration, detected time.Time) bool {
	p.mu.Lock()
	incoming := w.reserved != 0
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
	for id, pending := range w.pending {
		if !seen[id] {
			leases = append(leases, pending.lease)
		}
	}
	p.mu.Unlock()
	jobs := make(chan sessionstore.Lease)
	var wg sync.WaitGroup
	for range min(len(leases), p.config.TakeoverParallelism) {
		wg.Go(func() {
			for lease := range jobs {
				p.takeover(ctx, w, lease, detected)
			}
		})
	}
	for _, lease := range leases {
		jobs <- lease
	}
	close(jobs)
	wg.Wait()
	remaining, err := p.store.ListByWorker(ctx, w.addr)
	p.mu.Lock()
	noIncoming := w.reserved == 0 && len(w.pending) == 0
	p.mu.Unlock()
	return err == nil && len(remaining) == 0 && noIncoming
}

// takeoverState survives transient errors even after Transfer changed the owner.
// Its fields are accessed under the call lock; pending membership uses p.mu.
type takeoverState struct {
	lease           sessionstore.Lease
	routed          netip.AddrPort
	excluded        map[netip.AddrPort]bool
	attempts        int
	attemptLimit    int
	transientResume bool
}

// Margins cover 100 ms between snapshots + 400 ms without heartbeat + a
// 50 ms detector tick. At 10,000 RTP packets/s, 5500 indexes could be used;
// 8192 adds 49% headroom (another 269 ms for scheduling/routing). At 100
// feedback packets/s, 128 SRTCP indexes exceed twice the baseline budget.
// Reserve another 10,000 indexes for the caller's own outage packets (2 s
// at 5,000 packets/s per track). The snapshot's retained advance reduces the
// retry budget: fresh state admits two default margins, not three. Higher
// configured margins or already silent tracks allow fewer attempts.
// Longer queues/store stalls or higher rates need a later age/rate policy.
const (
	recentTakeoverLimit     = 256
	takeoverBudget          = 1500 * time.Millisecond // leaves detection headroom below 2 s
	maxResumeAttempts       = 3                       // also limited by the snapshot's remaining sequence budget
	defaultSequenceMargin   = 8192
	defaultSRTCPIndexMargin = 128
)

func (p *Plane) takeover(ctx context.Context, source *registration, listed sessionstore.Lease, detected time.Time) {
	ctx, cancel := context.WithTimeout(ctx, takeoverBudget)
	defer cancel()
	p.mu.Lock()
	c := p.calls[listed.SessionID]
	if c == nil {
		// A live lease is authoritative even if a racing move lost metadata.
		c = &call{id: listed.SessionID}
		p.calls[c.id] = c
	}
	p.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	lease, err := p.store.Get(ctx, c.id)
	if err != nil || lease.Worker != listed.Worker || lease.Epoch != listed.Epoch {
		if errors.Is(err, sessionstore.ErrNotFound) {
			p.mu.Lock()
			pending := source.pending[c.id]
			lastHeartbeat := source.lastHeartbeat
			to := ""
			if pending != nil {
				if target := p.owner(pending.lease.Worker); target != nil {
					to = target.name
				}
			}
			p.mu.Unlock()
			if pending != nil {
				res := MoveResult{Kind: "takeover", ID: c.id, From: source.name, To: to, Start: detected,
					DetectedAt: detected, LastHeartbeat: lastHeartbeat, Result: mediaworker.HandoverResult{SessionID: c.id}}
				p.completeTakeover(source, c, pending.lease, &res, true,
					fmt.Errorf("controlplane: pending takeover lease vanished: %w", err))
			}
		} else if err == nil {
			p.finishPending(source, c.id)
		}
		return
	}
	p.mu.Lock()
	if source.pending == nil {
		source.pending = make(map[string]*takeoverState)
	}
	pending := source.pending[c.id]
	if pending == nil {
		pending = &takeoverState{lease: lease, routed: lease.Worker,
			excluded: map[netip.AddrPort]bool{source.addr: true}, attemptLimit: maxResumeAttempts}
		source.pending[c.id] = pending
	}
	r := p.relay
	lastHeartbeat := source.lastHeartbeat
	p.mu.Unlock()
	res := MoveResult{Kind: "takeover", ID: c.id, From: source.name, Start: detected,
		DetectedAt: detected, LastHeartbeat: lastHeartbeat, Result: mediaworker.HandoverResult{SessionID: c.id}}
	// Publish only terminal outcomes. A transient error preserves the lease,
	// call and retry state without falsely reporting a lost takeover.
	complete := func(lost bool, cause error) {
		p.completeTakeover(source, c, pending.lease, &res, lost, cause)
	}

	for noTargetAttempts := 0; pending.attempts < pending.attemptLimit; {
		p.mu.Lock()
		target, pickErr := p.pickExcluding(ctx, "", pending.excluded)
		p.mu.Unlock()
		if pickErr != nil {
			if !errors.Is(pickErr, ErrNoTarget) {
				return
			}
			noTargetAttempts++
			if noTargetAttempts == maxResumeAttempts {
				complete(true, pickErr)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}
		res.To = target.name
		transferred := pending.lease
		if transferred.Worker != target.addr {
			transferred, err = p.store.Transfer(ctx, pending.lease, target.addr, p.ttl)
			if err != nil {
				p.unreserve(target)
				return
			}
			pending.lease = transferred
		}
		// No source hold or rollback. Routing fences the previous leg immediately.
		if err = r.MoveSession(c.id, pending.routed, target.addr); err != nil {
			p.unreserve(target)
			return
		}
		pending.routed = target.addr
		state, stateErr := p.store.GetState(ctx, c.id)
		if stateErr != nil {
			p.unreserve(target)
			if errors.Is(stateErr, sessionstore.ErrNotFound) {
				complete(true, fmt.Errorf("controlplane: no takeover snapshot: %w", stateErr))
			}
			return
		}
		budget, budgetErr := mediaworker.SequenceResumeAttempts(state, p.config.SequenceMargin)
		if budgetErr != nil {
			p.unreserve(target)
			complete(true, budgetErr)
			return
		}
		pending.attemptLimit = min(pending.attemptLimit, pending.attempts+budget)
		if pending.attempts >= pending.attemptLimit {
			p.unreserve(target)
			complete(true, mediaworker.ErrSequenceBudgetExhausted)
			return
		}
		res.Result.StateBytes = len(state)
		started := time.Now()
		pending.attempts++
		_, err = target.worker.ResumeSession(state, mediaworker.ResumeOptions{Lease: transferred,
			Context: ctx, SequenceMargin: p.config.SequenceMargin, SRTCPIndexMargin: p.config.SRTCPIndexMargin})
		res.Result.Resume += time.Since(started)
		p.unreserve(target)
		if err == nil {
			now := time.Now()
			c.lastMove, c.lastMoveKind = &now, "takeover"
			c.moveCount++
			c.takeoverCount++
			complete(false, nil)
			return
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			pending.transientResume = true
			return
		}
		if errors.Is(err, mediaworker.ErrSequenceBudgetExhausted) {
			complete(true, err)
			return
		}
		pending.transientResume = false
		pending.excluded[target.addr] = true
		// Resume persists adjusted state before adoption. Reload that latest blob
		// on the next target, and apply one more margin within the capped budget.
	}
	// Cancellation cannot turn an unfinished adoption into a definitive loss.
	// Keep the tenure until explicit hangup or lease expiry if its safe counter
	// budget has been consumed by cancelled attempts.
	if pending.transientResume {
		return
	}
	if err == nil {
		err = errors.New("counter margin retry budget exhausted")
	}
	complete(true, fmt.Errorf("controlplane: takeover resume attempts exhausted: %w", err))
}

// completeTakeover runs under the call lock; loss cleanup outlives detector
// cancellation. Expired pending leases are terminal too, never silent exits.
func (p *Plane) completeTakeover(source *registration, c *call, lease sessionstore.Lease,
	res *MoveResult, lost bool, cause error) {
	if lost {
		p.lose(c, lease)
	}
	p.finishPending(source, c.id)
	res.End = time.Now()
	res.Result.Duration = res.End.Sub(res.Start)
	res.Lost = lost
	if cause != nil {
		res.Error = cause.Error()
	}
	p.mu.Lock()
	p.recordTakeover(*res)
	if lost {
		p.lostCount++
	}
	p.mu.Unlock()
}

func (p *Plane) finishPending(source *registration, id string) {
	p.mu.Lock()
	delete(source.pending, id)
	p.mu.Unlock()
}

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

func (p *Plane) lose(c *call, lease sessionstore.Lease) {
	// Cleanup outlives a cancelled detector context, as with move rollback.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = p.store.Release(ctx, lease)
	p.deleteFrames(c.id)
	p.mu.Lock()
	r := p.relay
	p.mu.Unlock()
	r.ForgetSession(c.id)
	p.forget(c)
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
	if old == nil || !old.dead || !old.recovered || old.recovering || old.reserved != 0 {
		p.mu.Unlock()
		return errors.New("controlplane: replacement requires a recovered dead worker")
	}
	for otherName, w := range p.workers {
		if otherName != name && w.addr == addr {
			p.mu.Unlock()
			return errors.New("controlplane: duplicate worker address")
		}
	}
	leases, err := p.store.ListByWorker(context.Background(), old.addr)
	if err != nil || len(leases) != 0 {
		p.mu.Unlock()
		return errors.New("controlplane: replacement still has leases")
	}
	p.workers[name] = &registration{name: name, addr: addr, worker: worker, lastHeartbeat: time.Now()}
	p.mu.Unlock()
	if w, ok := worker.(interface{ StartHeartbeats(mediaworker.Heartbeats) }); ok {
		w.StartHeartbeats(p)
	}
	return nil
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
