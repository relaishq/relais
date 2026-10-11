package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/sessionstore"
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
	s.mu.Lock()
	if s.fenced.Load() || s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, agentState{}, errHandedOver
	}
	if s.dtlsConn == nil || s.srtpIn == nil {
		s.mu.Unlock()
		return nil, agentState{}, ErrNotEstablished
	}
	dtlsState, ok := s.dtlsConn.ConnectionState()
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
	state.Agent = cloneAgentState(state.Agent)
	// Certificate/key byte slices are immutable throughout a session.
	s.mu.Unlock()
	if !ok {
		return nil, agentState{}, errors.New("mediaworker: DTLS connection state unavailable")
	}
	binary, err := dtlsState.MarshalBinary()
	if err != nil {
		return nil, agentState{}, fmt.Errorf("mediaworker: snapshot DTLS: %w", err)
	}
	data, err := json.Marshal(snapshot{Version: sessionStateVersion, State: state, DTLSConnection: binary})
	return data, state.Agent, err
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
	if s.worker.cfg.Relay == nil {
		return nil
	}
	// Serialize copy+put so an older copy cannot overwrite a newer one. The
	// packet lock is never held during encoding or the fenced store operation.
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.mu.Lock()
	changed := !agentStateEqual(s.state.Agent, s.durableAgent)
	tooSoon := s.snapshotStored.Load() && time.Since(s.lastCheckpoint) < s.worker.cfg.Agent.SaveInterval
	s.mu.Unlock()
	if !force && changed && tooSoon {
		return errCheckpointSkipped
	}
	ctx, cancel := context.WithTimeout(parent, ownershipTimeout)
	defer cancel()
	s.mu.Lock()
	s.state.Checkpoint.Attempts++
	s.mu.Unlock()
	captured, err := s.worker.cfg.Relay.Owners.Clock(ctx, s.id)
	var state []byte
	var pair agentState
	if err == nil {
		state, pair, err = s.captureSnapshotAt(captured)
	}
	if err != nil {
		s.mu.Lock()
		s.state.Checkpoint.Failures++
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
		return errCheckpointSkipped
	}
	err = s.worker.cfg.Relay.Owners.PutState(ctx, lease, state)
	if err == nil {
		s.snapshotStored.Store(true)
		s.mu.Lock()
		s.state.Checkpoint.Successes++
		s.state.Checkpoint.CapturedAt = captured
		s.checkpointRates(&s.state.Checkpoint)
		s.durableAgent = pair
		s.lastCheckpoint = time.Now()
		s.mu.Unlock()
		metrics.CheckpointWrites.WithLabelValues("success").Inc()
	} else {
		s.mu.Lock()
		s.state.Checkpoint.Failures++
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
		}
	}
}
