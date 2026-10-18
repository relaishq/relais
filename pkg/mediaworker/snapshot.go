package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// SnapshotSession copies a live session without fencing or dropping it. The
// result has the same resumable format as ExportSession. Only the copy takes
// the packet lock; binary/JSON encoding happens after releasing it.
func (w *Worker) SnapshotSession(id string) ([]byte, error) {
	s := w.session(id)
	if s == nil {
		return nil, ErrUnknownSession
	}
	return s.snapshotBytes()
}

func (s *session) snapshotBytes() ([]byte, error) {
	return s.snapshotBytesAt(time.Time{})
}

func (s *session) snapshotBytesAt(captured time.Time) ([]byte, error) {
	data, _, err := s.captureSnapshotAt(captured)
	return data, err
}

// captureSnapshotAt returns the exact owned pair encoded in this snapshot.
func (s *session) captureSnapshotAt(captured time.Time) ([]byte, agentState, error) {
	return s.captureSnapshotCandidate(captured, nil)
}

func (s *session) captureSnapshotCandidate(captured time.Time, candidate *agentState) ([]byte, agentState, error) {
	data, pair, _, err := s.captureSnapshotCandidateMode(captured, candidate, false)
	return data, pair, err
}

func (s *session) captureSnapshotCandidateMode(captured time.Time, candidate *agentState, writing bool) ([]byte, agentState, dtlsSequenceCheckpoint, error) {
	s.mu.Lock()
	if s.fenced.Load() || s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, agentState{}, dtlsSequenceCheckpoint{}, errHandedOver
	}
	if s.dtlsConn == nil || s.srtpIn == nil {
		s.mu.Unlock()
		return nil, agentState{}, dtlsSequenceCheckpoint{}, ErrNotEstablished
	}
	dtlsState, ok := s.dtlsConn.ConnectionState()
	var binary []byte
	var next dtlsSequenceCheckpoint
	// Arm at the capture boundary, before releasing the packet lock. Waiting
	// until encoding or PutState would leave an unbounded tail before the
	// first write can commit. Read-only snapshots and admission do not arm it.
	if ok && writing && s.dtlsProvisional == nil && s.dtlsAcknowledged == nil {
		var err error
		binary, err = dtlsState.MarshalBinary()
		if err != nil {
			s.mu.Unlock()
			return nil, agentState{}, dtlsSequenceCheckpoint{}, fmt.Errorf("mediaworker: snapshot DTLS: %w", err)
		}
		serialized, err := decodeDTLSSerializedState(binary)
		if err != nil {
			s.mu.Unlock()
			return nil, agentState{}, dtlsSequenceCheckpoint{}, err
		}
		next = dtlsSequenceCheckpoint{epoch: serialized.LocalEpoch, next: serialized.SequenceNumber}
		s.dtlsProvisional = &next
	}
	state := s.state
	if !captured.IsZero() {
		state.Checkpoint.CapturedAt = captured
		state.Checkpoint.Successes++ // only durable if this put succeeds
		if !s.lastCheckpoint.IsZero() {
			state.Checkpoint.AgeAtCapture = time.Since(s.lastCheckpoint)
		}
	}
	s.checkpointRates(&state.Checkpoint)
	state.SRTP.Inbound = maps.Clone(state.SRTP.Inbound)
	state.SRTP.Received = maps.Clone(state.SRTP.Received)
	state.Agent = cloneAgentState(state.Agent)
	if candidate != nil {
		state.Agent = cloneAgentState(*candidate)
	}
	// Certificate/key byte slices are immutable throughout a session.
	s.mu.Unlock()
	if !ok {
		return nil, agentState{}, dtlsSequenceCheckpoint{}, errors.New("mediaworker: DTLS connection state unavailable")
	}
	if binary == nil {
		var err error
		binary, err = dtlsState.MarshalBinary()
		if err != nil {
			return nil, agentState{}, dtlsSequenceCheckpoint{}, fmt.Errorf("mediaworker: snapshot DTLS: %w", err)
		}
		if writing {
			serialized, err := decodeDTLSSerializedState(binary)
			if err != nil {
				return nil, agentState{}, dtlsSequenceCheckpoint{}, err
			}
			next = dtlsSequenceCheckpoint{epoch: serialized.LocalEpoch, next: serialized.SequenceNumber}
		}
	}
	now := captured
	if now.IsZero() {
		now = s.taskNow()
	}
	if state.Agent.V2 != nil {
		state.Agent.V2.Quota = s.captureSnapshotQuota(now)
	}
	value := snapshot{Version: state.Version, State: state, DTLSConnection: binary}
	data, err := json.Marshal(value)
	if err == nil && state.Agent.V2 != nil && !captured.IsZero() {
		// The fixed-width balance has the same encoded size before/after debit.
		// Commit it only if the enclosing fenced write succeeds.
		quota := state.Agent.V2.Quota
		available, parseErr := quota.balance()
		if parseErr != nil {
			return nil, agentState{}, dtlsSequenceCheckpoint{}, parseErr
		}
		cost := uint64(len(data) + snapshotEnvelopeBytes)
		if available > cost {
			quota.Balance = quotaBalance(available - cost)
		} else {
			quota.Balance = quotaBalance(0)
		}
		data, err = json.Marshal(value)
	}
	if err == nil && state.Agent.V2 != nil {
		err = s.checkSnapshotSize(data)
	}
	// Return the counter from this owned connection-state capture, never a live
	// counter read after PutState or a second parse of the full snapshot JSON.
	return data, state.Agent, next, err
}

// Snapshot ordering: copy+put is serialized by snapshotMu, preventing an old
// copy from overwriting a newer one. Copy under mu; encode/store outside it.
// Persist after handshake/resume, every configurable 100 ms, and on ROC or
// first-packet changes. Wakeups coalesce without storage on the packet path.
func (s *session) persistSnapshot() error {
	return s.persistSnapshotContext(s.ctx)
}

// errCheckpointSkipped is not a durable acknowledgment. Resume and replay
// reservations use force=true and cannot skip their required checkpoint.
var errCheckpointSkipped = errors.New("mediaworker: checkpoint rate limited")

func (s *session) persistSnapshotContext(parent context.Context) error {
	return s.persistSnapshotMode(parent, false)
}

func (s *session) persistSnapshotMode(parent context.Context, force bool) error {
	return s.persistSnapshotBudget(parent, force, false)
}

func (s *session) persistSnapshotBudget(parent context.Context, force, intent bool) error {
	if s.worker.cfg.Relay == nil {
		return nil
	}
	// Serialize copy+put so an older copy cannot overwrite a newer one. The
	// packet lock is never held during encoding or the fenced store operation.
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.mu.Lock()
	v2 := s.state.Agent.V2 != nil
	if v2 && (s.fenced.Load() || s.ctx.Err() != nil) {
		s.mu.Unlock()
		return errHandedOver
	}
	changed := !agentStateEqual(s.state.Agent, s.durableAgent)
	tooSoon := s.snapshotStored.Load() && time.Since(s.lastCheckpoint) < s.worker.cfg.Agent.SaveInterval
	if !force && changed && tooSoon {
		s.mu.Unlock()
		return errCheckpointSkipped
	}
	if v2 {
		s.snapshotBudgetMu.Lock()
		s.snapshotWriteInFlight = true
		s.snapshotBudgetMu.Unlock()
	}
	s.mu.Unlock()
	if v2 {
		defer func() { s.snapshotBudgetMu.Lock(); s.snapshotWriteInFlight = false; s.snapshotBudgetMu.Unlock() }()
	}
	ctx, cancel := context.WithTimeout(parent, ownershipTimeout)
	defer cancel()
	s.mu.Lock()
	s.state.Checkpoint.Attempts++
	if v2 && intent {
		s.worker.metrics.forcedAttempts.Add(1)
	}
	s.mu.Unlock()
	captured, err := s.worker.cfg.Relay.Owners.Clock(ctx, s.id)
	if err == nil {
		s.mu.Lock()
		s.storeClock = captured
		s.storeClockAt = time.Now()
		s.mu.Unlock()
	}
	var state []byte
	var pair agentState
	var dtlsNext dtlsSequenceCheckpoint
	if err == nil {
		state, pair, dtlsNext, err = s.captureSnapshotCandidateMode(captured, nil, true)
	}
	if err != nil {
		s.mu.Lock()
		s.state.Checkpoint.Failures++
		if v2 && intent {
			s.worker.metrics.forcedFailures.Add(1)
		}
		s.mu.Unlock()
		metrics.CheckpointWrites.WithLabelValues("failure").Inc()
		return err
	}
	// Clock may wait while a callback publishes new state. Apply the cap to
	// the actual captured pair too, rather than only the pre-Clock observation.
	s.mu.Lock()
	capturedChanged := !agentStateEqual(pair, s.durableAgent)
	capturedTooSoon := s.snapshotStored.Load() && time.Since(s.lastCheckpoint) < s.worker.cfg.Agent.SaveInterval
	lease := s.lease
	s.mu.Unlock()
	if !force && capturedChanged && capturedTooSoon {
		s.mu.Lock()
		s.state.Checkpoint.Attempts-- // discarded capture is not a write attempt
		s.mu.Unlock()
		return errCheckpointSkipped
	}
	wireBytes := len(state) + snapshotEnvelopeBytes
	if pair.V2 != nil {
		if err := s.checkSnapshotSize(state); err != nil {
			return err
		}
		s.snapshotBudgetMu.Lock()
		available := s.snapshotBytesBudget.available(s.worker.cfg.SnapshotLimits, captured)
		s.snapshotBudgetMu.Unlock()
		required := wireBytes
		if intent {
			required += s.worker.cfg.SnapshotLimits.MaxBytes
		}
		if available < float64(required) {
			if intent {
				return errCheckpointSkipped
			}
			return fmt.Errorf("%w: required v2 snapshot byte budget", agent.ErrCapacity)
		}
	}
	err = s.worker.cfg.Relay.Owners.PutState(ctx, lease, state)
	if err == nil {
		if pair.V2 != nil {
			s.snapshotBudgetMu.Lock()
			s.snapshotBytesBudget.debit(s.worker.cfg.SnapshotLimits, wireBytes, captured)
			s.worker.metrics.snapshotDebitBytes.Add(uint64(wireBytes))
			if intent {
				s.worker.metrics.forcedSuccesses.Add(1)
			}
			s.snapshotWriteInFlight = false
			s.snapshotBudgetMu.Unlock()
		}
		s.snapshotStored.Store(true)
		s.mu.Lock()
		bound := s.dtlsAcknowledged
		if bound == nil {
			bound = s.dtlsProvisional
		}
		// Keep the latch on identical acknowledgments: retransmission spam must
		// not request another write until the acknowledged bound actually moves.
		if bound == nil || *bound != dtlsNext {
			s.dtlsCheckpointRequested = false
		}
		s.dtlsAcknowledged = &dtlsNext
		s.dtlsProvisional = nil
		s.state.Checkpoint.Successes++
		s.state.Checkpoint.CapturedAt = captured
		s.checkpointRates(&s.state.Checkpoint)
		s.durableAgent = pair
		if pair.V2 != nil {
			s.controlAck = max(s.controlAck, pair.V2.ControlHigh)
		}
		s.lastCheckpoint = time.Now()
		s.mu.Unlock()
		metrics.CheckpointWrites.WithLabelValues("success").Inc()
	} else {
		s.mu.Lock()
		s.state.Checkpoint.Failures++
		if v2 && intent {
			s.worker.metrics.forcedFailures.Add(1)
		}
		s.mu.Unlock()
		metrics.CheckpointWrites.WithLabelValues("failure").Inc()
	}

	if errors.Is(err, sessionstore.ErrLeaseLost) {
		s.mu.Lock()
		s.fenced.Store(true)
		s.mu.Unlock()
		s.close()
	}
	return err
}

func (s *session) wantSnapshot() {
	select {
	case s.snapshotWanted <- struct{}{}:
	default:
	}
}

func (s *session) snapshotLoop() {
	if s.worker.cfg.Relay == nil {
		return
	}
	ticker := time.NewTicker(s.worker.cfg.SnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		case <-s.snapshotWanted:
		}
		if s.worker.paused.Load() {
			continue
		}
		workerprobe.BeforeSnapshot(s.worker.localAddr, s.ctx, s.id)
		if s.ctx.Err() != nil {
			return
		}
		if err := s.persistSnapshot(); err != nil && !errors.Is(err, ErrNotEstablished) && !errors.Is(err, errCheckpointSkipped) && s.ctx.Err() == nil {
			s.log.Warnf("session %s: snapshot: %v", s.id, err)
			if errors.Is(err, agent.ErrCapacity) {
				s.agentFailed(err)
				return
			}
		}
	}
}
