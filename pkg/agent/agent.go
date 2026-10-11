// Package agent defines per-call, audio-only applications. Audio is encoded
// Opus; RTP timestamps use the 48 kHz Opus clock. Video never enters an agent.
package agent

import (
	"context"
	"errors"
	"time"
)

const DefaultMaxStateBytes = 8 << 10

var (
	ErrVersion       = errors.New("agent: unknown state version")
	ErrStateTooLarge = errors.New("agent: state exceeds size limit")
	ErrSave          = errors.New("agent: state save failed")
	ErrRestore       = errors.New("agent: state restore failed")
)

// State is an application's versioned snapshot. Version zero is reserved.
// Bytes must include all application state needed for exact continuation.
type State struct {
	Version uint32
	Bytes   []byte
}

// Progress identifies the last successfully consumed input in the caller's
// inbound SRTP index space, not the worker's outbound sequence space.
type Progress struct {
	SSRC      uint32
	Index     uint64
	Timestamp uint32
	Consumed  uint64
}

// Input owns its payload; the callback can keep it until it returns.
type Input struct {
	Progress Progress
	Payload  []byte
	Marker   bool
}

// Output replaces one input's audio with one encoded Opus packet. Nil Audio
// suppresses output; a non-nil empty slice is an empty RTP payload. The host
// keeps the input's timestamp. Custom output sequence numbers are contiguous
// within a tenure and spend the existing safety margin on takeover. Suppressed
// inputs do not consume output indexes. Built-in Echo keeps its source gaps.
// Changed must be true whenever any state needed for continuation changes.
type Output struct {
	Audio   []byte
	Changed bool
}

type ResumeKind string

const (
	PlannedMove ResumeKind = "planned_move"
	Takeover    ResumeKind = "takeover"
)

// DuplicateWindow is the caller's inbound SRTP index range that may replay.
// The resume path supplies this evidence; an absent window promises no replay
// in the current implementation. Ticket #27 will populate it on takeover.
type DuplicateWindow struct {
	SSRC  uint32
	First uint64
	Last  uint64
}

type ResumeNotice struct {
	Kind                 ResumeKind
	CheckpointAge        time.Duration
	SnapshotAge          time.Duration
	InputMayBeDuplicated bool
	DuplicateWindows     []DuplicateWindow
	Progress             Progress
}

// Agent callbacks are serialized and each receives a deadline. Save encodes
// state; the host rate-limits durable saves. Save and Restore must be inverses,
// and Restore must reject unsupported versions with ErrVersion. The host copies
// returned bytes. Callbacks must honor cancellation and must not start work
// that mutates the agent after returning. External side effects are not made
// transactional by this interface; replay-tolerant apps must deduplicate them.
type Agent interface {
	Process(context.Context, Input) (Output, error)
	Save(context.Context) (State, error)
	Restore(context.Context, State) error
	Resume(context.Context, ResumeNotice) error
}

// Factory creates a fresh independent agent for each session, including resume.
// It must be quick and must not block; initialization belongs in Restore.
type Factory func(sessionID string) Agent

// Decoder is a seam for an optional Opus adapter. A stateful decoder must put
// its own continuation state in the application's State, or explicitly reset
// on Resume. No codec or native library is required by this package.
type Decoder interface {
	Decode(context.Context, []byte) (PCM, error)
}
type PCM struct {
	Samples    []int16
	SampleRate int
	Channels   int
}

// Echo is the default, stateless audio agent.
type Echo struct{}

func (*Echo) Process(_ context.Context, in Input) (Output, error) {
	return Output{Audio: in.Payload}, nil
}
func (*Echo) Save(context.Context) (State, error) { return State{Version: 1}, nil }
func (*Echo) Restore(_ context.Context, state State) error {
	if state.Version != 1 {
		return ErrVersion
	}
	if len(state.Bytes) != 0 {
		return errors.New("agent: invalid echo state")
	}
	return nil
}
func (*Echo) Resume(context.Context, ResumeNotice) error { return nil }
