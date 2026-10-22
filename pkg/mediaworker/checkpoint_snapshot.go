package mediaworker

import (
	"bytes"
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

func (c *sessionCheckpoint) snapshotBytes() ([]byte, error) {
	return c.snapshotBytesAt(time.Time{})
}

func (c *sessionCheckpoint) snapshotBytesAt(captured time.Time) ([]byte, error) {
	data, _, err := c.captureSnapshotAt(captured)
	return data, err
}

// captureSnapshotAt returns the exact owned pair encoded in this snapshot.
func (c *sessionCheckpoint) captureSnapshotAt(captured time.Time) ([]byte, agentState, error) {
	return c.captureSnapshotCandidate(captured, nil)
}

func (c *sessionCheckpoint) captureSnapshotCandidate(captured time.Time, candidate *agentState) ([]byte, agentState, error) {
	data, pair, _, err := c.captureSnapshotCandidateMode(captured, candidate, false)
	return data, pair, err
}

func (c *sessionCheckpoint) captureSnapshotCandidateMode(captured time.Time, candidate *agentState, writing bool) ([]byte, agentState, dtlsSequenceCheckpoint, error) {
	s := c.session
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
	if ok && writing && c.dtlsProvisional == nil && c.dtlsAcknowledged == nil {
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
		if err := c.armDTLSBound(next); err != nil {
			s.mu.Unlock()
			return nil, agentState{}, dtlsSequenceCheckpoint{}, err
		}
	}
	state := s.state
	if !captured.IsZero() {
		state.Checkpoint.CapturedAt = captured
		state.Checkpoint.Successes++ // only durable if this put succeeds
		if !c.lastCheckpoint.IsZero() {
			state.Checkpoint.AgeAtCapture = time.Since(c.lastCheckpoint)
		}
	}
	s.checkpointRates(&state.Checkpoint)
	state.SRTP.Inbound = maps.Clone(state.SRTP.Inbound)
	state.SRTP.Received = maps.Clone(state.SRTP.Received)
	if candidate != nil {
		state.Agent = cloneAgentState(*candidate)
	} else {
		state.Agent = cloneAgentState(state.Agent)
	}
	var split *componentSnapshotState
	if s.componentMode {
		history := s.componentHistory
		if state.Agent.V2 != nil {
			history = state.Agent.History
		}
		split = &componentSnapshotState{history: bytes.Clone(history)}
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
		state.Agent.V2.Quota = c.captureSnapshotQuota(now)
	}
	value := snapshot{Version: state.Version, State: state, DTLSConnection: binary, split: split}
	data, err := json.Marshal(value)
	if err == nil && state.Agent.V2 != nil && !captured.IsZero() && split == nil {
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
	if err == nil && (state.Agent.V2 != nil || split != nil) {
		err = c.checkSnapshotSize(data)
	}
	// Return the counter from this owned connection-state capture, never a live
	// counter read after PutState or a second parse of the full snapshot JSON.
	return data, state.Agent, next, err
}

// Snapshot ordering: copy+put is serialized by snapshotMu, preventing an old
// copy from overwriting a newer one. Copy under mu; encode/store outside it.
// Persist after handshake/resume, every configurable 100 ms, and on ROC or
// first-packet changes. Wakeups coalesce without storage on the packet path.

// errCheckpointSkipped is not a durable acknowledgment. Resume and replay
// reservations use force=true and cannot skip their required checkpoint.
var errCheckpointSkipped = errors.New("mediaworker: checkpoint rate limited")

func (c *sessionCheckpoint) persistSnapshotBudget(parent context.Context, force, intent bool) error {
	s := c.session
	forcedStarted := time.Now()
	measuredForced := false
	defer func() {
		if measuredForced {
			ns := uint64(time.Since(forcedStarted))
			s.worker.metrics.forcedWallNS.Add(ns)
			for {
				old := s.worker.metrics.forcedMaxNS.Load()
				if ns <= old || s.worker.metrics.forcedMaxNS.CompareAndSwap(old, ns) {
					break
				}
			}
		}
	}()

	if s.worker.cfg.Relay == nil {
		return nil
	}
	// Serialize copy+put so an older copy cannot overwrite a newer one. The
	// packet lock is never held during encoding or the fenced store operation.
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	if c.componentPending != nil {
		ctx, cancel := context.WithTimeout(parent, ownershipTimeout)
		err := c.settleComponentWrite(ctx)
		cancel()
		if err != nil {
			if errors.Is(err, sessionstore.ErrLeaseLost) {
				s.mu.Lock()
				s.fenced.Store(true)
				s.mu.Unlock()
				s.close()
			}
			return err
		}
	}
	s.mu.Lock()
	v2 := s.state.Agent.V2 != nil
	if v2 && (s.fenced.Load() || s.ctx.Err() != nil) {
		s.mu.Unlock()
		return errHandedOver
	}
	changed := !agentStateEqual(s.state.Agent, c.durableAgent)
	tooSoon := c.snapshotStored.Load() && time.Since(c.lastCheckpoint) < s.worker.cfg.Agent.SaveInterval
	if !force && changed && tooSoon {
		s.mu.Unlock()
		return c.noteSnapshotSkip(checkpointSkipInterval)
	}
	if v2 {
		c.snapshotBudgetMu.Lock()
		c.snapshotWriteInFlight = true
		c.snapshotBudgetMu.Unlock()
	}
	s.mu.Unlock()
	if v2 {
		defer func() {
			c.snapshotBudgetMu.Lock()
			// An unresolved write may still publish. Its maximum reservation survives
			// the local timeout until tuple settlement proves its outcome.
			c.snapshotWriteInFlight = c.componentPending != nil
			c.snapshotBudgetMu.Unlock()
		}()
	}
	ctx, cancel := context.WithTimeout(parent, ownershipTimeout)
	defer cancel()
	s.mu.Lock()
	s.state.Checkpoint.Attempts++
	if v2 && intent {
		s.worker.metrics.forcedAttempts.Add(1)
		measuredForced = true
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
		state, pair, dtlsNext, err = c.captureSnapshotCandidateMode(captured, nil, true)
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
	capturedChanged := !agentStateEqual(pair, c.durableAgent)
	capturedTooSoon := c.snapshotStored.Load() && time.Since(c.lastCheckpoint) < s.worker.cfg.Agent.SaveInterval
	lease := s.lease
	s.mu.Unlock()
	if !force && capturedChanged && capturedTooSoon {
		s.mu.Lock()
		s.state.Checkpoint.Attempts-- // discarded capture is not a write attempt
		s.mu.Unlock()
		return c.noteSnapshotSkip(checkpointSkipInterval)
	}
	wireBytes := len(state) + snapshotEnvelopeBytes
	if pair.V2 != nil && !s.componentMode {
		if err := c.checkSnapshotSize(state); err != nil {
			return err
		}
		c.snapshotBudgetMu.Lock()
		available := c.snapshotBytesBudget.available(s.snapshotLimits(), captured)
		c.snapshotBudgetMu.Unlock()
		required := wireBytes
		if intent {
			required += s.snapshotLimits().MaxBytes
		}
		if available < float64(required) {
			if intent {
				return c.noteSnapshotSkip(checkpointSkipBudget)
			}
			return fmt.Errorf("%w: required v2 snapshot byte budget", agent.ErrCapacity)
		}
	}
	if s.componentMode {
		wireBytes, err = c.writeComponents(ctx, lease, state, pair, dtlsNext, captured, intent)
	} else {
		err = s.worker.cfg.Relay.Owners.PutState(ctx, lease, state)
	}
	if err == nil {
		c.ackSnapshot(pair, dtlsNext, captured, wireBytes, intent)
	} else if errors.Is(err, errCheckpointSkipped) {
		err = c.noteSnapshotSkip(checkpointSkipBudget)
	} else if c.componentPending == nil {
		c.failSnapshot(v2, intent)
	}

	if errors.Is(err, sessionstore.ErrLeaseLost) {
		s.mu.Lock()
		s.fenced.Store(true)
		s.mu.Unlock()
		s.close()
	}
	return err
}

const (
	checkpointSkipBudget   = "budget"
	checkpointSkipInterval = "interval"
)

func (c *sessionCheckpoint) noteSnapshotSkip(reason string) error {
	s := c.session
	if reason == checkpointSkipInterval {
		s.worker.metrics.snapshotIntervalSkips.Add(1)
	} else {
		s.worker.metrics.snapshotBudgetSkips.Add(1)
	}
	metrics.CheckpointWrites.WithLabelValues("skipped-" + reason).Inc()
	return errCheckpointSkipped
}

// Unknown component outcomes are counted only when settlement resolves them.
func (c *sessionCheckpoint) failSnapshot(v2, intent bool) {
	s := c.session
	s.mu.Lock()
	s.state.Checkpoint.Failures++
	if v2 && intent {
		s.worker.metrics.forcedFailures.Add(1)
	}
	s.mu.Unlock()
	metrics.CheckpointWrites.WithLabelValues("failure").Inc()
}

func (c *sessionCheckpoint) request() {
	select {
	case c.snapshotWanted <- struct{}{}:
	default:
	}
}

func (c *sessionCheckpoint) run(parent context.Context) {
	s := c.session
	if s.worker.cfg.Relay == nil {
		return
	}
	ticker := time.NewTicker(s.worker.cfg.SnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
		case <-c.snapshotWanted:
		}
		if s.worker.paused.Load() {
			continue
		}
		workerprobe.BeforeSnapshot(s.worker.localAddr, parent, s.id)
		if parent.Err() != nil {
			return
		}
		if err := c.persist(parent, checkpointOptional); err != nil && !errors.Is(err, ErrNotEstablished) && !errors.Is(err, errCheckpointSkipped) && parent.Err() == nil {
			c.warnSnapshot(err)
			if errors.Is(err, agent.ErrCapacity) {
				s.agentFailed(err)
				return
			}
		}
	}
}

// warnSnapshot is called outside the packet lock. Repeated invalid captures
// remain refused, with one visible warning rather than a 100 ms log flood.
func (c *sessionCheckpoint) warnSnapshot(err error) {
	s := c.session
	warn := func() { s.log.Warnf("session %s: snapshot: %v", s.id, err) }
	if errors.Is(err, errDTLSCheckpointEpoch) {
		c.dtlsEpochWarning.Do(warn)
		return
	}
	warn()
}

func (c *sessionCheckpoint) ackSnapshot(pair agentState, dtlsNext dtlsSequenceCheckpoint, captured time.Time, wireBytes int, intent bool) {
	s := c.session
	if pair.V2 != nil {
		c.snapshotBudgetMu.Lock()
		c.snapshotBytesBudget.debit(s.snapshotLimits(), wireBytes, captured)
		if c.snapshotHistoryHold.matches(pair) {
			c.snapshotHistoryHold = nil
		}
		s.worker.metrics.snapshotDebitBytes.Add(uint64(wireBytes))
		if intent {
			s.worker.metrics.forcedSuccesses.Add(1)
		}
		c.snapshotWriteInFlight = false
		c.snapshotBudgetMu.Unlock()
	}
	c.snapshotStored.Store(true)
	s.mu.Lock()
	bound := c.dtlsAcknowledged
	if bound == nil {
		bound = c.dtlsProvisional
	}
	// Keep the latch on identical acknowledgments: retransmission spam must
	// not request another write until the acknowledged bound actually moves.
	if bound == nil || *bound != dtlsNext {
		c.dtlsCheckpointRequested = false
	}
	c.dtlsAcknowledged = &dtlsNext
	c.dtlsProvisional = nil
	s.state.Checkpoint.Successes++
	s.state.Checkpoint.CapturedAt = captured
	s.checkpointRates(&s.state.Checkpoint)
	c.durableAgent = pair
	if pair.V2 != nil {
		c.controlAck = max(c.controlAck, pair.V2.ControlHigh)
	}
	c.lastCheckpoint = time.Now()
	s.mu.Unlock()
	metrics.CheckpointWrites.WithLabelValues("success").Inc()
}
