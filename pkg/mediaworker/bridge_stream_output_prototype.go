package mediaworker

// PROTOTYPE, NOT FOR MERGE: private encoded-output effect for the #254 spike.
// The audio task remains the bounded receipt/export lineage, but packets enter
// an accepted transaction without Emit spacing or a TaskMessage callback.
// No existing TaskRun.Emit limit, public Agent interface, or wire format changes.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

var errPrototypeOutputCallback = errors.New("prototype output callback failed")

const prototypeEncodedOutput agent.EventKind = "prototype_encoded_output"

type prototypeOutputAgent interface {
	prototypeOutput(context.Context, agent.TaskResult) (agent.Effects, error)
}
type prototypeOutputRequest struct {
	ctx    context.Context
	result agent.TaskResult
	done   chan error
}

func (s *session) submitPrototypeOutput(ctx context.Context, result agent.TaskResult) error {
	request := prototypeOutputRequest{ctx: ctx, result: result, done: make(chan error, 1)}
	select {
	case s.agent.v2.prototypeOutput <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	// Once handed to the actor, cancellation is an uncertain local outcome. Wait
	// for its bounded transaction; never retry different bytes under the same ID.
	select {
	case err := <-request.done:
		return err
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func (s *session) acceptPrototypeOutput(request prototypeOutputRequest) error {
	if _, ok := s.agent.v2.instance.(prototypeOutputAgent); !ok {
		return agent.ErrIncompatibleAgent
	}
	if err := request.ctx.Err(); err != nil {
		return err
	}
	h := s.agent.v2
	s.mu.Lock()
	paused := s.agent.paused || s.fenced.Load()
	s.mu.Unlock()
	if paused {
		return context.Canceled
	}
	r := request.result
	index := slices.IndexFunc(h.tasks, func(t *localTask) bool { return t.intent.ID == r.TaskID })
	if index < 0 || r.Chunk.Audio == nil || r.Final || r.Outcome != agent.OK {
		return agent.ErrIncompatibleAgent
	}
	prior := h.tasks[index]
	if prior.intent.Class != agent.ReadOnly || prior.intent.Kind != "bridge.audio/v1" {
		return agent.ErrIncompatibleAgent
	}
	if prior.intent.Final || r.Turn != prior.intent.Turn || r.Turn < s.agent.latest.V2.CancelFloor {
		return context.Canceled
	}
	if len(prior.results) > 0 {
		return agent.ErrBackpressure
	}
	if r.Sequence <= prior.intent.Published || r.Sequence == ^uint64(0) {
		return agent.ErrIncompatibleAgent
	}
	if len(r.Chunk.Audio.Packets) > 10 {
		return agent.ErrCapacity
	}
	if err := s.admitTaskAudio(r.Chunk.Audio); err != nil {
		return err
	}
	// Reuse the ordinary immutable audio result representation for exact moves.
	// This unpublished staging is invisible to exports, which serialize on actor
	// flush. The pacer has no reference until the accepted transaction commits.
	staged := *prior
	staged.intent.Published = r.Sequence
	staged.results = append(slices.Clone(prior.results), r)
	staged.mailboxBytes += taskResultBytes(r)
	tasks := slices.Clone(h.tasks)
	tasks[index] = &staged
	if err := s.taskBudget(tasks, s.agent.latest.V2.MoveOnlyLength); err != nil {
		return err
	}
	r.Chunk = cloneChunk(r.Chunk)
	staged.results[len(staged.results)-1] = r
	s.mu.Lock()
	err := s.registerAudioLocked(r)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	h.tasks = tasks
	event := agent.Event{Kind: prototypeEncodedOutput, ID: agent.EventID(fmt.Sprintf("prototype/output/%s/%d", r.TaskID, r.Sequence)), Result: &r}
	err = (acceptedEvents{s}).accept(event, eventLive, nil)
	if err != nil {
		h.tasks[index] = prior
		s.mu.Lock()
		delete(h.playback.payloads, audioKey{r.TaskID, r.Sequence})
		s.mu.Unlock()
		_ = s.taskBudget(h.tasks, s.agent.latest.V2.MoveOnlyLength)
	}
	return err
}
