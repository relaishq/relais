package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// moveTransfer survives transient errors even after Transfer changed the owner.
// Its fields are accessed under the call lock; pending membership uses p.mu.
type moveTransfer struct {
	exported       map[uint32]relay.ConsumedInput
	releaseTo      netip.AddrPort
	released       bool
	source, target *registration
	relay          Relay
	transferred    bool
	route          moveRoute
	resume         resumeAttempt
	// Retain completed hang-ups even after End or Status removes live metadata.
	call                    *call
	lease                   sessionstore.Lease
	candidate               *sessionstore.Lease
	plannedState            []byte
	planned                 bool
	held                    bool
	plannedHoldTarget       *registration // A planned hold may auto-release to this possibly adopted target.
	snapshot                recoverySnapshot
	routed                  netip.AddrPort
	routedLease             sessionstore.Lease
	excluded                map[netip.AddrPort]bool
	attempts                int
	attemptLimit            int
	firstDetection          time.Time
	targetDeadline          time.Time
	transferIssued          bool
	sourceChecked           time.Time
	sourceNeedsHeartbeat    bool
	unrequested             bool
	rebuilding              bool
	deathCycle              uint64
	transientResume         bool
	budgetRollback          bool // Retained after rollback itself refused move memory.
	budgetStarted           time.Time
	budgetNext              time.Time
	budgetBackoff           time.Duration
	budgetExcluded          map[netip.AddrPort]bool
	agentFailure            error
	checkpointAge           time.Duration
	snapshotAge             time.Duration
	checkpointStoredAt      time.Time
	outageStarted           time.Time
	checkpoint              mediaworker.CheckpointState
	envelope                bool
	reserve                 uint32
	replayPlan              *relay.ReplayPlan
	replayDuplicationWindow time.Duration
	replayAnchors           map[uint32]uint64
}

func (m *moveRecovery) recover(ctx context.Context, source *registration, listed sessionstore.Lease, detected time.Time, cycles ...uint64) *MoveResult {
	p := m.plane
	ctx, cancel := context.WithTimeout(ctx, takeoverBudget)
	defer cancel()
	p.mu.Lock()
	if len(cycles) > 0 && source.observation.deathCycle != cycles[0] {
		p.mu.Unlock()
		return nil
	}
	if source.observation.deathPreserved && m.pending[source][listed.SessionID] == nil {
		p.mu.Unlock()
		return nil
	}
	c := p.calls[listed.SessionID]
	if pending := m.pending[source][listed.SessionID]; pending != nil && pending.call != nil {
		c = pending.call
	}
	p.mu.Unlock()
	if c == nil {
		// A stale inventory must not recreate a call already terminally counted.
		current, err := p.store.Get(ctx, listed.SessionID)
		if err != nil {
			return nil
		}
		p.mu.Lock()
		c = p.calls[listed.SessionID]
		if c == nil {
			c = &call{id: listed.SessionID, lease: current}
			p.calls[c.id] = c
		}
		p.mu.Unlock()
	}
	if err := c.lockOperator(ctx, true); err != nil {
		return nil
	}
	defer c.mu.Unlock()
	p.mu.Lock()
	stale := len(cycles) > 0 && source.observation.deathCycle != cycles[0]
	p.mu.Unlock()
	if stale {
		return nil
	}
	return m.continueLocked(ctx, source, c, listed, detected)
}

// takeoverLocked also recovers an ambiguous final export while Move holds c.mu.
// A non-nil outcome has completed terminal accounting, independent of the
// bounded status history. Nil means recovery is pending, ownership changed, or
// the call was already terminal (its pending work is then cleared).
func (m *moveRecovery) continueLocked(ctx context.Context, source *registration, c *call, listed sessionstore.Lease, detected time.Time) (outcome *MoveResult) {
	p := m.plane
	if err := p.beginIntent(ctx, c, "takeover", listed); err != nil {
		return nil
	}
	defer p.finishIntent(c)
	if c.terminal {
		m.finishPending(source, c.id)
		return nil
	}
	p.mu.Lock()
	existing := m.pending[source][c.id]
	if source.observation.deathPreserved && (existing == nil || existing.unrequested) {
		p.mu.Unlock()
		return nil
	}
	if existing != nil && existing.routedLease.Epoch == 0 && existing.routed == listed.Worker {
		existing.routedLease = listed
	}
	p.mu.Unlock()
	// Read/reconcile ownership before declaring pressure loss: a committed
	// hangup or an uncertain Transfer candidate takes precedence over the cap.
	if existing != nil && !existing.budgetStarted.IsZero() &&
		time.Since(existing.budgetStarted) < takeoverBudget && time.Now().Before(existing.budgetNext) {
		return nil
	}
	lease, err := p.store.Get(ctx, c.id)
	if err == nil {
		c.rememberLease(lease)
	}
	if existing != nil && existing.candidate != nil {
		candidate := *existing.candidate
		if err == nil && lease.Worker == candidate.Worker && lease.Epoch == candidate.Epoch {
			p.mu.Lock()
			existing.lease = lease
			existing.candidate = nil
			p.mu.Unlock()
			listed = lease
		} else {
			settled, settleErr := m.resolveCandidate(ctx, candidate, &sessionstore.TransientError{Op: "pending transfer", Err: err, Candidate: &candidate})
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
		if errors.Is(err, sessionstore.ErrNotFound) || errors.Is(err, sessionstore.ErrEnded) {
			p.mu.Lock()
			pending := m.pending[source][c.id]
			lastHeartbeat := source.observation.recoveryHeartbeat()
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
				if errors.Is(err, sessionstore.ErrEnded) || c.hungUp.Load() {
					c.hungUp.Store(true)
					// A committed release explains a normally ended pending lease.
					// Keep cleanup, including any retained relay hold.
					p.lose(c, pending.lease, pending.planned || pending.held, pending.routedLease)
					lost = false
					if !pending.planned {
						res.Kind = "ended"
					}
				}
				if lost {
					res.LossCause = "ownership_expired"
					if !pending.budgetStarted.IsZero() {
						res.LossCause = "move_budget_exhausted"
						err = ErrMoveBudgetExhausted
					}
				}
				m.complete(source, c, pending.lease, &res, lost,
					fmt.Errorf("controlplane: pending takeover lease vanished: %w", err))
				outcome = &res
			}
		} else if err == nil {
			if existing != nil && existing.held {
				_, _ = p.relay.ReleaseSession(c.id, lease.Worker)
			}
			m.finishPending(source, c.id)
		}
		return
	}
	p.mu.Lock()
	pending := m.pending[source][c.id]
	if pending == nil && lease.Worker != source.addr {
		p.mu.Unlock()
		return
	}
	if pending == nil {
		if m.pending[source] == nil {
			m.pending[source] = make(map[string]*moveTransfer)
		}
		pending = &moveTransfer{call: c, lease: lease, routed: lease.Worker, routedLease: lease,
			excluded: map[netip.AddrPort]bool{source.addr: true}, attemptLimit: maxResumeAttempts,
			unrequested: source.observation.dead, deathCycle: source.observation.deathCycle}
		m.pending[source][c.id] = pending
	}
	if pending.outageStarted.IsZero() {
		pending.outageStarted = source.observation.recoveryHeartbeat()
		if pending.outageStarted.IsZero() {
			pending.outageStarted = detected
		}
	}
	if pending.unrequested && pending.firstDetection.IsZero() {
		pending.firstDetection = detected
		if first := source.observation.deathDetected; source.observation.dead && !first.IsZero() && first.Before(detected) {
			pending.firstDetection = first
		}
		pending.targetDeadline = pending.firstDetection.Add(takeoverBudget)
		anchor := source.observation.recoveryHeartbeat()
		if anchor.IsZero() {
			anchor = detected
		}
		if limit := anchor.Add(targetOutageBudget); limit.Before(pending.targetDeadline) {
			pending.targetDeadline = limit
		}
		if limit, ok := ctx.Deadline(); ok && limit.Before(pending.targetDeadline) {
			pending.targetDeadline = limit
		}
	}
	if pending.unrequested {
		detected = pending.firstDetection
	}
	r := p.relay
	lastHeartbeat := source.observation.recoveryHeartbeat()
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
		if errors.Is(cause, ErrTargetWaitExpired) && pending.agentFailure != nil {
			cause = fmt.Errorf("controlplane: no compatible agent target: %w", errors.Join(cause, pending.agentFailure))
		}
		m.complete(source, c, pending.lease, &res, lost, cause)
		outcome = &res
	}

	for noTargetAttempts := 0; pending.transientResume || !pending.budgetStarted.IsZero() || pending.attempts < pending.attemptLimit; {
		p.mu.Lock()
		stale := pending.unrequested && pending.deathCycle != source.observation.deathCycle
		p.mu.Unlock()
		if stale {
			return
		}
		if m.preserveSource(ctx, source, pending) {
			return
		}
		if !pending.budgetStarted.IsZero() && time.Since(pending.budgetStarted) >= takeoverBudget {
			res.LossCause = "move_budget_exhausted"
			complete(true, ErrMoveBudgetExhausted)
			return
		}
		name := ""
		if pending.transientResume {
			p.mu.Lock()
			previous := p.owner(pending.lease.Worker)
			if previous != nil && previous == pending.resume.target && previous.observation.knowledge().resumeRetry {
				name = previous.name
			} else {
				// Confirmation only reuses bytes for the same eligible registration.
				// Transfer fences the old epoch; another target must reload the
				// last persisted counters and spend a fresh margin.
				pending.excluded[pending.lease.Worker] = true
				pending.transientResume = false
				pending.resume.state, pending.plannedState = nil, nil
				pending.snapshot = recoveryCheckpoint
			}
			p.mu.Unlock()
		}
		excluded := pending.excluded
		if len(pending.budgetExcluded) > 0 {
			excluded = make(map[netip.AddrPort]bool, len(pending.excluded)+len(pending.budgetExcluded))
			for addr, skip := range pending.excluded {
				excluded[addr] = skip
			}
			for addr := range pending.budgetExcluded {
				excluded[addr] = true
			}
		}
		// An exported call cannot stay on its original running session. All
		// retained recovery follows takeover's overflow rule to preserve it.
		target, pickErr := p.pickCapacity(ctx, name, excluded, true, c.id)
		if pickErr != nil {
			if !errors.Is(pickErr, ErrNoTarget) && !errors.Is(pickErr, ErrAtCapacity) {
				return
			}
			// Eligibility can change after the pinned registration check.
			// Retry next tick instead of declaring this uncertain adoption lost.
			if pending.transientResume {
				return
			}
			if !pending.budgetStarted.IsZero() {
				// All currently eligible targets refused capacity. Reconsider them
				// after backoff, while retaining the same absolute retry deadline.
				pending.budgetExcluded = nil
				pending.budgetBackoff = min(max(50*time.Millisecond, pending.budgetBackoff*2), 400*time.Millisecond)
				pending.budgetNext = time.Now().Add(pending.budgetBackoff)
				return
			}
			if !pending.unrequested {
				// Bootstrap observes the command link and heartbeat separately.
				// Wait for either missing observation within the existing adoption
				// budget, without changing ordinary planned/no-target recovery.
				if pending.rebuilding && m.observationTime().Before(pending.targetDeadline) {
					p.mu.Lock()
					returning := false
					for _, w := range p.workers {
						o := &w.observation
						// Before the first heartbeat, lastHeartbeat dates registration,
						// not liveness. Only the rebuild deadline bounds that wait.
						if p.links != nil && p.links.entries[w.name] != nil && !excluded[w.addr] &&
							(o.linkDown || o.livenessUnknown) && !o.dead && !o.draining && !o.recovering &&
							(o.livenessUnknown || m.observationTime().Sub(o.lastHeartbeat) < p.config.DeadAfter) {
							returning = true
						}
					}
					p.mu.Unlock()
					if returning {
						if waitErr := m.waitForTarget(ctx, pending); waitErr != nil && !errors.Is(waitErr, ErrTargetWaitExpired) {
							return
						}
						continue
					}
				}
				noTargetAttempts++
				if noTargetAttempts == maxResumeAttempts {
					cause := pickErr
					if pending.agentFailure != nil {
						cause = fmt.Errorf("controlplane: no compatible agent target: %w", pending.agentFailure)
					}
					complete(true, cause)
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(25 * time.Millisecond):
				}
				continue
			}
			if waitErr := m.waitForTarget(ctx, pending); waitErr != nil {
				if errors.Is(waitErr, ErrTargetWaitExpired) {
					if m.preserveSource(ctx, source, pending) {
						return
					}
					complete(true, errors.Join(waitErr, pickErr))
				}
				return
			}
			continue
		}
		if pending.transientResume && target != pending.resume.target {
			// Replacement can also occur between the registration check and
			// selection. Do not confirm an old payload on a new registration.
			p.unreserve(target)
			continue
		}
		// A returning source wins even when a target returned in the same tick.
		if m.preserveSource(ctx, source, pending) {
			p.unreserve(target)
			return
		}
		if m.beforeTransfer != nil {
			m.beforeTransfer()
		}
		p.mu.Lock()
		// Confirmation I/O can outlive preservation and even the next death.
		// Only a still-registered job for this cycle may issue Transfer.
		if pending.unrequested && (source.observation.deathPreserved ||
			pending.deathCycle != source.observation.deathCycle || m.pending[source][c.id] != pending) {
			p.mu.Unlock()
			p.unreserve(target)
			return
		}
		// Serialize the last liveness decision with issuing Transfer. A heartbeat
		// racing selection sends us back through exact source confirmation.
		if pending.unrequested && !source.observation.deathTransferIssued && pending.sourceNeedsHeartbeat && source.observation.lastHeartbeat.After(pending.sourceChecked) {
			p.mu.Unlock()
			p.unreserve(target)
			continue
		}
		pending.transferIssued = true
		if pending.unrequested {
			source.observation.deathTransferIssued = true
		}
		p.mu.Unlock()
		res.To = target.name
		transferred := pending.lease
		if transferred.Worker != target.addr {
			transferred, err = m.transfer(ctx, pending.lease, target.addr)
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
		if err = p.settleDeadRelayChange(ctx, transferred); err != nil {
			p.unreserve(target)
			return
		}
		// A planned hold may already have flushed to this target after its
		// resume reply was lost. Confirmation must never replay its live input
		// against the old export checkpoint. A different target can still replay.
		confirmPlannedTarget := pending.transientResume && target == pending.plannedHoldTarget
		if replay, ok := r.(ReplayRelay); ok && !pending.planned && pending.replayPlan == nil && !confirmPlannedTarget {
			// The route follows adoption; the gate's source must stay the same
			// as checkpoint preparation, including after a planned hold expires.
			plan, gateErr := replay.BeginReplay(ctx, c.id, source.addr, nil)
			if gateErr == nil {
				p.mu.Lock()
				pending.replayPlan, pending.held = &plan, true
				p.mu.Unlock()
			} else if !errors.Is(gateErr, relay.ErrBufferDisabled) && (!pending.held || !errors.Is(gateErr, relay.ErrHeld)) {
				p.unreserve(target)
				return
			} else if errors.Is(gateErr, relay.ErrHeld) {
				pending.plannedHoldTarget = target
			}
		}
		// Gate and old-leg fencing precede snapshot I/O. Store errors must not
		// leave the previous owner able to send after its lease was transferred.
		if pending.planned && pending.snapshot != recoveryCheckpoint {
			// The retained export succeeded: forward its pre-fence output first.
			err = moveAfterExport(r, c.id, pending.routed, target.addr, transferred.Epoch)
		} else if tenureRelay, ok := r.(TenureRelay); ok {
			err = tenureRelay.MoveSessionAtEpoch(c.id, pending.routed, target.addr, transferred.Epoch)
		} else {
			err = r.MoveSession(c.id, pending.routed, target.addr)
		}
		if err != nil {
			pending.routed = netip.AddrPort{}
			p.unreserve(target)
			return
		}
		pending.routed = target.addr
		pending.routedLease = transferred
		state := pending.plannedState
		if pending.snapshot == recoveryCheckpoint {
			state = nil
		}
		if pending.transientResume {
			state = pending.resume.state
		}
		var stateErr error
		var tupleMetadata *sessionstore.Checkpoint
		if state == nil {
			state, tupleMetadata, stateErr = mediaworker.ReadRecoverySnapshot(ctx, p.store, c.id)
		}
		if stateErr != nil {
			p.unreserve(target)
			if errors.Is(stateErr, sessionstore.ErrNotFound) || errors.Is(stateErr, sessionstore.ErrUnsafeCheckpoint) || errors.Is(stateErr, sessionstore.ErrUnsafeCheckpointClock) {
				complete(true, fmt.Errorf("controlplane: no takeover snapshot: %w", stateErr))
			}
			return
		}
		margin, rtcpMargin := p.config.SequenceMargin, p.config.SRTCPIndexMargin
		if pending.transientResume {
			margin, rtcpMargin = pending.resume.margin, pending.resume.rtcpMargin
			reserve := p.config.CheckpointEnvelope.CallerSequenceReserve(pending.checkpoint, checkpointOutageBudget(ctx, pending.outageStarted))
			pending.envelope = pending.envelope || reserve > pending.reserve
			pending.reserve = max(pending.reserve, reserve)
			// An adopted tenure acknowledges idempotently before budget checks.
			// A still-unadopted tenure must recheck the caller's growing outage gap.
		} else if !pending.planned || pending.snapshot == recoveryCheckpoint {
			decision, checkpointErr := m.checkpointDecision(ctx, c.id, state, checkpointOutageBudget(ctx, pending.outageStarted), tupleMetadata)
			if checkpointErr != nil && !errors.Is(checkpointErr, mediaworker.ErrSequenceBudgetExhausted) && !errors.Is(checkpointErr, sessionstore.ErrUnsafeCheckpointClock) && !errors.Is(checkpointErr, errUnsafeCheckpointState) && !errors.Is(checkpointErr, sessionstore.ErrUnsafeCheckpoint) {
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
		// Only fix the replay filter after metadata validates these exact bytes.
		// A superseded checkpoint leaves the gate unprepared for a fresh read.
		if pending.replayPlan != nil {
			replay := r.(ReplayRelay)
			inbound, indexErr := mediaworker.SnapshotReplayIndexes(state)
			if indexErr != nil {
				p.unreserve(target)
				complete(true, indexErr)
				return
			}
			anchors, indexErr := mediaworker.SnapshotInboundIndexes(state)
			if indexErr != nil {
				p.unreserve(target)
				complete(true, indexErr)
				return
			}
			pending.replayAnchors = anchors
			plan, gateErr := replay.BeginReplay(ctx, c.id, source.addr, inbound)
			if gateErr == nil {
				p.mu.Lock()
				pending.replayPlan, pending.held = &plan, true
				p.mu.Unlock()
				pending.replayDuplicationWindow = max(pending.replayDuplicationWindow, pending.snapshotAge)
			} else {
				p.unreserve(target)
				return
			}
		}
		res.CheckpointAge, res.Checkpoint = pending.checkpointAge, pending.checkpoint
		res.SnapshotAge, res.CheckpointStoredAt = pending.snapshotAge, pending.checkpointStoredAt
		if pending.planned && pending.snapshot != recoveryCheckpoint {
			margin, rtcpMargin = 0, 0
		}
		pending.resume.margin, pending.resume.rtcpMargin = margin, rtcpMargin
		budget, budgetErr := maxResumeAttempts, error(nil)
		if (!pending.planned || pending.snapshot == recoveryCheckpoint) && !pending.transientResume {
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
		if !pending.planned {
			pending.plannedState = nil
		}
		kind := agent.Takeover
		if pending.planned && pending.snapshot != recoveryCheckpoint {
			kind = agent.PlannedMove
		}
		recovery, canRecoverReplay := target.worker.(ReplayRecoveryWorker)
		var duplicateWindows []agent.DuplicateWindow
		if pending.replayPlan != nil {
			duplicateWindows = agentReplayWindows(pending.replayPlan.Windows, pending.replayAnchors)
		}
		// Retained input takes precedence over cached pictures even if a gap
		// broke codec continuity. Cached replay would gate and discard this video.
		// Require the post-drain PLI backstop before deferring resume recovery.
		relayReplay := pending.replayPlan != nil && pending.replayPlan.Complete && canRecoverReplay
		relayInputReplay := pending.replayPlan != nil && !pending.replayPlan.Complete && len(pending.replayPlan.Windows) > 0 && canRecoverReplay
		if pending.transientResume {
			// The first request may not have reached the worker. Match its
			// recovery flags to the retained post-drain recovery obligation.
			relayReplay, relayInputReplay = pending.resume.relayReplay, pending.resume.relayInputReplay
		} else {
			pending.attempts++
			pending.resume.state = state
			pending.resume.target = target
			// A new attempt records its flags; confirmations reuse them even
			// when the current relay plan has gained or lost selected input.
			pending.resume.relayReplay, pending.resume.relayInputReplay = relayReplay, relayInputReplay
			pending.resume.deferredRecovery = relayReplay || relayInputReplay
		}
		_, err = target.worker.ResumeSession(state, mediaworker.ResumeOptions{Kind: kind, RelayInputReplay: relayInputReplay, InputMayBeDuplicated: len(duplicateWindows) > 0, DuplicateWindows: duplicateWindows, Lease: transferred,
			Context: ctx, CallerSequenceReserve: pending.reserve, CheckpointAge: pending.checkpointAge, SnapshotAge: pending.snapshotAge, CheckpointStoredAt: pending.checkpointStoredAt, SequenceMargin: margin, SRTCPIndexMargin: rtcpMargin, RelayReplay: relayReplay})
		res.Result.Resume += time.Since(started)
		p.unreserve(target)
		if !errors.Is(err, mediaworker.ErrMoveBudget) || errors.Is(err, privateapi.ErrUncertain) {
			// Capacity is no longer the reason for retry. An uncertain reply may
			// hide adoption and must use the existing confirmation rules.
			pending.budgetStarted, pending.budgetNext = time.Time{}, time.Time{}
			pending.budgetExcluded = nil
			pending.budgetBackoff = 0
		}
		if err == nil {
			if pending.replayPlan != nil {
				replayed, replayErr := r.(ReplayRelay).ReplaySession(ctx, c.id, target.addr)
				res.Result.DuplicateWindows = deliveredReplayWindows(pending.replayPlan.Windows, replayed.Windows, pending.replayAnchors)
				res.Result.InputMayBeDuplicated = len(res.Result.DuplicateWindows) > 0
				if res.Result.InputMayBeDuplicated {
					res.Result.InputDuplicationWindow = pending.replayDuplicationWindow
				}
				res.Result.RelayReplayPackets = replayed.Packets
				res.Result.RelayReplayDuration = replayed.Duration
				res.Result.RelayReplayComplete = pending.replayPlan.Complete && replayed.Complete && replayErr == nil
				res.Result.RelayReplayDrops = replayed.Dropped
				res.Result.RelayReplaySendFailures = replayed.SendFailures
				res.Result.HoldExpired = replayed.Expired || errors.Is(replayErr, relay.ErrHoldExpired)
				if replayErr != nil && !res.Result.HoldExpired {
					pending.transientResume = true
					return
				}
				if pending.resume.deferredRecovery && !res.Result.RelayReplayComplete && canRecoverReplay {
					if err := recovery.RequestKeyframe(ctx, c.id); err != nil {
						pending.transientResume = true
						return
					}
					res.Result.RelayReplayRecoveryPLI = true
				}
				p.mu.Lock()
				pending.held = false
				p.mu.Unlock()
			}
			if pending.replayPlan == nil && (pending.planned || pending.held) {
				// A margin-zero resume of the retained export is exact (ADR 0009).
				// Crash margins resume from a checkpoint that may predate it.
				var exported map[uint32]relay.ConsumedInput
				if pending.planned && pending.snapshot != recoveryCheckpoint && margin == 0 {
					exported, _ = mediaworker.SnapshotConsumedInput(state)
				}
				releaseErr := releaseHold(r, c.id, target.addr, exported, &res.Result)
				res.Result.HoldExpired = errors.Is(releaseErr, relay.ErrHoldExpired)
				if res.Result.HoldExpired && confirmPlannedTarget && canRecoverReplay {
					// Idempotent resume does not re-request video recovery. Account
					// for the expired hold and recover any gap without old ciphertext.
					if err := recovery.RequestKeyframe(ctx, c.id); err != nil {
						pending.transientResume = true
						return
					}
					res.Result.RelayReplayRecoveryPLI = true
				}
			}
			if pending.budgetRollback && target == source {
				// Only a move-budget rollback has these semantics. Legacy
				// uncertain transfers keep their established move accounting.
				res.Result.RolledBack = true
			} else {
				now := time.Now()
				c.stateMu.Lock()
				c.lastMove, c.lastMoveKind = &now, res.Kind
				c.moveCount++
				if !pending.planned {
					c.takeoverCount++
				}
				c.stateMu.Unlock()
			}
			complete(false, nil)
			return
		}
		if errors.Is(err, mediaworker.ErrMoveBudget) && !errors.Is(err, privateapi.ErrUncertain) {
			// This typed refusal proves no adoption and no counter publication.
			// Try another target without pinning or spending an adoption attempt.
			if pending.budgetStarted.IsZero() {
				pending.budgetStarted = time.Now()
			}
			if pending.budgetExcluded == nil {
				pending.budgetExcluded = make(map[netip.AddrPort]bool)
			}
			pending.budgetExcluded[target.addr] = true
			if !pending.transientResume {
				pending.attempts--
			}
			pending.transientResume = false
			continue
		}
		// Explicit transport uncertainty can hide an adopted target even with
		// an agent cause. Confirm that target before treating typed failures as
		// definitive, including callback deadlines.
		agentFailure := errors.Is(err, agentv2.ErrIncompatibleAgent) || errors.Is(err, agent.ErrRestore) || errors.Is(err, agent.ErrVersion) || errors.Is(err, agent.ErrStateTooLarge) || errors.Is(err, agent.ErrSave)
		if errors.Is(err, privateapi.ErrUncertain) || (!agentFailure && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))) {
			pending.transientResume = true
			return
		}
		if agentFailure {
			pending.agentFailure = err
		}
		if errors.Is(err, mediaworker.ErrSequenceBudgetExhausted) || errors.Is(err, mediaworker.ErrSRTCPIndexExhausted) || errors.Is(err, mediaworker.ErrDTLSSequenceExhausted) {
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
	return
}

// completeTakeover runs under the call lock; loss cleanup outlives detector
// cancellation. Expired pending leases are terminal too, never silent exits.
func (m *moveRecovery) complete(source *registration, c *call, lease sessionstore.Lease,
	res *MoveResult, lost bool, cause error) {
	p := m.plane
	if c.terminal {
		m.finishPending(source, c.id)
		return
	}
	if lost {
		p.mu.Lock()
		c.terminal = true
		p.mu.Unlock()
		if res.LossCause == "" {
			res.LossCause = "recovery_failed"
			if errors.Is(cause, ErrMoveBudgetExhausted) {
				res.LossCause = "move_budget_exhausted"
			}
			if errors.Is(cause, sessionstore.ErrNotFound) {
				res.LossCause = "checkpoint_missing"
			} else if errors.Is(cause, sessionstore.ErrUnsafeCheckpoint) || checkpointEnvelopeLoss(cause) {
				res.LossCause = "checkpoint_unsafe"
			}
		}
	}
	record := res.Kind == "takeover" || res.Kind == "ended" || lost
	if lost {
		p.mu.Lock()
		pending := m.pending[source][c.id]
		held := pending != nil && pending.held
		routedLease := sessionstore.Lease{}
		if pending != nil {
			routedLease = pending.routedLease
		}
		p.mu.Unlock()
		p.lose(c, lease, res.Kind == "move" || held, routedLease)
	}
	p.mu.Lock()
	pending := m.pending[source][c.id]
	if pending != nil {
		res.CheckpointAge, res.Checkpoint = pending.checkpointAge, pending.checkpoint
		res.SnapshotAge, res.CheckpointStoredAt = pending.snapshotAge, pending.checkpointStoredAt
		res.SequenceMargin, res.SRTCPIndexMargin = pending.resume.margin, pending.resume.rtcpMargin
		if pending.envelope && (!lost || checkpointEnvelopeLoss(cause)) {
			res.CheckpointPolicy = "scaled"
			if lost {
				res.CheckpointPolicy = "definitive-loss"
			}
			metrics.CheckpointEnvelopeEvents.WithLabelValues(res.CheckpointPolicy).Inc()
		}
	}
	p.mu.Unlock()
	m.finishPending(source, c.id)
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
	metricKind := res.Kind
	if !lost && c.hungUp.Load() {
		metricKind = "ended"
	} else if pending != nil && pending.held && !pending.planned {
		// Uncertain export uses crash recovery for safety, but its initiating
		// operation was a move. Preserve status and recovery semantics.
		metricKind = "move"
	}
	switch metricKind {
	case "takeover":
		if lost || cause != nil {
			p.metrics.takeoverErrors.Add(1)
		} else {
			p.metrics.takeovers.Add(1)
		}
	case "move":
		if lost || cause != nil {
			// An immediate rollback failure is counted by move's defer.
			// Only retained recovery completes a separate attempt here.
			if pending != nil {
				p.metrics.moveErrors.Add(1)
			}
		} else if !res.Result.RolledBack {
			p.metrics.moves.Add(1)
		}
	}
	if lost {
		p.lostCount++
		p.lossesByCause[res.LossCause]++
	}
	p.mu.Unlock()
	// Publish terminal accounting before a slow or failed cleanup attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.cleanupExpired(ctx, c)
}

func (m *moveRecovery) finishPending(source *registration, id string) {
	p := m.plane
	p.mu.Lock()
	delete(m.pending[source], id)
	if len(m.pending[source]) == 0 {
		delete(m.pending, source)
	}
	p.mu.Unlock()
}

// Storage disappearance, no target, and unrelated resume errors are losses,
// but do not imply that the checkpoint envelope caused the loss.
func checkpointEnvelopeLoss(err error) bool {
	return errors.Is(err, mediaworker.ErrSequenceBudgetExhausted) || errors.Is(err, mediaworker.ErrSRTCPIndexExhausted) || errors.Is(err, mediaworker.ErrDTLSSequenceExhausted) || errors.Is(err, sessionstore.ErrUnsafeCheckpointClock)
}

func agentReplayWindows(windows []relay.ReplayWindow, anchors ...map[uint32]uint64) []agent.DuplicateWindow {
	if len(windows) == 0 {
		return nil
	}
	out := make([]agent.DuplicateWindow, len(windows))
	for i, window := range windows {
		first := window.First
		if len(anchors) > 0 {
			if high, known := anchors[0][window.SSRC]; known {
				first = alignReplayIndex(high, uint16(first)) //nolint:gosec // RTP sequence low bits
			}
		}
		out[i] = agent.DuplicateWindow{SSRC: window.SSRC, First: first, Last: first + window.Last - window.First}
	}
	return out
}

// Only ranges selected before resume can repeat the fenced source's work.
// Later gated live traffic refines the replay result but adds no duplicate risk.
func deliveredReplayWindows(planned, delivered []relay.ReplayWindow, anchors ...map[uint32]uint64) []agent.DuplicateWindow {
	var out []relay.ReplayWindow
	for _, window := range planned {
		for _, sent := range delivered {
			if window.SSRC != sent.SSRC {
				continue
			}
			first, last := max(window.First, sent.First), min(window.Last, sent.Last)
			if first <= last {
				out = append(out, relay.ReplayWindow{SSRC: window.SSRC, First: first, Last: last})
			}
		}
	}
	return agentReplayWindows(out, anchors...)
}

// RFC 3711 half-space inference uses the authenticated transport checkpoint
// to translate a cold relay's locally counted ROC. The bounded source-rate
// envelope and ring window keep the selected range within this half-space.
func alignReplayIndex(high uint64, seq uint16) uint64 {
	roc, previous := high>>16, uint16(high) //nolint:gosec // RTP sequence low bits
	if previous < 1<<15 && seq > previous && uint32(seq)-uint32(previous) > 1<<15 {
		if roc > 0 {
			roc--
		}
	} else if previous >= 1<<15 && previous > seq && uint32(previous)-uint32(seq) > 1<<15 {
		roc++
	}
	return roc<<16 | uint64(seq)
}

// rebuild reconciles a non-end intent without replaying confirmed worker effects.
// Candidate settlement precedes ownership observation; inventory grants no authority.
func (m *moveRecovery) rebuild(ctx context.Context, intent sessionstore.ControlIntent, observations map[string]mediaworker.InstalledSession) error {
	p := m.plane
	if intent.Candidate != nil {
		resolver, ok := p.store.(sessionstore.TransitionResolver)
		if !ok {
			return errors.New("controlplane: transition settlement unavailable")
		}
		if _, _, err := resolver.Settle(ctx, *intent.Candidate); err != nil {
			return err
		}
	}
	current, err := p.store.Get(ctx, intent.ID)
	if errors.Is(err, sessionstore.ErrEnded) || errors.Is(err, sessionstore.ErrNotFound) {
		if err := p.restart.store.DeleteControlIntent(ctx, intent.ID, intent.Token); err != nil {
			return err
		}
		p.restartOutcome(intent.Kind, intent.ID, "ended", "interrupted")
		return nil
	}
	if err != nil {
		return err
	}
	c := p.rebuildCall(current)
	if err := func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.intent = &intent

		installed, ok := observations[current.SessionID+"/"+fmt.Sprint(current.Epoch)+"/"+current.Worker.String()]
		if ok && installed.Owner == current.Worker {
			// Route repair/release is idempotent coordination. Never re-export or
			// re-resume a tenure that the store and live worker both confirm.
			if err := p.relay.MoveSession(intent.ID, netip.AddrPort{}, current.Worker); err != nil {
				return err
			}
			if _, err := p.relay.ReleaseSession(intent.ID, current.Worker); err != nil && !errors.Is(err, privateapi.ErrUncertain) {
				// No hold is normal when the operation stopped before its barrier.
				// The relay backstop also expires holds while control is absent.
				if !errors.Is(err, relay.ErrHoldExpired) {
					return err
				}
			}
			if err := p.restart.store.DeleteControlIntent(ctx, intent.ID, intent.Token); err != nil {
				return err
			}
			p.mu.Lock()
			for source := range m.pending {
				delete(m.pending[source], c.id)
				if len(m.pending[source]) == 0 {
					delete(m.pending, source)
				}
			}
			p.mu.Unlock()
			c.intent = nil
			p.restartOutcome(intent.Kind, intent.ID, "live", "interrupted")
			return nil
		}
		// Missing installation is not a failed resume proof. Transfer away from the
		// current tenure before any new resume, using the existing tuple read and
		// bounded crash-margin recovery. A delayed old adoption cannot own that epoch.
		p.mu.Lock()
		source := p.owner(current.Worker)
		p.mu.Unlock()
		if source == nil {
			return fmt.Errorf("controlplane: unknown persisted owner %s", current.Worker)
		}
		p.mu.Lock()
		var linkDeadline time.Time
		for _, worker := range p.workers {
			if prior := m.pending[worker][c.id]; prior != nil && prior.rebuilding {
				linkDeadline = prior.targetDeadline
			}
			delete(m.pending[worker], c.id)
			if len(m.pending[worker]) == 0 {
				delete(m.pending, worker)
			}
		}
		if m.pending[source] == nil {
			m.pending[source] = map[string]*moveTransfer{}
		}
		if linkDeadline.IsZero() {
			linkDeadline = m.observationTime().Add(takeoverBudget)
		}
		m.pending[source][c.id] = &moveTransfer{call: c, lease: current, routed: current.Worker, routedLease: current, excluded: map[netip.AddrPort]bool{current.Worker: true}, attemptLimit: maxResumeAttempts, held: true, outageStarted: intent.Started, rebuilding: true, targetDeadline: linkDeadline}
		p.mu.Unlock()
		bounded, stop := context.WithTimeout(ctx, takeoverBudget)
		result := m.continueLocked(bounded, source, c, current, time.Now())
		stop()
		if result == nil {
			return ErrNotReady
		}
		outcome, code := "live", "interrupted"
		if result.Lost {
			outcome, code = "lost", result.LossCause
		}
		p.restartOutcome(intent.Kind, intent.ID, outcome, code)
		return nil
	}(); err != nil {
		return err
	}
	return nil
}

type resumeAttempt struct {
	margin           uint16
	rtcpMargin       uint32
	state            []byte
	target           *registration
	deferredRecovery bool // The possibly adopted tenure skipped its own resume PLI.
	relayReplay      bool
	relayInputReplay bool
}
