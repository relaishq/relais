package mediaworker

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// SnapshotLimits are operator settings; registrations cannot change them.
// The encoded cap also bounds v1 component captures. Byte-rate and move quotas
// apply to v2 sessions. Zero fields resolve by session profile: ADR 0010
// for legacy sessions and ADR 0012 for v10 sessions.
type SnapshotLimits struct {
	MaxBytes            int
	BytesPerSecond      int
	BurstBytes          int
	MoveBytesPerSession int
	MoveBytesTotal      int64
}

func (l SnapshotLimits) defaults() SnapshotLimits {
	if l.MaxBytes == 0 {
		l.MaxBytes = 7168
	}
	if l.BytesPerSecond == 0 {
		l.BytesPerSecond = 81920
	}
	if l.BurstBytes == 0 {
		l.BurstBytes = 28672
	}
	if l.MoveBytesPerSession == 0 {
		l.MoveBytesPerSession = v2MoveSessionBytes
	}
	if l.MoveBytesTotal == 0 {
		l.MoveBytesTotal = v2MoveWorkerBytes
	}
	return l
}

// SplitDefaults resolves the gate-derived v10 profile. Explicit settings keep
// their meaning in both layouts; defaults are selected per session, not per worker.
func (l SnapshotLimits) SplitDefaults() SnapshotLimits { return l.splitDefaults() }

func (l SnapshotLimits) splitDefaults() SnapshotLimits {
	if l.BytesPerSecond == 0 {
		l.BytesPerSecond = 80000
	}
	if l.BurstBytes == 0 {
		l.BurstBytes = 40960
	}
	return l.defaults()
}
func (s *session) snapshotLimits() SnapshotLimits {
	if s.componentMode {
		return s.worker.splitLimits.splitDefaults()
	}
	return s.worker.cfg.SnapshotLimits
}

// Validate checks limits at the stock worker's 100 ms snapshot interval.
func (l SnapshotLimits) Validate() error { return l.ValidateForInterval(100 * time.Millisecond) }

// ValidateForInterval reserves one maximum-size forced write per second beyond
// periodic transport checkpoints. Intent admission also preserves one required
// checkpoint in the bucket; excess forced work waits for replenishment.
func (l SnapshotLimits) ValidateForInterval(interval time.Duration) error {
	if err := l.validateResourceLimits(); err != nil {
		return err
	}
	if interval <= 0 {
		return fmt.Errorf("%w: snapshot interval must be positive", agent.ErrCapacity)
	}
	numerator := int64(l.MaxBytes) * int64(time.Second)
	periodic := numerator / int64(interval)
	if numerator%int64(interval) != 0 {
		periodic++
	}
	minimum := periodic + int64(l.MaxBytes)
	if int64(l.BytesPerSecond) < minimum {
		return fmt.Errorf("%w: -agent-snapshot-bytes-per-second must be at least %d for snapshot interval %s", agent.ErrCapacity, minimum, interval)
	}
	return nil
}

// validateResourceLimits is independent of checkpoint cadence and agent kind.
func (l SnapshotLimits) validateResourceLimits() error {
	const maxByteFlag = 1<<30 - 1 // sums of two byte caps fit even a 32-bit int
	for _, v := range []struct {
		name    string
		value   int64
		maximum int64
	}{
		{"agent-max-snapshot-bytes", int64(l.MaxBytes), maxByteFlag},
		{"agent-snapshot-bytes-per-second", int64(l.BytesPerSecond), maxByteFlag},
		{"agent-snapshot-burst-bytes", int64(l.BurstBytes), maxByteFlag},
		{"agent-move-bytes-per-session", int64(l.MoveBytesPerSession), v2MoveSessionBytes},
		{"agent-move-bytes-total", l.MoveBytesTotal, 1 << 40},
	} {
		if v.value < 1 || v.value > v.maximum {
			return fmt.Errorf("%w: -%s must be between 1 and %d bytes", agent.ErrCapacity, v.name, v.maximum)
		}
	}
	if l.BurstBytes < 2*l.MaxBytes {
		return fmt.Errorf("%w: -agent-snapshot-burst-bytes must be at least %d", agent.ErrCapacity, 2*l.MaxBytes)
	}
	if l.MoveBytesTotal <= int64(l.MoveBytesPerSession)*8 {
		return fmt.Errorf("%w: -agent-move-bytes-total must be at least %d", agent.ErrCapacity, int64(l.MoveBytesPerSession)*8+1)
	}
	return nil
}

// effectiveAgentBounds resolves defaults and local ceilings without changing the
// declaration used as registration identity. State is never silently clamped.
func effectiveAgentBounds(declared agent.Limits, limits SnapshotLimits, split ...bool) agenthost.Bounds {
	if len(split) > 0 && split[0] {
		if declared.MaxStateBytes == 0 {
			declared.MaxStateBytes = 3072
		}
		if declared.MaxHistoryBytes == 0 {
			declared.MaxHistoryBytes = 8192
		}
	}
	b := agenthost.Bounds(declared).Defaults()
	if len(split) == 0 || !split[0] {
		b.MaxHistoryBytes = 0
	}
	b.MaxPendingBytes = min(b.MaxPendingBytes, v2HostMetadataBytes)
	b.MaxMailboxBytes = min(b.MaxMailboxBytes, limits.MoveBytesPerSession)
	b.MaxActiveTasks = min(b.MaxActiveTasks, 4)
	if b.MaxAudioBytes == 0 {
		b.MaxAudioBytes = b.MaxMailboxBytes
	} else {
		b.MaxAudioBytes = min(b.MaxAudioBytes, b.MaxMailboxBytes)
	}
	return b
}

// ValidateAgentRegistration checks declared crash-state capacity before startup.
// The measured transport baseline is an admission allowance only, never a claim
// about a live snapshot. Live admission and capture encode the actual snapshot.
func ValidateAgentRegistration(name string, kinds []string, bounds agent.Limits, limits SnapshotLimits, split ...bool) error {
	isSplit := len(split) > 0 && split[0]
	if isSplit {
		limits = limits.splitDefaults()
	} else {
		limits = limits.defaults()
	}
	declared := bounds
	bounds = agent.Limits(effectiveAgentBounds(declared, limits, isSplit))
	if declared.MaxHistoryBytes < 0 || declared.MaxHistoryBytes > componentMaxHistoryBytes || bounds.MaxStateBytes <= 0 || bounds.MaxStateBytes > 64<<10 || bounds.MaxPendingBytes <= 0 || bounds.MaxPendingBytes > v2HostMetadataBytes || bounds.MaxMailboxBytes <= 0 || bounds.MaxMailboxBytes > limits.MoveBytesPerSession || bounds.MaxActiveTasks <= 0 || bounds.MaxAudioBytes < 0 || bounds.MaxAudioDuration < 0 {
		return fmt.Errorf("%w: invalid limits for %q", agent.ErrIncompatibleAgent, name)
	}
	if isSplit && limits.BurstBytes < 2*splitComponentBudgets(limits.MaxBytes, bounds.MaxHistoryBytes).Total {
		return fmt.Errorf("%w: split burst cannot hold two maximum transactions for %q", agent.ErrCapacity, name)
	}
	const measuredTransport = 3474 // ADR 0010 C; rechecked from real payloads at runtime.
	if !isSplit && measuredTransport+snapshotEnvelopeBytes+base64.StdEncoding.EncodedLen(bounds.MaxStateBytes)+v2HostMetadataBytes > limits.MaxBytes {
		return fmt.Errorf("%w: declared state for %q cannot fit operator snapshot cap", agent.ErrStateTooLarge, name)
	}
	if isSplit && bounds.MaxStateBytes+6+componentCipherOverhead > limits.MaxBytes {
		return fmt.Errorf("%w: declared working component for %q cannot fit operator hot cap", agent.ErrStateTooLarge, name)
	}
	metadata, err := json.Marshal(v2Envelope{Name: name, Contract: v2Contract, TaskKinds: kinds, Limits: &declared, Quota: &snapshotQuota{Version: 1, Balance: quotaBalance(uint64(limits.BurstBytes)), At: time.Now().UTC()}})
	if err != nil {
		return err
	}
	if len(metadata) > bounds.MaxPendingBytes {
		return fmt.Errorf("%w: registration metadata", agent.ErrCapacity)
	}
	return nil
}

// validateRestoredAgentBounds checks the encoded content against this worker,
// independently of identity. Never truncate an adopted state or payload.
func (s *session) validateRestoredAgentBounds(b agenthost.Bounds, exact bool) error {
	a := s.state.Agent
	if len(a.History) > b.MaxHistoryBytes || (!s.componentMode && len(a.History) != 0) {
		return fmt.Errorf("%w: %w", agent.ErrCapacity, agent.ErrHistoryTooLarge)
	}
	if len(a.State.Bytes) > b.MaxStateBytes {
		return fmt.Errorf("%w: %w", agent.ErrCapacity, agent.ErrStateTooLarge)
	}
	metadata, err := json.Marshal(a.V2)
	if err != nil {
		return err
	}
	if len(metadata) > b.MaxPendingBytes {
		return fmt.Errorf("%w: restored host metadata exceeds local limit", agent.ErrCapacity)
	}
	if exact && a.V2.MoveOnlyLength > b.MaxMailboxBytes {
		return fmt.Errorf("%w: restored move data exceeds local limit", agent.ErrCapacity)
	}
	active := 0
	for _, t := range a.V2.Tasks {
		if t.Phase == taskStarted && !t.Final {
			active++
		}
	}
	if exact && active > b.MaxActiveTasks {
		return fmt.Errorf("%w: restored active tasks exceed local limit", agent.ErrCapacity)
	}
	p := a.V2.Playback
	if exact && (p.Length > b.MaxAudioBytes || (b.MaxAudioDuration > 0 && p.Duration > b.MaxAudioDuration)) {
		return fmt.Errorf("%w: restored audio exceeds local limit", agent.ErrCapacity)
	}
	return nil
}

// Include the conservative Redis encryption envelope even with a memory store,
// so a pool has one budget independent of its configured store implementation.
const snapshotEnvelopeBytes = 42

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

func (s *session) checkSnapshotSize(data []byte) error {
	if s.componentMode {
		_, err := s.componentAdmission(data)
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
func (s *session) admissionSnapshot(candidate agentState) (int, error) {
	data, err := s.captureAdmissionSnapshot(candidate)
	if err != nil {
		if err == ErrNotEstablished {
			if s.componentMode {
				// No negotiated transport exists yet. Reserve the full hot bound until
				// the first actual capture enforces the encoded component sizes.
				size := componentSizes{Hot: s.componentBudgets().Hot, Setup: s.componentBudgets().Setup, History: len(candidate.History) + 6 + componentCipherOverhead}
				size.Total = size.Hot + size.Setup + size.History
				return size.Hot, s.admitHistoryUpdate(candidate, size)
			}
			return 0, nil
		}
		return 0, err
	}
	if s.componentMode {
		size, err := s.componentAdmission(data)
		if err == nil {
			size.History += len(candidate.History)
			size.Total += len(candidate.History)
			err = s.componentBudgets().admitSizes(size)
		}
		if err == nil {
			err = s.admitHistoryUpdate(candidate, size)
		}
		return size.Hot, err
	}
	return len(data) + snapshotEnvelopeBytes, nil
}

// captureAdmissionSnapshot is the production Save capture, with history measured
// separately by its exact length. Its timestamps and quota use the store clock,
// matching a real writer even when worker and store clocks differ.
func (s *session) captureAdmissionSnapshot(candidate agentState) ([]byte, error) {
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
	data, _, err := s.captureSnapshotCandidate(captured, &captureCandidate)
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
func (s *session) admitHistoryUpdate(candidate agentState, size componentSizes) error {
	if !s.componentMode || candidate.V2 == nil {
		return nil
	}
	before := s.agent.latest
	if candidate.State.Version == before.State.Version && slices.Equal(candidate.History, before.History) {
		return nil
	}
	limits := s.snapshotLimits()
	now := s.taskNow()
	s.snapshotBudgetMu.Lock()
	available := s.snapshotBytesBudget.available(limits, now)
	// Future host/working changes can use the whole hot cap before this
	// history is captured. Reserve that cap, not today's smaller hot encoding.
	transaction := limits.MaxBytes + size.Setup + size.History
	required := transaction + limits.MaxBytes
	if s.snapshotWriteInFlight {
		required += s.componentBudgets().Total
	}
	if available >= float64(required) {
		s.snapshotHistoryHold = &historyWriteHold{version: candidate.State.Version, history: slices.Clone(candidate.History), bytes: transaction}
	}
	s.snapshotBudgetMu.Unlock()
	if available < float64(required) {
		return fmt.Errorf("%w: history transaction needs %d bytes, available %.0f", agent.ErrCapacity, required, available)
	}
	return nil
}

// A callback can time out after admission but before publication. Restore the
// reservation together with the app pair. A conservative maximum hold is safe:
// the next matching capture can spend it, while older captures cannot.
func (s *session) rollbackHistoryAdmission(before agentState) {
	if !s.componentMode {
		return
	}
	s.snapshotBudgetMu.Lock()
	s.snapshotHistoryHold = &historyWriteHold{version: before.State.Version, history: slices.Clone(before.History), bytes: s.componentBudgets().Total}
	s.snapshotBudgetMu.Unlock()
}

// A refused callback publishes one typed Capacity notice after rollback. It
// never repeatedly invokes an event whose state can never fit. Input progress
// records the explicit refusal, while the prior application state is retained.
var errSnapshotAdmissionCapacity = fmt.Errorf("%w: snapshot admission", agent.ErrCapacity)

func (s *session) refuseSnapshotEvent(event agent.Event, before agentState) error {
	h := s.agent.v2
	if event.Kind == agent.TaskMessage {
		// Replace the undelivered result, retaining its stable sequence. One Capacity
		// response is allowed; a Capacity handler which itself cannot fit fails closed.
		if event.Result.Outcome == agent.Capacity {
			return errSnapshotAdmissionCapacity
		}
		for _, t := range h.tasks {
			if t.intent.ID == event.Result.TaskID {
				if t.cancel != nil {
					t.cancel()
				}
				t.attempt++
				t.launched, t.prepared = false, false
				t.intent.Final, t.intent.Phase, t.intent.Terminal = true, taskTerminal, agent.Capacity
				t.intent.Published = event.Result.Sequence
				s.mu.Lock()
				for _, r := range t.results {
					delete(h.playback.payloads, audioKey{r.TaskID, r.Sequence})
				}
				s.mu.Unlock()
				t.results = []agent.TaskResult{{TaskID: t.intent.ID, Sequence: event.Result.Sequence, Turn: t.intent.Turn, Outcome: agent.Capacity, Final: true, Detail: "snapshot admission capacity"}}
				t.mailboxBytes = taskResultBytes(t.results[0])
				for _, r := range t.audio {
					t.mailboxBytes += taskRetainedAudioBytes(r)
				}
				s.retireTaskRequest(t)
				s.taskStateChanged()
				return s.taskBudget(h.tasks, before.V2.MoveOnlyLength)
			}
		}
		return errSnapshotAdmissionCapacity
	}
	next := cloneAgentState(before)
	id := deriveTaskID(s.id, h.registration.Name, event.ID, "relais/snapshot-capacity/v1")
	next.V2.Capacity = append(slices.Clone(next.V2.Capacity), taskRefusal{ID: id})
	switch event.Kind {
	case agent.AudioInput:
		next.Progress = event.Input.Progress
		next.Progress.Consumed = before.Progress.Consumed + 1
	case agent.Started:
		next.V2.Started = true
	case agent.TimerFired:
		next.V2.Timers = slices.DeleteFunc(next.V2.Timers, func(t hostTimer) bool { return t.ID == event.Timer.ID && t.Generation == event.Timer.Generation })
	case agent.PlaybackFinished:
		next.V2.Playback.Notify = slices.DeleteFunc(next.V2.Playback.Notify, func(t agent.Turn) bool { return t == event.Playback.Turn })
	default:
		return errSnapshotAdmissionCapacity
	}
	if err := s.taskMetadataBudget(next.V2.Tasks, next.V2.Capacity, false); err != nil {
		return err
	}
	s.compactControlMetadata(next.V2, min(h.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes))
	next.Revision++
	if _, err := s.admissionSnapshot(next); err != nil {
		return err
	}
	s.mu.Lock()
	h.playback.notify = slices.Clone(next.V2.Playback.Notify)
	s.mu.Unlock()
	s.agent.latest = next
	s.publishAgent()
	s.wantSnapshot()
	return nil
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
func (s *session) captureSnapshotQuota(now time.Time) *snapshotQuota {
	return s.captureSnapshotQuotaMode(now, false)
}

// Export cannot wait for storage. Reserve one maximum snapshot while a writer
// might still acknowledge, covering Clock, encoding and PutState under one flag.
func (s *session) captureSnapshotQuotaMode(now time.Time, exporting bool) *snapshotQuota {
	s.snapshotBudgetMu.Lock()
	tokens := s.snapshotBytesBudget.available(s.snapshotLimits(), now)
	if exporting && s.snapshotWriteInFlight {
		maximum := s.snapshotLimits().MaxBytes
		if s.componentMode {
			maximum = s.componentBudgets().Total
		}
		tokens = max(0, tokens-float64(maximum))
	}
	s.snapshotBudgetMu.Unlock()
	return &snapshotQuota{Version: 1, Balance: quotaBalance(uint64(max(0, tokens))), At: now}
}
func (s *session) restoreSnapshotQuota(q *snapshotQuota) error {
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
	s.snapshotBudgetMu.Lock()
	s.snapshotBytesBudget = snapshotBucket{tokens: float64(min(tokens, uint64(s.snapshotLimits().BurstBytes))), at: q.At}
	s.snapshotBudgetMu.Unlock()
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
