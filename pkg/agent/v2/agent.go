// Package agent defines the experimental v2 event contract. It is experimental
// until ADR 0010 slice 6. Worker registration is not public yet. This slice hosts
// Started, AudioInput, TaskMessage, TimerFired, PlaybackFinished and Resumed;
// Control is reserved.
// After a crash before Started is checkpointed, the target delivers Resumed
// before redelivering Started. An AudioInput Handle deadline outside Save drops
// that input after quarantine and Restore. TaskMessage Handle errors and deadlines
// fail closed after Restore; this also applies to admission Capacity callbacks.
// Admission refusals are queued as compact terminal intents. Each Capacity
// callback runs in a later transaction; Handle is never called recursively.
// Save failures/deadlines, other callback
// errors, invalid state and unsupported effects fail closed. A Save that grows
// move-only length beyond the available worker memory fails closed with
// mediaworker.ErrMoveBudget.
// A callback whose compact task refusals cannot fit (about 33 at default
// bounds) fails closed with ErrCapacity and ends the call. At default bounds
// a width-20 fan-out admits 4 tasks; width 33 admits none. Admission reserves
// refusal space before starting tasks; only excess tasks are refused.
// TimerFired and PlaybackFinished callback errors/deadlines also fail closed.
// Paced output marshal/encryption failures end the call without draining its cursor.
// Audio references name immutable task-emitted Opus packets. Playback continues
// independently of input and callbacks. A crash loses buffered audio and reports
// Resumed.MoveOnly=OutcomeUnknown; restart the turn on a later event.
package agent

import (
	"context"
	"errors"
	"time"

	"github.com/relaishq/relais/internal/taskrun"
	legacy "github.com/relaishq/relais/pkg/agent"
)

var (
	ErrVersion           = legacy.ErrVersion
	ErrStateTooLarge     = legacy.ErrStateTooLarge
	ErrIncompatibleAgent = errors.New("agent: incompatible registration")
	// ErrBackpressure is retried internally. Emit waits with cancellation and
	// never returns this sentinel to a handler.
	ErrBackpressure = errors.New("agent: task mailbox backpressure")
	ErrCapacity     = errors.New("agent: task capacity")
)

// State holds crash-recoverable bytes and O(1) metadata about move-only data.
// Save must not serialize, copy or hash move-only bytes. Version zero is reserved.
type State struct {
	Version        uint32
	Bytes          []byte
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
	SessionID     string
	AgentName     string
	MaxStateBytes int
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
	Control          EventKind = "control" // reserved, never delivered in this slice
)

type Event struct {
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

// Task declarations remain experimental until the worker registration slice.
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
	ID       string
	Sequence uint64
	Name     string
	Data     []byte
}
