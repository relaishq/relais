package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
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
// A returning worker stays dead until recovery settles and it acknowledges
// fencing/dropping its old sessions. Remote workers need an explicit token
// for this acknowledgment; the in-process contract uses the next heartbeat.
type Config struct {
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
			planned := []*registration{}
			for _, w := range p.workers {
				if w.recovering {
					continue
				}
				if (!w.dead && now.Sub(w.lastHeartbeat) >= p.config.DeadAfter) || (w.dead && !w.recovered) {
					if !w.dead {
						w.rejoinToken, w.acceptedToken = "", ""
					}
					w.dead, w.recovering, w.recovered, w.rejoinReady = true, true, false, false
					dead = append(dead, w)
				} else if !w.dead && !w.retryingMoves && len(w.pending) > 0 {
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
	// Retain completed hang-ups even after End or Status removes live metadata.
	call               *call
	lease              sessionstore.Lease
	candidate          *sessionstore.Lease
	plannedState       []byte
	planned            bool
	held               bool
	crashMargins       bool
	routed             netip.AddrPort
	excluded           map[netip.AddrPort]bool
	attempts           int
	attemptLimit       int
	transientResume    bool
	resumeState        []byte
	resumeTarget       *registration
	checkpointAge      time.Duration
	snapshotAge        time.Duration
	checkpointStoredAt time.Time
	outageStarted      time.Time
	checkpoint         mediaworker.CheckpointState
	envelope           bool
	margin             uint16
	reserve            uint32
	rtcpMargin         uint32
	replayPlan         *relay.ReplayPlan
}

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

func (p *Plane) takeover(ctx context.Context, source *registration, listed sessionstore.Lease, detected time.Time) {
	ctx, cancel := context.WithTimeout(ctx, takeoverBudget)
	defer cancel()
	p.mu.Lock()
	c := p.calls[listed.SessionID]
	if pending := source.pending[listed.SessionID]; pending != nil && pending.call != nil {
		c = pending.call
	}
	if c == nil {
		// A live lease is authoritative even if a racing move lost metadata.
		c = &call{id: listed.SessionID}
		p.calls[c.id] = c
	}
	p.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	p.takeoverLocked(ctx, source, c, listed, detected)
}

// takeoverLocked also recovers an ambiguous final export while Move holds c.mu.
func (p *Plane) takeoverLocked(ctx context.Context, source *registration, c *call, listed sessionstore.Lease, detected time.Time) {
	p.mu.Lock()
	existing := source.pending[c.id]
	p.mu.Unlock()
	lease, err := p.store.Get(ctx, c.id)
	if existing != nil && existing.candidate != nil {
		candidate := *existing.candidate
		if err == nil && lease.Worker == candidate.Worker && lease.Epoch == candidate.Epoch {
			p.mu.Lock()
			existing.lease = lease
			existing.candidate = nil
			p.mu.Unlock()
			listed = lease
		} else {
			settled, settleErr := p.resolveCandidate(ctx, candidate, &sessionstore.TransientError{Op: "pending transfer", Err: err, Candidate: &candidate})
			if settleErr == nil {
				p.mu.Lock()
				existing.lease = settled
				existing.candidate = nil
				p.mu.Unlock()
				lease, listed, err = settled, settled, nil
			} else {
				var transient *sessionstore.TransientError
				if errors.As(settleErr, &transient) && transient.Candidate == nil {
					p.mu.Lock()
					existing.candidate = nil
					p.mu.Unlock()
				} else {
					return
				}
			}
		}
	}
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
				if pending.planned {
					res.Kind = "move"
					res.DetectedAt = time.Time{}
					res.LastHeartbeat = time.Time{}
				}
				lost := true
				if c.hungUp.Load() {
					// Only End's completed hang-up explains a vanished pending lease.
					// Keep cleanup, including any retained relay hold.
					p.lose(c, pending.lease, pending.planned || pending.held)
					lost = false
					if !pending.planned {
						res.Kind = "ended"
					}
				}
				p.completeTakeover(source, c, pending.lease, &res, lost,
					fmt.Errorf("controlplane: pending takeover lease vanished: %w", err))
			}
		} else if err == nil {
			if existing != nil && existing.held {
				_, _ = p.relay.ReleaseSession(c.id, lease.Worker)
			}
			p.finishPending(source, c.id)
		}
		return
	}
	p.mu.Lock()
	if source.pending == nil {
		source.pending = make(map[string]*takeoverState)
	}
	pending := source.pending[c.id]
	if pending == nil && lease.Worker != source.addr {
		p.mu.Unlock()
		return
	}
	if pending == nil {
		pending = &takeoverState{call: c, lease: lease, routed: lease.Worker,
			excluded: map[netip.AddrPort]bool{source.addr: true}, attemptLimit: maxResumeAttempts}
		source.pending[c.id] = pending
	}
	if pending.outageStarted.IsZero() {
		pending.outageStarted = source.lastHeartbeat
		if pending.outageStarted.IsZero() {
			pending.outageStarted = detected
		}
	}
	r := p.relay
	lastHeartbeat := source.lastHeartbeat
	p.mu.Unlock()
	res := MoveResult{Kind: "takeover", ID: c.id, From: source.name, Start: detected,
		DetectedAt: detected, LastHeartbeat: lastHeartbeat, Result: mediaworker.HandoverResult{SessionID: c.id}}
	if pending.planned {
		res.Kind = "move"
		res.DetectedAt = time.Time{}
		res.LastHeartbeat = time.Time{}
	}
	// Publish only terminal outcomes. A transient error preserves the lease,
	// call and retry state without falsely reporting a lost takeover.
	complete := func(lost bool, cause error) {
		p.completeTakeover(source, c, pending.lease, &res, lost, cause)
	}

	for noTargetAttempts := 0; pending.transientResume || pending.attempts < pending.attemptLimit; {
		name := ""
		if pending.transientResume {
			p.mu.Lock()
			previous := p.owner(pending.lease.Worker)
			if previous != nil && previous == pending.resumeTarget && !previous.dead && !previous.draining {
				name = previous.name
			} else {
				// Confirmation only reuses bytes for the same eligible registration.
				// Transfer fences the old epoch; another target must reload the
				// last persisted counters and spend a fresh margin.
				pending.excluded[pending.lease.Worker] = true
				pending.transientResume = false
				pending.resumeState, pending.plannedState = nil, nil
				pending.crashMargins = true
			}
			p.mu.Unlock()
		}
		target, pickErr := p.pickExcluding(ctx, name, pending.excluded)
		if pickErr != nil {
			if !errors.Is(pickErr, ErrNoTarget) {
				return
			}
			// Eligibility can change after the pinned registration check.
			// Retry next tick instead of declaring this uncertain adoption lost.
			if pending.transientResume {
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
		if pending.transientResume && target != pending.resumeTarget {
			// Replacement can also occur between the registration check and
			// selection. Do not confirm an old payload on a new registration.
			p.unreserve(target)
			continue
		}
		res.To = target.name
		transferred := pending.lease
		if transferred.Worker != target.addr {
			transferred, err = p.transfer(ctx, pending.lease, target.addr)
			if err != nil {
				var transient *sessionstore.TransientError
				if errors.As(err, &transient) && transient.Candidate != nil {
					p.mu.Lock()
					pending.candidate = transient.Candidate
					p.mu.Unlock()
				}
				p.unreserve(target)
				return
			}
			p.mu.Lock()
			pending.lease = transferred
			p.mu.Unlock()
		}
		if replay, ok := r.(ReplayRelay); ok && !pending.planned && pending.replayPlan == nil {
			plan, gateErr := replay.BeginReplay(ctx, c.id, pending.routed, nil)
			if gateErr == nil {
				pending.replayPlan, pending.held = &plan, true
			} else if !errors.Is(gateErr, relay.ErrBufferDisabled) {
				p.unreserve(target)
				return
			}
		}
		// Gate and old-leg fencing precede snapshot I/O. Store errors must not
		// leave the previous owner able to send after its lease was transferred.
		if err = r.MoveSession(c.id, pending.routed, target.addr); err != nil {
			pending.routed = netip.AddrPort{}
			p.unreserve(target)
			return
		}
		pending.routed = target.addr
		state := pending.plannedState
		if pending.crashMargins {
			state = nil
		}
		if pending.transientResume {
			state = pending.resumeState
		}
		var stateErr error
		if state == nil {
			state, stateErr = p.store.GetState(ctx, c.id)
		}
		if stateErr != nil {
			p.unreserve(target)
			if errors.Is(stateErr, sessionstore.ErrNotFound) {
				complete(true, fmt.Errorf("controlplane: no takeover snapshot: %w", stateErr))
			}
			return
		}
		if pending.replayPlan != nil {
			replay := r.(ReplayRelay)
			inbound, indexErr := mediaworker.SnapshotInboundIndexes(state)
			if indexErr != nil {
				p.unreserve(target)
				complete(true, indexErr)
				return
			}
			plan, gateErr := replay.BeginReplay(ctx, c.id, source.addr, inbound)
			if gateErr == nil {
				pending.replayPlan, pending.held = &plan, true
			} else {
				p.unreserve(target)
				return
			}
		}
		margin, rtcpMargin := p.config.SequenceMargin, p.config.SRTCPIndexMargin
		if pending.transientResume {
			margin, rtcpMargin = pending.margin, pending.rtcpMargin
			reserve := p.config.CheckpointEnvelope.CallerSequenceReserve(pending.checkpoint, checkpointOutageBudget(ctx, pending.outageStarted))
			pending.envelope = pending.envelope || reserve > pending.reserve
			pending.reserve = max(pending.reserve, reserve)
			// An adopted tenure acknowledges idempotently before budget checks.
			// A still-unadopted tenure must recheck the caller's growing outage gap.
		} else if !pending.planned || pending.crashMargins {
			decision, checkpointErr := p.checkpointDecision(ctx, c.id, state, checkpointOutageBudget(ctx, pending.outageStarted))
			if checkpointErr != nil && !errors.Is(checkpointErr, mediaworker.ErrSequenceBudgetExhausted) && !errors.Is(checkpointErr, sessionstore.ErrUnsafeCheckpointClock) && !errors.Is(checkpointErr, errUnsafeCheckpointState) {
				p.unreserve(target)
				if errors.Is(checkpointErr, sessionstore.ErrNotFound) {
					complete(true, checkpointErr)
				}
				return
			}
			pending.checkpointAge, pending.checkpoint = decision.age, decision.info
			pending.snapshotAge, pending.checkpointStoredAt = decision.snapshotAge, decision.storedAt
			copiedAt := time.Now().Add(-decision.snapshotAge)
			if copiedAt.Before(pending.outageStarted) {
				pending.outageStarted = copiedAt
			}
			pending.envelope = pending.envelope || decision.outside
			if checkpointErr != nil {
				p.unreserve(target)
				complete(true, checkpointErr)
				return
			}
			margin, rtcpMargin = decision.margin, decision.rtcpMargin
			pending.reserve = decision.reserve
			metrics.CheckpointAge.Observe(pending.checkpointAge.Seconds())
		}
		res.CheckpointAge, res.Checkpoint = pending.checkpointAge, pending.checkpoint
		res.SnapshotAge, res.CheckpointStoredAt = pending.snapshotAge, pending.checkpointStoredAt
		if pending.planned && !pending.crashMargins {
			margin, rtcpMargin = 0, 0
		}
		pending.margin, pending.rtcpMargin = margin, rtcpMargin
		budget, budgetErr := maxResumeAttempts, error(nil)
		if (!pending.planned || pending.crashMargins) && !pending.transientResume {
			budget, budgetErr = mediaworker.SequenceResumeAttemptsWithReserve(state, margin, pending.reserve)
		}
		if budgetErr != nil {
			p.unreserve(target)
			complete(true, budgetErr)
			return
		}
		if !pending.transientResume {
			pending.attemptLimit = min(pending.attemptLimit, pending.attempts+budget)
		}
		if !pending.transientResume && pending.attempts >= pending.attemptLimit {
			p.unreserve(target)
			complete(true, mediaworker.ErrSequenceBudgetExhausted)
			return
		}
		res.Result.StateBytes = len(state)
		started := time.Now()
		if !pending.transientResume {
			pending.attempts++
			pending.resumeState = state
			pending.resumeTarget = target
		}
		if !pending.planned {
			pending.plannedState = nil
		}
		_, err = target.worker.ResumeSession(state, mediaworker.ResumeOptions{Lease: transferred,
			Context: ctx, CallerSequenceReserve: pending.reserve, CheckpointAge: pending.checkpointAge, SnapshotAge: pending.snapshotAge, CheckpointStoredAt: pending.checkpointStoredAt, SequenceMargin: margin, SRTCPIndexMargin: rtcpMargin,
			RelayReplay: pending.replayPlan != nil && pending.replayPlan.Complete})
		res.Result.Resume += time.Since(started)
		p.unreserve(target)
		if err == nil {
			if pending.replayPlan != nil {
				replayed, replayErr := r.(ReplayRelay).ReplaySession(ctx, c.id, target.addr)
				res.Result.RelayReplayPackets = replayed.Packets
				res.Result.RelayReplayDuration = replayed.Duration
				res.Result.RelayReplayComplete = pending.replayPlan.Complete
				res.Result.HoldExpired = errors.Is(replayErr, relay.ErrHoldExpired)
				if replayErr != nil && !res.Result.HoldExpired {
					pending.transientResume = true
					return
				}
				pending.held = false
			}
			now := time.Now()
			c.lastMove, c.lastMoveKind = &now, res.Kind
			c.moveCount++
			if !pending.planned {
				c.takeoverCount++
			}
			if pending.replayPlan == nil && (pending.planned || pending.held) {
				held, releaseErr := r.ReleaseSession(c.id, target.addr)
				res.Result.HeldPackets = held
				res.Result.HoldExpired = errors.Is(releaseErr, relay.ErrHoldExpired)
			}
			complete(false, nil)
			return
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, privateapi.ErrUncertain) {
			pending.transientResume = true
			return
		}
		if errors.Is(err, mediaworker.ErrSequenceBudgetExhausted) || errors.Is(err, mediaworker.ErrSRTCPIndexExhausted) {
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
	record := res.Kind == "takeover" || res.Kind == "ended" || lost
	if lost {
		p.mu.Lock()
		pending := source.pending[c.id]
		held := pending != nil && pending.held
		p.mu.Unlock()
		p.lose(c, lease, res.Kind == "move" || held)
	}
	p.mu.Lock()
	pending := source.pending[c.id]
	if pending != nil {
		res.CheckpointAge, res.Checkpoint = pending.checkpointAge, pending.checkpoint
		res.SnapshotAge, res.CheckpointStoredAt = pending.snapshotAge, pending.checkpointStoredAt
		res.SequenceMargin, res.SRTCPIndexMargin = pending.margin, pending.rtcpMargin
		if pending.envelope && (!lost || checkpointEnvelopeLoss(cause)) {
			res.CheckpointPolicy = "scaled"
			if lost {
				res.CheckpointPolicy = "definitive-loss"
			}
			metrics.CheckpointEnvelopeEvents.WithLabelValues(res.CheckpointPolicy).Inc()
		}
	}
	p.mu.Unlock()
	p.finishPending(source, c.id)
	res.End = time.Now()
	res.Result.Duration = res.End.Sub(res.Start)
	res.Lost = lost
	if cause != nil {
		res.Error = cause.Error()
	}
	p.mu.Lock()
	if record {
		p.recordTakeover(*res)
	}
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

func (p *Plane) lose(c *call, lease sessionstore.Lease, planned bool) {
	// Cleanup outlives a cancelled detector context, as with move rollback.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = p.store.Release(ctx, lease)
	p.deleteFrames(c.id)
	p.mu.Lock()
	r := p.relay
	p.mu.Unlock()
	// A lost final export must free its bounded relay hold immediately.
	if planned {
		_, _ = r.ReleaseSession(c.id, netip.AddrPort{})
	}
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
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leases, err := p.store.ListByWorker(ctx, old.addr)
	if err != nil || len(leases) != 0 {
		return errors.New("controlplane: replacement still has leases")
	}
	p.mu.Lock()
	if p.workers[name] != old || !old.dead || !old.recovered || old.recovering || old.reserved != 0 {
		p.mu.Unlock()
		return errors.New("controlplane: replacement raced with rejoin or recovery")
	}
	for otherName, w := range p.workers {
		if otherName != name && w.addr == addr {
			p.mu.Unlock()
			return errors.New("controlplane: duplicate worker address")
		}
	}
	p.workers[name] = &registration{name: name, addr: addr, worker: worker, lastHeartbeat: time.Now()}
	p.mu.Unlock()
	if w, ok := worker.(interface{ StartHeartbeats(mediaworker.Heartbeats) }); ok {
		w.StartHeartbeats(p)
	}
	return nil
}

func (p *Plane) retryPlannedMoves(ctx context.Context, w *registration, now time.Time) {
	p.mu.Lock()
	leases := make([]sessionstore.Lease, 0, len(w.pending))
	for _, pending := range w.pending {
		if pending.planned || pending.held {
			leases = append(leases, pending.lease)
		}
	}
	p.mu.Unlock()
	jobs := make(chan sessionstore.Lease)
	var wg sync.WaitGroup
	for range min(len(leases), p.config.TakeoverParallelism) {
		wg.Go(func() {
			for lease := range jobs {
				p.takeover(ctx, w, lease, now)
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

// Storage disappearance, no target, and unrelated resume errors are losses,
// but do not imply that the checkpoint envelope caused the loss.
func checkpointEnvelopeLoss(err error) bool {
	return errors.Is(err, mediaworker.ErrSequenceBudgetExhausted) || errors.Is(err, mediaworker.ErrSRTCPIndexExhausted) || errors.Is(err, sessionstore.ErrUnsafeCheckpointClock)
}
