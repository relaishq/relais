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

// SnapshotLimits are operator settings. They apply only to v2 sessions; a
// registration cannot change them. Zero fields use the ADR 0010 defaults.
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

// Validate rejects invalid operator settings, including a burst too small for
// one required snapshot. Worker flags supply explicit nonzero defaults.
func (l SnapshotLimits) Validate() error {
	if l.MaxBytes <= 0 || l.BytesPerSecond <= 0 || l.BurstBytes < l.MaxBytes || l.MoveBytesPerSession <= 0 || l.MoveBytesPerSession > v2MoveSessionBytes || l.MoveBytesTotal <= int64(l.MoveBytesPerSession)*8 {
		return fmt.Errorf("%w: invalid operator agent limits", agent.ErrCapacity)
	}
	return nil
}

// ValidateAgentRegistration checks declared crash-state capacity before startup.
// The measured transport baseline is an admission allowance only, never a claim
// about a live snapshot. Live admission and capture encode the actual snapshot.
func ValidateAgentRegistration(name string, kinds []string, bounds agent.Limits, limits SnapshotLimits) error {
	limits = limits.defaults()
	if bounds.MaxStateBytes <= 0 || bounds.MaxStateBytes > 64<<10 || bounds.MaxPendingBytes <= 0 || bounds.MaxPendingBytes > v2HostMetadataBytes || bounds.MaxMailboxBytes <= 0 || bounds.MaxMailboxBytes > limits.MoveBytesPerSession || bounds.MaxActiveTasks <= 0 || bounds.MaxAudioBytes < 0 || bounds.MaxAudioDuration < 0 {
		return fmt.Errorf("%w: invalid limits for %q", agent.ErrIncompatibleAgent, name)
	}
	const measuredTransport = 3474 // ADR 0010 C; rechecked from real payloads at runtime.
	if measuredTransport+snapshotEnvelopeBytes+base64.StdEncoding.EncodedLen(bounds.MaxStateBytes)+v2HostMetadataBytes > limits.MaxBytes {
		return fmt.Errorf("%w: declared state for %q cannot fit operator snapshot cap", agent.ErrStateTooLarge, name)
	}
	metadata, err := json.Marshal(v2Envelope{Name: name, Contract: v2Contract, TaskKinds: kinds, Bounds: agenthost.Bounds(bounds), Quota: &snapshotQuota{Version: 1, Balance: quotaBalance(uint64(limits.BurstBytes)), At: time.Now().UTC()}})
	if err != nil {
		return err
	}
	if len(metadata) > bounds.MaxPendingBytes {
		return fmt.Errorf("%w: registration metadata", agent.ErrCapacity)
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
	return min(float64(l.BurstBytes), b.tokens+now.Sub(b.at).Seconds()*float64(l.BytesPerSecond))
}
func (b *snapshotBucket) debit(l SnapshotLimits, n int, now time.Time) {
	b.tokens = b.available(l, now) - float64(n)
	b.at = now
}

func (s *session) checkSnapshotSize(data []byte) error {
	if len(data)+snapshotEnvelopeBytes > s.worker.cfg.SnapshotLimits.MaxBytes {
		return fmt.Errorf("%w: whole v2 snapshot is %d bytes (cap %d)", agent.ErrCapacity, len(data)+snapshotEnvelopeBytes, s.worker.cfg.SnapshotLimits.MaxBytes)
	}
	return nil
}

// admissionSnapshot measures the actual transport plus a proposed app/host pair.
// Before DTLS exists no ciphertext can be emitted or checkpointed; the first
// established capture enforces the full cap before starting the v2 actor.
func (s *session) admissionSnapshot(candidate agentState) (int, error) {
	data, _, err := s.captureSnapshotCandidate(time.Time{}, &candidate)
	if err != nil {
		if err == ErrNotEstablished {
			return 0, nil
		}
		return 0, err
	}
	return len(data) + snapshotEnvelopeBytes, nil
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
	s.snapshotBudgetMu.Lock()
	tokens := s.snapshotBytesBudget.available(s.worker.cfg.SnapshotLimits, now)
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
	s.snapshotBytesBudget = snapshotBucket{tokens: float64(min(tokens, uint64(s.worker.cfg.SnapshotLimits.BurstBytes))), at: q.At}
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
