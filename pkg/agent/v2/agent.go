// Package agent defines the experimental v2 event contract. It is experimental
// until ADR 0010 slice 6. Worker registration is not public yet. This slice hosts
// Started, AudioInput and Resumed; Control is reserved and is never delivered.
// Task, timer and playback effect declarations describe requests only: every
// nonempty effect is rejected as unsupported until its host slice is implemented.
package agent

import (
	"context"
	"errors"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
)

var (
	ErrVersion           = legacy.ErrVersion
	ErrStateTooLarge     = legacy.ErrStateTooLarge
	ErrIncompatibleAgent = errors.New("agent: incompatible registration")
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
type MoveOnlyState interface {
	SaveMoveOnly(context.Context) ([]byte, error)
	RestoreMoveOnly(context.Context, []byte) error
}

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
type TaskID string
type Turn uint64
type EventKind string

const (
	Started    EventKind = "started"
	AudioInput EventKind = "audio_input"
	Resumed    EventKind = "resumed"
	Control    EventKind = "control" // reserved, never delivered in this slice
)

type Event struct {
	ID      EventID
	Kind    EventKind
	Input   *legacy.Input
	Resume  *ResumeNotice
	Control *ControlMessage
}

// Effects are validated atomically. Nonempty requests currently return a clear
// unsupported error; the host never silently drops them. Task execution, timers
// and paced audio are deferred to the next implementation slices.
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
type TimerSpec struct {
	ID    string
	After time.Duration
}
type TaskClass string
type ResumePolicy string

const (
	ReadOnly      TaskClass    = "read_only"
	SideEffecting TaskClass    = "side_effecting"
	Retry         ResumePolicy = "retry"
	Abandon       ResumePolicy = "abandon"
)

type TaskSpec struct {
	Step    string
	Kind    string
	Turn    Turn
	Class   TaskClass
	Resume  ResumePolicy
	Request []byte
	Timeout time.Duration
}
type Outcome string

const (
	OK             Outcome = "ok"
	Failed         Outcome = "failed"
	Timeout        Outcome = "timeout"
	Canceled       Outcome = "canceled"
	OutcomeUnknown Outcome = "outcome_unknown"
	Capacity       Outcome = "capacity"
)

type ControlMessage struct {
	ID       string
	Sequence uint64
	Name     string
	Data     []byte
}
