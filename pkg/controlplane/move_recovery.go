package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/agent"
	agentv2 "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// moveRecovery owns the planned-move sequence and retained recovery for each call.
// The plane supplies existing placement, authority, observation and cleanup logic.
// Transfer facts never leave this module; callers use scheduling snapshots only.
type moveRecovery struct {
	plane   *Plane
	pending map[*registration]map[string]*moveTransfer // membership under Plane.mu
	// Tests control observation time/wakeup without scheduler-sized sleeps.
	targetNow      func() time.Time
	targetWait     func(context.Context, <-chan struct{}, time.Duration) error
	beforeTransfer func() // deterministic observation boundary before the atomic decision
}

// Snapshot provenance controls counter margins; route knowledge controls repair.
type recoverySnapshot uint8

const (
	recoveryCheckpoint recoverySnapshot = iota
	recoveryExact
)

type moveRoute uint8

const (
	routeAtSource moveRoute = iota
	routeAttempted
)

type recoveryScope uint8

const (
	recoveryAll recoveryScope = iota
	recoveryPlanned
)

// Scheduling queries require Plane.mu and never perform I/O.
func (m *moveRecovery) pendingLocked(id string) bool {
	for _, entries := range m.pending {
		if entries[id] != nil {
			return true
		}
	}
	return false
}

// hasWorkLocked checks membership without allocating scheduling tokens. The
// all-work check is constant-time; planned work stops at the first match.
func (m *moveRecovery) hasWorkLocked(source *registration, scope recoveryScope) bool {
	entries := m.pending[source]
	if scope == recoveryAll {
		return len(entries) > 0
	}
	for _, pending := range entries {
		if pending.planned || pending.held {
			return true
		}
	}
	return false
}

func (m *moveRecovery) workLocked(source *registration, scope recoveryScope) []sessionstore.Lease {
	entries := m.pending[source]
	leases := make([]sessionstore.Lease, 0, len(entries))
	for _, pending := range entries {
		if scope == recoveryAll || pending.planned || pending.held {
			leases = append(leases, pending.lease)
		}
	}
	return leases
}

// moveAfterExport keeps the existing route call when the capability is absent.
// A retry whose previous route call failed no longer knows the routed source,
// so it cannot drain that source's output: it uses the existing route repair.
func moveAfterExport(r Relay, id string, from, to netip.AddrPort, epoch uint64) error {
	if mover, ok := r.(ExportMoveRelay); ok && from.IsValid() {
		return mover.MoveSessionAfterExport(id, from, to, epoch)
	}
	if tenure, ok := r.(TenureRelay); ok && epoch != 0 {
		return tenure.MoveSessionAtEpoch(id, from, to, epoch)
	}
	return r.MoveSession(id, from, to)
}

// releaseHold releases with the export's consumed input when both it and the
// relay capability are available, and otherwise exactly as before.
func releaseHold(r Relay, id string, to netip.AddrPort, input map[uint32]relay.ConsumedInput, result *mediaworker.HandoverResult) error {
	if exporter, ok := r.(ExportReleaseRelay); ok && input != nil {
		reply, err := exporter.ReleaseAfterExport(id, to, input)
		result.HeldPackets, result.RecoveredPackets, result.ReleaseLockHeld = reply.Packets, reply.Recovered, reply.LockHeld
		return err
	}
	held, err := r.ReleaseSession(id, to)
	result.HeldPackets = held
	return err
}

// moveLocked requires the call lock and an incoming target reservation. It owns
// the rollback slot, relay hold and all transfer facts through final accounting.
// Definite export refusals leave the source live; uncertainty retains recovery.
// Store and transport work use the existing bounds without holding registry locks.
func (m *moveRecovery) moveLocked(ctx context.Context, c *call, lease sessionstore.Lease, target *registration) (res MoveResult, err error) {
	p := m.plane
	moved, recoveryMetrics := false, false
	defer func() {
		if recoveryMetrics {
			return // Pending recovery owns the eventual metric outcome.
		}
		if err != nil {
			p.metrics.moveErrors.Add(1)
		} else if moved {
			p.metrics.moves.Add(1)
		}
	}()
	p.mu.Lock()
	source, r := p.owner(lease.Worker), p.relay
	if source != nil {
		source.outgoingMoves++
		defer func() { p.mu.Lock(); source.outgoingMoves--; p.mu.Unlock() }()
	}
	// Keep a slot for synchronous rollback after Transfer releases the source
	// lease. Otherwise a concurrent create could fill it before rollback.
	rollbackReserved := source != nil && p.config.WorkerCallCap > 0
	if rollbackReserved {
		source.rollbackSlots++
		p.loadVersion++
	}
	p.mu.Unlock()
	if rollbackReserved {
		defer func() {
			p.mu.Lock()
			source.rollbackSlots--
			p.loadVersion++
			p.mu.Unlock()
		}()
	}
	if source == nil {
		return res, ErrUnknownCall
	}

	move := &moveTransfer{call: c, source: source, target: target, relay: r, lease: lease, snapshot: recoveryExact, routed: source.addr, routedLease: lease}
	if err = p.beginIntent(ctx, c, "move", lease); err != nil {
		return res, err
	}
	defer p.finishIntent(c)
	res = MoveResult{Kind: "move", ID: c.id, From: source.name, To: target.name, Start: time.Now(), Result: mediaworker.HandoverResult{SessionID: c.id}}
	defer func() {
		res.End = time.Now()
		res.Result.Duration = res.End.Sub(res.Start)
		if err != nil {
			res.Error = err.Error()
		}
	}()
	move.outageStarted = res.Start
	started := time.Now()
	// Missing acknowledgement aborts before export; the relay replays to A.
	if err = diagnosticRelay(r, movediag.Phase(ctx, "barrier")).HoldSession(movediag.Phase(ctx, "barrier"), c.id, source.addr); err != nil {
		if errors.Is(err, privateapi.ErrUncertain) {
			_, _ = r.ReleaseSession(c.id, source.addr)
		}
		return res, err
	}

	res.Result.Drain = time.Since(started)
	// Successful/rolled-back resume releases to its owner. An uncertain final
	// export retains the hold until adoption, bounded by the relay backstop.
	// exported is set only while the releasing owner resumed from the exact,
	// margin-zero export: the hold then also replays input it never consumed.
	move.releaseTo = source.addr
	move.released = false
	move.exported = nil
	defer func() {
		if move.released {
			return
		}
		p.mu.Lock()
		pending := m.pending[source][c.id]
		retained := pending != nil
		lost := p.calls[c.id] != c
		p.mu.Unlock()
		if lost {
			// Terminal cleanup already move.released the hold and forgot the call.
			res.Lost = true
			return
		}
		if retained {
			return
		}
		releaseErr := releaseHold(diagnosticRelay(r, movediag.Phase(ctx, "release")), c.id, move.releaseTo, move.exported, &res.Result)
		if errors.Is(releaseErr, relay.ErrHoldExpired) {
			res.Result.HoldExpired = true
			if err == nil {
				// Resume completed; expiry affected packet holding, not ownership.
				// The relay already counted it in HoldTimeouts. Do not invite a retry.
				return
			}
		}
		if releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("controlplane: release caller hold: %w", releaseErr))
		}
	}()
	started = time.Now()
	state, err := diagnosticWorker(source.worker, movediag.Phase(ctx, "export")).ExportSession(c.id)
	res.Result.Export = time.Since(started)
	if err != nil {
		// A generic/transport error cannot prove export did not flush A.
		// Bump its epoch either way and recover the latest store snapshot.
		if uncertainExport(err) {
			recoveryMetrics = true
			p.mu.Lock()
			if m.pending[source] == nil {
				m.pending[source] = make(map[string]*moveTransfer)
			}
			m.pending[source][c.id] = &moveTransfer{call: c, lease: lease, routed: source.addr, routedLease: lease,
				excluded: map[netip.AddrPort]bool{source.addr: true}, attemptLimit: maxResumeAttempts, held: true, outageStarted: res.Start}
			p.mu.Unlock()
			recovery, cancel := context.WithTimeout(context.Background(), takeoverBudget)
			defer cancel()
			outcome := m.continueLocked(recovery, source, c, lease, res.Start)
			p.mu.Lock()
			pending := m.pending[source][c.id]
			p.mu.Unlock()
			if outcome != nil {
				res = *outcome
				if !res.Lost {
					move.releaseTo = target.addr
					err = nil
				} else {
					err = errors.New(res.Error)
				}
			}
			if pending != nil {
				return res, fmt.Errorf("controlplane: export uncertain; recovery pending: %w", err)
			}
			if outcome == nil {
				// A concurrent transfer ended recovery without a terminal outcome.
				// Let move's defer count the failed coordination attempt.
				recoveryMetrics = false
			}
			move.released = true
			return res, err
		}
		if errors.Is(err, mediaworker.ErrUnknownSession) {
			// A hard-killed source has no memory but can still own a live
			// lease and snapshot. Preserve that record for crash recovery.
			current, getErr := p.store.Get(ctx, c.id)
			if errors.Is(getErr, sessionstore.ErrEnded) {
				p.forget(c)
			} else if errors.Is(getErr, sessionstore.ErrNotFound) {
				c.rememberLease(lease)
				p.expiredLocked(ctx, c)
				res.Lost, res.LossCause = true, "ownership_expired"
			} else if getErr == nil &&
				(current.Worker != lease.Worker || current.Epoch != lease.Epoch) {
				p.forget(c)
			}
		}

		return res, err
	}

	move.plannedState = state
	res.Result.StateBytes = len(state)
	// A malformed copy keeps the plain release; resume would reject it anyway.
	move.exported, _ = mediaworker.SnapshotConsumedInput(state)
	transferred, err := m.transfer(movediag.Phase(ctx, "transfer"), lease, target.addr)
	if err != nil {
		if m.retainUncertainMove(move, err) {
			return res, err
		}
		err = m.rollback(move, &res.Result, err)
		return res, err
	}

	move.lease, move.transferred, move.route = transferred, true, routeAttempted
	if err = moveAfterExport(diagnosticRelay(r, movediag.Phase(ctx, "transfer")), c.id, source.addr, target.addr, 0); err != nil {
		err = m.rollback(move, &res.Result, err)
		return res, err
	}

	started = time.Now()
	opts := mediaworker.ResumeOptions{Kind: agent.PlannedMove, Lease: transferred}
	if movediag.ID(ctx) != "" {
		opts.Context = movediag.Copy(context.Background(), movediag.Phase(ctx, "resume"))
	}
	_, err = target.worker.ResumeSession(state, opts)
	res.Result.Resume = time.Since(started)
	if err != nil {
		// Explicit transport uncertainty can hide a committed adoption, even
		// with an agent cause. Otherwise typed callback failures are definitive.
		agentFailure := errors.Is(err, agentv2.ErrIncompatibleAgent) || errors.Is(err, agent.ErrRestore) || errors.Is(err, agent.ErrVersion) || errors.Is(err, agent.ErrStateTooLarge) || errors.Is(err, agent.ErrSave)
		uncertainResume := errors.Is(err, privateapi.ErrUncertain) || (!agentFailure && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)))
		if uncertainResume {
			move.snapshot = recoveryCheckpoint
			p.metrics.uncertainMoveResumes.Add(1)
			// The source resumes from a store checkpoint with crash margins.
			// That copy may predate the export, so keep the plain release.
			move.exported = nil
		}
		err = m.rollback(move, &res.Result, err)
		return res, err
	}

	move.releaseTo = target.addr
	now := time.Now()
	c.stateMu.Lock()
	c.lastMove = &now
	c.moveCount++
	c.lastMoveKind = "move"
	c.stateMu.Unlock()
	moved = true
	return res, nil
}

func (m *moveRecovery) rollback(move *moveTransfer, result *mediaworker.HandoverResult, cause error) error {
	p := m.plane
	c, source, target, r := move.call, move.source, move.target, move.relay
	state, lease, moveStarted := move.plannedState, move.lease, move.outageStarted
	transferred, rerouted := move.transferred, move.route == routeAttempted
	uncertainResume := move.snapshot == recoveryCheckpoint
	// Recovery uses a fresh context after the request has flushed its source.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res := MoveResult{Kind: "move", ID: c.id, From: source.name, To: target.name, Start: moveStarted}
	var err error
	checkpointOutside := false
	if transferred {
		from := lease
		lease, err = m.transfer(ctx, lease, source.addr)
		routed := source.addr
		if rerouted {
			routed = netip.AddrPort{}
		}
		move.lease, move.routed = from, routed
		if m.retainUncertainMove(move, err) {
			return err
		}
	}
	var routeErr error
	if err == nil && rerouted {
		routeErr = r.MoveSession(c.id, target.addr, source.addr)
	}
	// A failed reverse notification must not prevent A from running again.
	// B never resumed, and the next authenticated consent check follows A's
	// restored store ownership even if the relay itself needs to recover.
	if err == nil {
		resumeCtx := ctx
		if uncertainResume {
			var resumeCancel context.CancelFunc
			resumeCtx, resumeCancel = context.WithTimeout(ctx, takeoverBudget)
			defer resumeCancel()
		}
		opts := mediaworker.ResumeOptions{Kind: agent.PlannedMove, Lease: lease, Context: resumeCtx}
		if uncertainResume {
			// B may already have emitted media and persisted adjusted counters.
			// Its fenced store state, rather than the final export from A, is now
			// the only valid checkpoint for a rollback that needs crash margins.
			var tupleMetadata *sessionstore.Checkpoint
			state, tupleMetadata, err = mediaworker.ReadRecoverySnapshot(ctx, p.store, c.id)
			if err == nil {
				var decision checkpointDecision
				decision, err = m.checkpointDecision(resumeCtx, c.id, state, checkpointOutageBudget(resumeCtx, moveStarted), tupleMetadata)
				res.CheckpointAge, res.SnapshotAge, res.CheckpointStoredAt = decision.age, decision.snapshotAge, decision.storedAt
				res.Checkpoint, res.SequenceMargin, res.SRTCPIndexMargin = decision.info, decision.margin, decision.rtcpMargin
				checkpointOutside = decision.outside
				opts.Kind = agent.Takeover
				opts.SequenceMargin, opts.SRTCPIndexMargin = decision.margin, decision.rtcpMargin
				opts.CallerSequenceReserve = decision.reserve
				opts.CheckpointAge, opts.SnapshotAge, opts.CheckpointStoredAt = decision.age, decision.snapshotAge, decision.storedAt
			}
		}
		if err == nil {
			_, err = source.worker.ResumeSession(state, opts)
		}
	}
	if errors.Is(err, mediaworker.ErrMoveBudget) && !errors.Is(err, privateapi.ErrUncertain) {
		// Both targets refused before adoption. Keep the exact export and hold;
		// the bounded recovery loop can retry either live worker or an alternate.
		p.mu.Lock()
		if m.pending[source] == nil {
			m.pending[source] = make(map[string]*moveTransfer)
		}
		excluded := make(map[netip.AddrPort]bool)
		if source.observation.knowledge().dead {
			excluded[source.addr] = true
		}
		m.pending[source][c.id] = &moveTransfer{call: c, lease: lease, routed: source.addr, routedLease: lease,
			planned: true, plannedState: state, snapshot: move.snapshot, held: true, excluded: excluded,
			attemptLimit: maxResumeAttempts, outageStarted: moveStarted, budgetRollback: true, budgetStarted: time.Now(),
			budgetExcluded: map[netip.AddrPort]bool{source.addr: true, target.addr: true}}
		p.mu.Unlock()
		return fmt.Errorf("controlplane: rollback awaiting move budget: %w", err)
	}
	if checkpointOutside && (err == nil || checkpointEnvelopeLoss(err)) {
		res.CheckpointPolicy = "scaled"
		if err != nil {
			res.CheckpointPolicy = "definitive-loss"
		}
		metrics.CheckpointEnvelopeEvents.WithLabelValues(res.CheckpointPolicy).Inc()
	}
	if err != nil {
		loss := fmt.Errorf("controlplane: call lost: move failed (%v), rollback failed: %w", cause, err)
		m.complete(source, c, lease, &res, true, loss)
		return loss
	}

	result.RolledBack = true
	if routeErr != nil {
		return fmt.Errorf("controlplane: rolled back on source; relay restore failed (%v): %w", routeErr, cause)
	}

	return fmt.Errorf("controlplane: move failed, rolled back: %w", cause)
}

// transfer adopts an exact candidate after uncertainty. Only settlement can
// prove a negative; a failed Get must never trigger rollback of a committed move.
func (m *moveRecovery) transfer(ctx context.Context, from sessionstore.Lease, to netip.AddrPort) (sessionstore.Lease, error) {
	p := m.plane
	if p.restart != nil {
		if !p.controlAuthority() {
			return sessionstore.Lease{}, ErrNotReady
		}
		c, e := p.lookup(from.SessionID)
		if e != nil {
			return sessionstore.Lease{}, e
		}
		if c.intent == nil {
			return sessionstore.Lease{}, errors.New("controlplane: transfer missing durable intent")
		}
		ctx = sessionstore.WithTransitionIntent(ctx, c.id, c.intent.Token)
	}
	p.mu.Lock()
	source := p.owner(from.Worker)
	if source != nil {
		source.transfers++
		if source.observation.dead {
			source.observation.deathTransferIssued = true
		}
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if source != nil {
			source.transfers--
		}
		p.mu.Unlock()
	}()
	lease, err := p.store.Transfer(ctx, from, to, p.ttl)
	defer func() {
		if err == nil {
			if c, e := p.lookup(from.SessionID); e == nil {
				c.rememberLease(lease)
			}
		}
	}()
	var transient *sessionstore.TransientError
	if !errors.As(err, &transient) || transient.Candidate == nil {
		return lease, err
	}
	lease, err = m.resolveCandidate(ctx, *transient.Candidate, err)
	return lease, err
}

func (m *moveRecovery) resolveCandidate(ctx context.Context, candidate sessionstore.Lease, cause error) (sessionstore.Lease, error) {
	p := m.plane
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	current, err := p.store.Get(ctx, candidate.SessionID)
	if err == nil && current.Worker == candidate.Worker && current.Epoch == candidate.Epoch {
		return current, nil
	}
	if resolver, ok := p.store.(sessionstore.TransitionResolver); ok {
		settled, committed, settleErr := resolver.Settle(ctx, candidate)
		if settleErr == nil {
			if committed {
				return settled, nil
			}
			return sessionstore.Lease{}, &sessionstore.TransientError{Op: "settled transfer", Err: cause}
		}
	}
	return sessionstore.Lease{}, &sessionstore.TransientError{Op: "uncertain transfer", Err: cause, Candidate: &candidate}
}

// Retain the final export and candidate if the store is still unavailable.
// Run retries these calls without declaring a healthy source worker dead or
// exporting it again. The relay hold waits for adoption or its bounded
// backstop; adopted retries explicitly release to the confirmed owner.
func (m *moveRecovery) retainUncertainMove(move *moveTransfer, cause error) bool {
	p := m.plane
	var transient *sessionstore.TransientError
	if !errors.As(cause, &transient) || transient.Candidate == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	source, from := move.source, move.lease
	if m.pending[source] == nil {
		m.pending[source] = make(map[string]*moveTransfer)
	}
	excluded := map[netip.AddrPort]bool{source.addr: true}
	if transient.Candidate.Worker == source.addr {
		excluded = map[netip.AddrPort]bool{from.Worker: true}
	}
	move.candidate, move.routedLease, move.excluded = transient.Candidate, from, excluded
	move.attemptLimit, move.planned = maxResumeAttempts, true
	m.pending[source][move.call.id] = move
	return true
}

// Only typed errors proving no flush and no discarded input are definitive.
// A successful scratch admission may already have consumed or dropped input;
// subsequent callback, encoding and transport errors require crash recovery.
func uncertainExport(err error) bool {
	if errors.Is(err, privateapi.ErrUncertain) {
		return true
	}
	for _, known := range []error{privateapi.ErrNotSent, mediaworker.ErrStaleTenure, mediaworker.ErrUnknownSession, mediaworker.ErrClosed, mediaworker.ErrNotEstablished, mediaworker.ErrExportNotFlushed, mediaworker.ErrExportIncompatible,
		sessionstore.ErrNotFound, sessionstore.ErrLeaseLost, sessionstore.ErrTransient, agentv2.ErrCapacity} {
		if errors.Is(err, known) {
			return false
		}
	}
	return true
}
