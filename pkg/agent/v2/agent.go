// Package agent defines the supported v2 event contract. Pin pre-1.0 releases:
// public signatures remain compatible within a minor, with deprecation before
// removal in a later minor and migration notes for breaking minors. Snapshot
// compatibility is tested separately. The host delivers
// Started, AudioInput, TaskMessage, TimerFired, PlaybackFinished and Resumed;
// Control carries routed client messages.
// After a crash before Started is checkpointed, the target delivers Resumed
// before redelivering Started. An AudioInput Handle deadline outside Save drops
// that input after quarantine and Restore. TaskMessage Handle errors and deadlines
// fail closed after Restore; this also applies to admission Capacity callbacks.
// Control is refused with ErrControlNotReady until Started has been accepted.
// A Control Handle error or deadline outside Save is quarantined and restored,
// then refused with ErrControlRejected without consuming its ID or sequence.
// The client may retry the identical message. Save/Restore failures and invalid
// effects still fail closed; agents must use Tasks for external side effects.
// Admission refusals are queued as compact terminal intents. Each Capacity
// callback runs in a later transaction; Handle is never called recursively.
// Snapshot admission refuses oversized callback state with a later Capacity
// TaskMessage, after restoring prior app state. For non-task events this notice
// has a host-derived task ID and no payload. Refused input is counted as consumed
// with the prior app state plus the refusal, so it cannot replay without its
// typed outcome. A refused task result becomes a terminal Capacity at its stable
// sequence. Capacity handlers which themselves exceed limits fail closed.
// Required transport snapshots which cannot fit the operator byte/size budget
// refuse adoption or end the call with ErrCapacity. Store errors never produce
// Capacity or debit successful-write tokens.
// Save failures/deadlines, other callback
// errors, invalid state and unsupported effects fail closed. A Save that grows
// move-only length beyond the available worker memory fails closed with
// mediaworker.ErrMoveBudget.
// A callback whose compact task refusals cannot fit fails closed with
// ErrCapacity. At default limits for short-step read-only tasks and zero turns,
// widths 20/24/33 admit 4/3/0 tasks without Control, 4/3/1 with one receipt,
// and 4/2/0 with the full two-receipt Control cap. Other tasks receive Capacity.
// Control-bearing snapshots compact refusals; other registrations keep their
// original encoding/accounting. Retained Control can reduce task admission, but
// never makes an otherwise representable refusal-only callback terminal: it is
// admitted after dropping acknowledged receipt details, or restored and held
// under backpressure for unacknowledged receipts. Receipt backpressure retries
// only the blocked event at most once per 250 ms, or immediately after
// acknowledgement; unrelated callbacks and playback/mailbox pressure keep their
// normal admission behavior.
// Admission reserves refusal space
// before starting tasks; only excess tasks are refused.
// TimerFired and PlaybackFinished callback errors/deadlines also fail closed.
// Paced output marshal/encryption failures end the call without draining its cursor.
// Audio references name immutable task-emitted Opus packets. Playback continues
// independently of input and callbacks. A crash loses buffered audio and reports
// Resumed.MoveOnly=OutcomeUnknown; restart the turn on a later event.
// Handler terminal publication can wait up to 100 ms for Emit spacing; a move
// during that wait follows the task's Retry/Abandon rules or reports OutcomeUnknown.
// Registration identity persists declared Limits (zeros mean host defaults).
// Workers resolve effective limits locally and refuse restored content that
// exceeds those limits without truncation. Crash-lost move-only data and audio
// do not consume the adopting worker's payload limits; they report OutcomeUnknown.
package agent

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/relaishq/relais/internal/taskrun"
	legacy "github.com/relaishq/relais/pkg/agent"
)

var (
	ErrVersion           = legacy.ErrVersion
	ErrStateTooLarge     = legacy.ErrStateTooLarge
	ErrHistoryTooLarge   = errors.New("agent: history exceeds limit")
	ErrIncompatibleAgent = errors.New("agent: incompatible registration")
	// ErrBackpressure is retried internally. Emit waits with cancellation and
	// never returns this sentinel to a handler.
	ErrBackpressure = errors.New("agent: task mailbox backpressure")
	ErrCapacity     = errors.New("agent: task capacity")
)

// State holds opaque crash-recoverable working bytes and conversation history.
// Save and Restore cover both fields together. Save is not a durable acknowledgement.
// A crash can lose the current turn's audio and repeat work.
// State also holds O(1) metadata about move-only data.
// Save must not serialize, copy or hash move-only bytes. Version zero is reserved.
type State struct {
	Version        uint32
	Bytes          []byte
	History        []byte
	HasMoveOnly    bool
	MoveOnlyLength int
}

// MoveOnlyState is required when State.HasMoveOnly is true. Its callbacks run
// only at a paused exact export or exact restore, never at a checkpoint.
// SaveMoveOnly must be non-destructive and repeatable: a failed export can return
// the session to service, and a retry calls SaveMoveOnly again.
type MoveOnlyState interface {
	SaveMoveOnly(context.Context) ([]byte, error)
	RestoreMoveOnly(context.Context, []byte) error
}

// ResumeNotice reports lost app move data or outstanding task payloads. A
// completed, consumed ReadOnly task does not count as lost move data.
type ResumeNotice struct {
	legacy.ResumeNotice
	MoveOnly Outcome
}
type FactoryArgs struct {
	SessionID       string
	AgentName       string
	MaxStateBytes   int
	MaxHistoryBytes int
}
type Factory func(FactoryArgs) Agent

// Agent callbacks are serialized, deadline bounded and must not mutate after
// return. Handle must ignore unknown EventKinds without state changes or effects.
// Resumed must not start tasks. Restore must reject unknown State.Version values.
type Agent interface {
	Handle(context.Context, Event) (Effects, error)
	Save(context.Context) (State, error)
	Restore(context.Context, State) error
}

type EventID string
type EventKind string

const (
	Started          EventKind = "started"
	AudioInput       EventKind = "audio_input"
	TaskMessage      EventKind = "task_message"
	Resumed          EventKind = "resumed"
	TimerFired       EventKind = "timer_fired"
	PlaybackFinished EventKind = "playback_finished"
	Control          EventKind = "control"
)

type Event struct {
	// At is the callback dispatch time, using the same clock as host timers.
	// Every delivery gets a new time, including redelivery of the same EventID
	// after a retry, move or takeover. It is not the event's creation time.
	// It can step backward. Storeless sessions, and this owner's dispatches
	// before its first store-clock observation, use the worker clock instead.
	// The first observation can therefore jump by the worker/store clock skew.
	// Across owners it is comparable only as store-clock time, not worker time.
	At       time.Time
	Result   *TaskResult
	ID       EventID
	Kind     EventKind
	Input    *legacy.Input
	Resume   *ResumeNotice
	Control  *ControlMessage
	Timer    *TimerNotice
	Playback *PlaybackNotice
}

// Effects are validated atomically. Audio appends individual packet references
// only to the TaskMessage currently being delivered. Append references in that
// callback; older results can be pruned after acknowledgement. References below
// CancelBefore, the saved cancellation floor, or this callback's ClearAudioBefore
// are ignored. Timers use a session-wide generation, including after ID reuse.
// Timer IDs must be valid UTF-8 and at most 128 bytes.
// CancelBefore is a monotonic task turn floor; ClearAudioBefore clears queued
// packets immediately. NotifyPlaybackFinished reports local send completion.
// SetTimers resets each named timer with a new generation and a store-clock
// deadline. CancelTimers invalidates its current generation. Timers with After=0
// are due on the next actor transaction; negative durations are invalid.
type Effects struct {
	StartTasks             []TaskSpec
	CancelTasks            []TaskID
	CancelBefore           *Turn
	SetTimers              []TimerSpec
	CancelTimers           []string
	Audio                  []Audio
	ClearAudioBefore       *Turn
	NotifyPlaybackFinished *Turn
}
type Audio struct {
	Turn   Turn
	Source AudioRef
}
type AudioRef struct {
	TaskID       TaskID
	Sequence     uint64
	PacketOffset uint32
}
type TimerNotice struct {
	ID         string
	Generation uint64
}
type PlaybackNotice struct{ Turn Turn }

type TimerSpec struct {
	ID    string
	After time.Duration
}

// Task declarations describe bounded work executed outside agent callbacks.
type TaskID = taskrun.TaskID
type Turn = taskrun.Turn
type TaskClass = taskrun.TaskClass
type ResumePolicy = taskrun.ResumePolicy
type TaskSpec = taskrun.TaskSpec
type Outcome = taskrun.Outcome
type EncodedAudio = taskrun.EncodedAudio
type Chunk = taskrun.Chunk
type TaskResult = taskrun.TaskResult
type TaskRun = taskrun.TaskRun
type TaskHandler = taskrun.TaskHandler

const (
	ReadOnly       = taskrun.ReadOnly
	SideEffecting  = taskrun.SideEffecting
	Retry          = taskrun.Retry
	Abandon        = taskrun.Abandon
	OK             = taskrun.OK
	Failed         = taskrun.Failed
	Timeout        = taskrun.Timeout
	Canceled       = taskrun.Canceled
	OutcomeUnknown = taskrun.OutcomeUnknown
	Capacity       = taskrun.Capacity
)

type ControlMessage struct {
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
	Name     string `json:"name"`
	Data     []byte `json:"data,omitempty"`
}

// Validate checks the public Control bounds before any ownership lock or I/O.
func (m ControlMessage) Validate() error {
	if m.ID == "" || len(m.ID) > 128 || !utf8.ValidString(m.ID) || m.Sequence == 0 || m.Name == "" || len(m.Name) > 128 || !utf8.ValidString(m.Name) || len(m.Data) > 1024 {
		return ErrControlInvalid
	}
	return nil
}

// Limits bounds application state, serialized host metadata, and move memory.
// Zero fields use host defaults and remain zero in persisted identity. Effective
// limits respect local operator ceilings; registrations cannot increase budgets.
type Limits struct {
	MaxStateBytes    int
	MaxHistoryBytes  int `json:",omitempty"`
	MaxPendingBytes  int
	MaxMailboxBytes  int
	MaxActiveTasks   int
	MaxAudioBytes    int
	MaxAudioDuration time.Duration
}

// ControlReceipt identifies one locally committed callback. Checkpointed means
// a successful fenced checkpoint recorded it; local acceptance alone can be lost
// on a crash. Retry the identical ControlMessage until checkpointed or ErrControlStale.
type ControlReceipt struct {
	ID           string  `json:"id"`
	Sequence     uint64  `json:"sequence"`
	EventID      EventID `json:"event_id"`
	Checkpointed bool    `json:"checkpointed"`
}

var (
	ErrControlRejected = errors.New("agent: control Handle refused after Restore")
	ErrControlNotReady = errors.New("agent: Started not yet accepted")
	ErrControlInvalid  = errors.New("agent: invalid control message")
	ErrControlConflict = errors.New("agent: control ID or sequence conflicts")
	ErrControlOrder    = errors.New("agent: control sequence must be next in order")
	// ErrControlStale proves that this sequence was durably accepted and compacted.
	// A client retrying its own original message can stop retrying.
	ErrControlStale       = errors.New("agent: control sequence outside retained window")
	ErrControlUnsupported = errors.New("agent: control unsupported by registration or limits")
)
