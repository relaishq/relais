package mediaworker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// ErrBridgeRevoked is a definite refusal: this scope is stopped for replacement.
var ErrBridgeRevoked = fmt.Errorf("%w: bridge generation revoked", ErrMoveBudget)

type bridgeRestartRequest struct {
	ctx       context.Context
	old, next BridgeOutputScope
	runner    string
	done      chan error
}

// CheckpointBridgeStop publishes the stop boundary before a revoke is sent.
// An uncertain write cannot authorize replacement; the next attempt settles it.
func (w *Worker) CheckpointBridgeStop(ctx context.Context, scope BridgeOutputScope) error {
	s, err := w.bridgeSession(scope.SessionID)
	if err != nil {
		return err
	}
	if err = s.checkpoint.persistSnapshotBudget(ctx, true, false); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.agent.v2.bridge
	if b.state == nil || BridgeOutputScope(b.state.Scope) != scope || b.durable == nil || b.durable.Scope != b.state.Scope || !b.durable.Stopped {
		return ErrBridgeOutputUncertain
	}
	return nil
}

// RestartBridge serializes settlement, conversation choice and replacement on
// the actor. The caller must first finish revocation (or its full expiry wait).
func (w *Worker) RestartBridge(ctx context.Context, old, next BridgeOutputScope, runner string) error {
	s, err := w.bridgeSession(old.SessionID)
	if err != nil {
		return err
	}
	if !next.valid(old.SessionID) || next.Generation <= old.Generation || next.LineageID == old.LineageID || next.Epoch < old.Epoch || !bridgeToken(runner) {
		return ErrBridgeOutputScope
	}
	r := bridgeRestartRequest{ctx: ctx, old: old, next: next, runner: runner, done: make(chan error, 1)}
	select {
	case s.agent.v2.bridge.restarts <- r:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *session) restartBridge(r bridgeRestartRequest) error {
	c := &s.checkpoint
	c.snapshotMu.Lock()
	var notifications []func()
	defer func() {
		c.snapshotMu.Unlock()
		for _, notify := range notifications {
			notify()
		}
	}()
	for range 3 {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(r.ctx, ownershipTimeout)
		notify, err := c.settleComponentWrite(ctx)
		cancel()
		if notify != nil {
			notifications = append(notifications, notify)
		}
		if err != nil {
			return err
		}
		if err = s.prepareBridgeRestart(r, &notifications); err != nil {
			return err
		}
		err = c.persistSnapshotBudgetMode(r.ctx, true, false, &notifications)
		if !errors.Is(err, sessionstore.ErrStateSuperseded) {
			return err
		}
	}
	return sessionstore.ErrStateSuperseded
}

// The actor chooses text from one authoritative tuple on every CAS attempt.
// It never replaces live transport/media progress with that tuple's counters.
func (s *session) prepareBridgeRestart(r bridgeRestartRequest, notifications *[]func()) error {
	c := &s.checkpoint
	s.mu.Lock()
	b := s.agent.v2.bridge
	if s.fenced.Load() || s.agent.paused || s.ctx.Err() != nil {
		s.mu.Unlock()
		return ErrBridgeOutputMoving
	}
	if b.state == nil || r.next.Epoch != s.lease.Epoch {
		s.mu.Unlock()
		return ErrBridgeOutputScope
	}
	current := BridgeOutputScope(b.state.Scope)
	if current == r.next && b.restart == nil && b.durable != nil && b.durable.Scope == b.state.Scope {
		s.mu.Unlock()
		return nil
	}
	continuing := current == r.next && b.restart != nil && b.restart.old == r.old && b.restart.next == r.next && b.restart.runner == r.runner
	if !continuing && (current != r.old || !b.state.Stopped) {
		s.mu.Unlock()
		return ErrBridgeOutputScope
	}
	lease := s.lease
	s.mu.Unlock()
	store := s.worker.cfg.Relay.Owners.(sessionstore.ComponentStore)
	ctx, cancel := context.WithTimeout(r.ctx, ownershipTimeout)
	tuple, err := store.ReadComponents(ctx, s.id)
	cancel()
	if err != nil {
		return err
	}
	if tuple.Lease.Worker != lease.Worker || tuple.Lease.Epoch != lease.Epoch {
		return sessionstore.ErrLeaseLost
	}
	data, err := ComponentSnapshot(tuple)
	if err != nil {
		return err
	}
	snap, err := decodeSnapshot(data)
	if err != nil {
		return err
	}
	if snap.State.Agent.V2 == nil || snap.State.Agent.V2.Bridge == nil {
		return ErrBridgeOutputScope
	}
	baseline := BridgeOutputScope(snap.State.Agent.V2.Bridge.Scope)
	if baseline != r.old && baseline != r.next {
		return sessionstore.ErrStateSuperseded
	}
	c.componentMetadata = &tuple.Metadata
	durable := cloneV2State(v2State(snap.State.Agent))
	app, ok := s.agent.v2.instance.(agenthost.BridgeRestarter)
	if !ok {
		return agent.ErrIncompatibleAgent
	}
	before := cloneAgentState(s.agent.latest)
	var saved agent.State
	err = s.agentCall(func(ctx context.Context) error {
		var err error
		saved, err = app.Restart(ctx, durable, agenthost.BridgeScope(r.next), r.runner)
		if err != nil {
			return err
		}
		return s.validateV2StateMetadata(saved)
	}, true)
	if err != nil {
		if s.ctx.Err() != nil {
			return err
		}
		_ = s.agentCall(func(ctx context.Context) error {
			return s.agent.v2.instance.Restore(ctx, cloneV2State(v2State(before)))
		}, true)
		return err
	}
	candidate := cloneAgentState(before)
	candidate.State = legacy.State{Version: saved.Version, Bytes: saved.Bytes}
	candidate.History = saved.History
	candidate.V2.HasMoveOnly, candidate.V2.MoveOnlyLength = saved.HasMoveOnly, saved.MoveOnlyLength
	candidate.V2.Tasks, candidate.V2.Capacity, candidate.V2.Timers = nil, nil, nil
	candidate.V2.TimerGeneration++
	candidate.V2.Timers = []hostTimer{{ID: "bridge-connect", Generation: candidate.V2.TimerGeneration, At: time.Now()}}
	candidate.Revision++
	if _, err = c.admit(checkpointAdmission{candidate: candidate, prior: before}); err != nil {
		c.rollback(before)
		if restoreErr := s.agentCall(func(ctx context.Context) error {
			return s.agent.v2.instance.Restore(ctx, cloneV2State(v2State(before)))
		}, true); restoreErr != nil {
			return restoreErr
		}
		return err
	}
	s.mu.Lock()
	// No actor callback can interleave. Recheck ownership after the local callback.
	if r.ctx.Err() != nil || s.fenced.Load() || s.agent.paused || r.next.Epoch != s.lease.Epoch {
		s.mu.Unlock()
		c.rollback(before)
		if restoreErr := s.agentCall(func(ctx context.Context) error {
			return s.agent.v2.instance.Restore(ctx, cloneV2State(v2State(before)))
		}, true); restoreErr != nil {
			return restoreErr
		}
		return ErrBridgeOutputMoving
	}
	s.stopTaskAttempts()
	b.attempts = make(map[agent.TaskID]bridgeAttempt)
	retained := b.recordMemory.Load() + b.settledMemory()
	s.agent.v2.tasks = nil
	s.agent.v2.receiptRetries = nil
	for _, record := range b.records {
		b.replaceRecordEncoding(&record, nil)
	}
	b.records = make(map[uint64]bridgePacketRecord)
	b.settled = nil
	b.settledCost.Store(0)
	b.settledLive.Store(0)
	_ = s.reserveBridgeMemory(-retained)
	b.audioBytes, b.audioDuration, b.replayHorizon = 0, 0, 0
	b.durable, b.checkpointSeq = nil, 0
	b.permit = bridgePermitCache{stopped: time.Now()}
	b.restart = &r
	b.provenUnadmitted = true
	b.state = &bridgeOutputState{Version: bridgeOutputVersion, Scope: bridgeOutputIdentity(r.next), Revision: b.state.Revision + 1}
	p := &s.agent.v2.playback
	p.bridgeQueue, p.notify = nil, nil
	p.length, p.duration = 0, 0
	candidate.V2.Playback = s.playbackSnapshotLocked()
	s.projectBridgeLocked(candidate.V2)
	s.agent.latest = candidate
	s.state.Agent = cloneAgentState(candidate)
	s.mu.Unlock()
	*notifications = append(*notifications, s.agent.v2.instance.(agenthost.BridgeHooks).Accepted)
	s.wakePacer()
	return nil
}

// The caller holds session.mu. Activation can finish under the same generation;
// a real stop requires a replacement generation instead.
func (s *session) bridgeExportRefusalLocked() error {
	b := s.agent.v2.bridge
	if b == nil {
		return nil
	}
	if b.state != nil && b.state.Stopped {
		return ErrBridgeRevoked
	}
	if b.restart != nil || !b.permit.stopped.IsZero() {
		return ErrExportNotFlushed
	}
	return nil
}
