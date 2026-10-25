package mediaworker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// snapshotBucket is guarded by snapshotBudgetMu. Snapshot writers serialize
// reservations with snapshotMu; admission never waits for store I/O. Only an
// acknowledged successful write debits bytes.
type snapshotBucket struct {
	tokens float64
	at     time.Time
}

func (b *snapshotBucket) available(l SnapshotLimits, now time.Time) float64 {
	if b.at.IsZero() {
		return float64(l.BurstBytes)
	}
	if now.Before(b.at) {
		// A read cannot rebase the bucket or mint refill after a clock step.
		// Only a successful writer rebases it onto the new store-clock value.
		return min(float64(l.BurstBytes), b.tokens)
	}
	return min(float64(l.BurstBytes), b.tokens+now.Sub(b.at).Seconds()*float64(l.BytesPerSecond))
}
func (b *snapshotBucket) debit(l SnapshotLimits, n int, now time.Time) {
	b.tokens = b.available(l, now) - float64(n)
	b.at = now
}

func (c *sessionCheckpoint) checkSnapshotSize(data []byte) error {
	s := c.session
	if s.componentMode {
		_, err := c.componentAdmission(data)
		return err
	}
	if len(data)+snapshotEnvelopeBytes > s.snapshotLimits().MaxBytes {
		return fmt.Errorf("%w: whole v2 snapshot is %d bytes (cap %d)", agent.ErrCapacity, len(data)+snapshotEnvelopeBytes, s.snapshotLimits().MaxBytes)
	}
	return nil
}

// admissionSnapshot measures the actual transport plus a proposed app/host pair.
// Before DTLS exists no ciphertext can be emitted or checkpointed; the first
// established capture enforces the full cap before starting the v2 actor.
func (c *sessionCheckpoint) admissionSnapshot(candidate, before agentState) (int, error) {
	s := c.session
	defer s.measureAdmission()()
	data, err := c.captureAdmissionSnapshot(candidate)
	if err != nil {
		if err == ErrNotEstablished {
			if s.componentMode {
				// No negotiated transport exists yet. Reserve the full hot bound until
				// the first actual capture enforces the encoded component sizes.
				size := componentSizes{Hot: c.componentBudgets().Hot, Setup: c.componentBudgets().Setup, History: len(candidate.History) + 6 + componentCipherOverhead}
				size.Total = size.Hot + size.Setup + size.History
				return size.Hot, c.admitHistoryUpdate(candidate, before, size)
			}
			return 0, nil
		}
		return 0, err
	}
	if s.componentMode {
		size, err := c.componentAdmission(data)
		if err == nil {
			size.History += len(candidate.History)
			size.Total += len(candidate.History)
			err = c.componentBudgets().admitSizes(size)
		}
		if err == nil {
			err = c.admitHistoryUpdate(candidate, before, size)
		}
		return size.Hot, err
	}
	return len(data) + snapshotEnvelopeBytes, nil
}

// captureAdmissionSnapshot is the production Save capture, with history measured
// separately by its exact length. Its timestamps and quota use the store clock,
// matching a real writer even when worker and store clocks differ.
// The timestamp only sizes fixed-width fields and is never published.
func (c *sessionCheckpoint) captureAdmissionSnapshot(candidate agentState) ([]byte, error) {
	s := c.session
	captured := time.Time{}
	if s.componentMode {
		captured = s.taskNow()
	} // include the full capture timestamp at admission
	captureCandidate := candidate
	if s.componentMode {
		// History is an opaque length-prefixed byte component: its size changes
		// exactly by len(History). Do not copy/base64/reparse it just to count it.
		// The actual capture and writer still encode and authenticate every byte.
		captureCandidate.History = nil
	}
	data, _, err := c.captureSnapshotCandidate(captured, &captureCandidate)
	return data, err
}

// historyWriteHold prevents an older capture or rotation from spending the
// bytes promised to an accepted app pair. The actor installs it before publishing
// that pair; only a matching acknowledgement clears it. Protected by budgetMu.
type historyWriteHold struct {
	version uint32
	history []byte
	bytes   int
}

func (h *historyWriteHold) matches(pair agentState) bool {
	return h != nil && h.version == pair.State.Version && slices.Equal(h.history, pair.History)
}

// admitHistoryUpdate reserves the candidate's full transaction plus one hot
// checkpoint. An older in-flight/uncertain write can still debit one maximum
// transaction, so reserve that too. Replacing an uncommitted candidate does not
// accumulate credits: every replacement is checked against the live bucket.
func (c *sessionCheckpoint) admitHistoryUpdate(candidate, before agentState, size componentSizes) error {
	s := c.session
	if !s.componentMode || candidate.V2 == nil {
		return nil
	}
	if candidate.State.Version == before.State.Version && slices.Equal(candidate.History, before.History) {
		return nil
	}
	limits := s.snapshotLimits()
	now := s.taskNow()
	c.snapshotBudgetMu.Lock()
	available := c.snapshotBytesBudget.available(limits, now)
	// Future host/working changes can use the whole hot cap before this
	// history is captured. Reserve that cap, not today's smaller hot encoding.
	transaction := limits.MaxBytes + size.Setup + size.History
	required := transaction + limits.MaxBytes
	if c.snapshotWriteInFlight {
		required += c.componentBudgets().Total
	}
	if available >= float64(required) {
		c.snapshotHistoryHold = &historyWriteHold{version: candidate.State.Version, history: slices.Clone(candidate.History), bytes: transaction}
	}
	c.snapshotBudgetMu.Unlock()
	if available < float64(required) {
		return fmt.Errorf("%w: history transaction needs %d bytes, available %.0f", agent.ErrCapacity, required, available)
	}
	return nil
}

// A callback can time out after admission but before publication. Restore the
// reservation together with the app pair. A conservative maximum hold is safe:
// the next matching capture can spend it, while older captures cannot.
func (c *sessionCheckpoint) rollback(before agentState) {
	s := c.session
	if !s.componentMode {
		return
	}
	c.snapshotBudgetMu.Lock()
	c.snapshotHistoryHold = &historyWriteHold{version: before.State.Version, history: slices.Clone(before.History), bytes: c.componentBudgets().Total}
	c.snapshotBudgetMu.Unlock()
}

// Quota is a versioned store-clock encoding. Fixed-width decimal bytes avoid
// a size/debit feedback loop when the balance changes its number of digits.
type snapshotQuota struct {
	Version uint32    `json:"v"`
	Balance string    `json:"b"`
	At      time.Time `json:"a"`
}

func quotaBalance(n uint64) string { return fmt.Sprintf("%020d", n) }
func (q snapshotQuota) balance() (uint64, error) {
	if q.Version != 1 || len(q.Balance) != 20 || q.At.IsZero() || strings.Trim(q.Balance, "0123456789") != "" {
		return 0, fmt.Errorf("%w: invalid snapshot quota", agent.ErrIncompatibleAgent)
	}
	n, err := strconv.ParseUint(q.Balance, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid snapshot quota balance", agent.ErrIncompatibleAgent)
	}
	return n, nil
}
func (c *sessionCheckpoint) captureSnapshotQuota(now time.Time) *snapshotQuota {
	return c.captureSnapshotQuotaMode(now, false)
}

// Export cannot wait for storage. Reserve one maximum snapshot while a writer
// might still acknowledge, covering Clock, encoding and PutState under one flag.
func (c *sessionCheckpoint) captureSnapshotQuotaMode(now time.Time, exporting bool) *snapshotQuota {
	s := c.session
	c.snapshotBudgetMu.Lock()
	tokens := c.snapshotBytesBudget.available(s.snapshotLimits(), now)
	if exporting && c.snapshotWriteInFlight {
		maximum := s.snapshotLimits().MaxBytes
		if s.componentMode {
			maximum = c.componentBudgets().Total
		}
		tokens = max(0, tokens-float64(maximum))
	}
	c.snapshotBudgetMu.Unlock()
	return &snapshotQuota{Version: 1, Balance: quotaBalance(uint64(max(0, tokens))), At: now}
}
func (c *sessionCheckpoint) restoreSnapshotQuota(q *snapshotQuota) error {
	s := c.session
	if q == nil {
		return nil
	} // previously published v8 snapshots had no byte limiter
	tokens, err := q.balance()
	if err != nil {
		return err
	}
	if q.At.After(s.taskNow()) {
		return sessionstore.ErrUnsafeCheckpointClock
	}
	c.snapshotBudgetMu.Lock()
	c.snapshotBytesBudget = snapshotBucket{tokens: float64(min(tokens, uint64(s.snapshotLimits().BurstBytes))), at: q.At}
	c.snapshotBudgetMu.Unlock()
	return nil
}

// MarshalJSON uses a fixed 17-byte versioned encoding: version, unsigned byte
// balance, and signed store-clock Unix nanoseconds, both integers big endian.
// Quota stays within the existing host allowance without expanding every task.
func (q snapshotQuota) MarshalJSON() ([]byte, error) {
	balance, err := q.balance()
	if err != nil {
		return nil, err
	}
	var wire [17]byte
	wire[0] = 1
	binary.BigEndian.PutUint64(wire[1:9], balance)
	binary.BigEndian.PutUint64(wire[9:17], uint64(q.At.UnixNano()))
	return json.Marshal(wire[:])
}
func (q *snapshotQuota) UnmarshalJSON(data []byte) error {
	var wire []byte
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("%w: snapshot quota encoding", agent.ErrIncompatibleAgent)
	}
	if len(wire) != 17 || wire[0] != 1 {
		return fmt.Errorf("%w: snapshot quota version or length", agent.ErrIncompatibleAgent)
	}
	q.Version = 1
	q.Balance = quotaBalance(binary.BigEndian.Uint64(wire[1:9]))
	q.At = time.Unix(0, int64(binary.BigEndian.Uint64(wire[9:17]))).UTC()
	return nil
}

func (c *sessionCheckpoint) validateAgentState(state legacy.State) error {
	s := c.session
	defer s.measureAdmission()()

	if state.Version == 0 {
		return legacy.ErrVersion
	}
	if len(state.Bytes) > s.worker.cfg.Agent.MaxStateBytes {
		return legacy.ErrStateTooLarge
	}
	if s.componentMode {
		s.mu.Lock()
		candidate := cloneAgentState(s.state.Agent)
		s.mu.Unlock()
		candidate.State = state
		candidate.Revision++
		// A zero timestamp has a shorter v10 encoding than a real capture.
		// Admission must include the timestamp even before the first checkpoint.
		captured := time.Now()
		_, _, err := c.captureSnapshotCandidate(captured, &candidate)
		if errors.Is(err, agent.ErrCapacity) {
			return legacy.ErrStateTooLarge
		}
		if errors.Is(err, ErrNotEstablished) {
			// Before handshake, transport progress has the same fixed-width wire
			// representation. Admit the candidate against the known host/track state
			// without requiring negotiated setup secrets or arming a DTLS bound.
			s.mu.Lock()
			value := snapshot{Version: s.state.Version, State: s.state, split: &componentSnapshotState{history: bytes.Clone(s.componentHistory)}}
			value.State.Agent = candidate
			value.State.Checkpoint.CapturedAt = captured
			value.State.SRTP.Inbound = maps.Clone(s.state.SRTP.Inbound)
			value.State.SRTP.Received = maps.Clone(s.state.SRTP.Received)
			s.mu.Unlock()
			value.DTLSConnection, err = joinDTLSComponents(componentDTLSSetup{}, componentDTLSProgress{LocalEpoch: 1})
			if err == nil {
				var data []byte
				data, err = json.Marshal(value)
				if err == nil {
					_, err = c.componentAdmission(data)
				}
			}
			if errors.Is(err, agent.ErrCapacity) {
				return legacy.ErrStateTooLarge
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// A complete candidate can reserve history. A host request is preflight only:
// it never installs a hold, debits quota, or publishes an app/host pair.
type checkpointAdmission struct {
	candidate  agentState
	prior      agentState
	sideEffect bool
	event      bool
	legacy     *legacy.State
	host       *checkpointHostPreflight
}

func (c *sessionCheckpoint) admit(r checkpointAdmission) (int, error) {
	if r.host != nil {
		return c.preflightHost(*r.host)
	}
	if r.legacy != nil {
		return 0, c.validateAgentState(*r.legacy)
	}
	size, err := c.admissionSnapshot(r.candidate, r.prior)
	if err != nil {
		if r.event && errors.Is(err, agent.ErrCapacity) {
			return size, fmt.Errorf("%w: %w", errSnapshotAdmissionCapacity, err)
		}
		return size, err
	}
	s := c.session
	if size > 0 && r.sideEffect {
		limits := s.snapshotLimits()
		required := size + limits.MaxBytes
		if required > limits.BurstBytes {
			return 0, errSnapshotAdmissionCapacity
		}
		now := s.taskNow()
		c.snapshotBudgetMu.Lock()
		available := c.snapshotBytesBudget.available(limits, now)
		c.snapshotBudgetMu.Unlock()
		if available < float64(required) {
			return 0, errTaskAdmissionBackpressure
		}
	}
	return size, nil
}

// The envelope is already projected by the host. Effects are supplied only by
// the playback caller to preserve its lazy, audio-only retry classification.
type checkpointHostPreflight struct {
	envelope     v2Envelope
	taskHeadroom bool
	effects      *agent.Effects
	event        agent.Event
}

func (c *sessionCheckpoint) preflightHost(r checkpointHostPreflight) (int, error) {
	s := c.session
	envelope := r.envelope
	if s.agent.v2.bridge != nil {
		s.mu.Lock()
		s.projectBridgeLocked(&envelope)
		s.mu.Unlock()
	}
	envelope.Quota = c.captureSnapshotQuota(s.taskNow())
	limit := min(s.agent.v2.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes)
	metadata := s.compactControlMetadata(&envelope, limit)
	if s.agent.v2.bridge != nil && envelope.Bridge != nil {
		base := envelope
		base.Bridge = nil
		base.Playback = playbackState{}
		baseCharge := s.compactControlMetadata(&base, limit-bridgeMetadataPartition)
		if baseCharge+bridgeMetadataPartition > limit {
			return baseCharge + bridgeMetadataPartition, errTaskAdmissionBackpressure
		}
	}

	if r.taskHeadroom {
		envelope = taskMetadataGuard(envelope)
		guarded := s.compactControlMetadata(&envelope, limit)
		if guarded > metadata {
			metadata = guarded
		}
	}
	if r.effects == nil {
		if metadata > limit {
			return metadata, agent.ErrCapacity
		}
		return metadata, nil
	}
	if metadata > limit {
		if len(r.effects.Audio) > 0 {
			e := *r.effects
			e.Audio = nil
			s.mu.Lock()
			envelope.Playback = s.projectedPlaybackLocked(e, r.event).snapshot()
			s.mu.Unlock()
			if s.compactControlMetadata(&envelope, limit) <= limit {
				return metadata, errTaskAdmissionBackpressure
			}
		}
		without := envelope
		without.Controls = nil
		if envelope.ControlHigh > 0 && v2MetadataCharge(without, limit) <= limit {
			return metadata, errControlReceiptBackpressure
		}
		return metadata, fmt.Errorf("%w: playback/timer metadata", agent.ErrCapacity)
	}
	return metadata, nil
}
