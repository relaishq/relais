// Package taskrun implements the experimental task API and its private host seam.
package taskrun

import (
	"context"
	"fmt"
	"slices"
	"time"
)

type TaskID string
type Turn uint64
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

type EncodedAudio struct {
	Format  string
	Packets [][]byte
}
type Chunk struct {
	Data  []byte
	Audio *EncodedAudio
}
type TaskResult struct {
	TaskID   TaskID
	Sequence uint64
	Turn     Turn
	Outcome  Outcome
	Chunk    Chunk
	Final    bool
	Detail   string
}

// TaskRun is owned by one host attempt. Spec returns an owned copy. After is
// the safe replay floor, which can exceed the application's committed cursor.
type TaskRun struct {
	spec  TaskSpec
	id    TaskID
	after uint64
	emit  func(context.Context, uint64, Chunk) error
}
type TaskHandler func(context.Context, *TaskRun) error

// New is an internal host seam, unavailable outside the module.
func New(spec TaskSpec, id TaskID, after uint64, emit func(context.Context, uint64, Chunk) error) *TaskRun {
	spec.Request = slices.Clone(spec.Request)
	return &TaskRun{spec: spec, id: id, after: after, emit: emit}
}
func (r *TaskRun) Spec() TaskSpec { s := r.spec; s.Request = slices.Clone(s.Request); return s }
func (r *TaskRun) ID() TaskID     { return r.id }
func (r *TaskRun) Key() string    { return string(r.id) }
func (r *TaskRun) After() uint64  { return r.after }

// Emit publishes an owned chunk or waits with cancellation for memory/rate
// admission. The first chunk is immediate; later chunks and handler terminals
// publish at most once per 100 ms. Batch at least 100 ms of encoded audio per
// chunk for continuous playout. A restored task with a prior publication waits
// one full interval before its next chunk. A chunk that can never fit returns ErrCapacity.
func (r *TaskRun) Emit(ctx context.Context, seq uint64, chunk Chunk) error {
	return r.emit(ctx, seq, chunk)
}

// Fail only constructs an error. The handler must return that error for the
// host to publish the supplied terminal outcome; ignoring it records nothing.
func (*TaskRun) Fail(outcome Outcome, detail string) error {
	if !ValidOutcome(outcome) || outcome == OK {
		return &Failure{Outcome: Failed, Detail: "invalid failure outcome"}
	}
	return &Failure{Outcome: outcome, Detail: detail}
}

type Failure struct {
	Outcome Outcome
	Detail  string
}

func (e *Failure) Error() string { return fmt.Sprintf("task: %s: %s", e.Outcome, e.Detail) }
func ValidOutcome(o Outcome) bool {
	switch o {
	case OK, Failed, Timeout, Canceled, OutcomeUnknown, Capacity:
		return true
	}
	return false
}
