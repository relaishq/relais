package mediaworker

import (
	"errors"
	"fmt"
	"slices"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

func (s *session) prepareTaskEffects(before v2Envelope, cause agent.Event, effects agent.Effects, base []*localTask, refusals []taskRefusal, projection ...playbackState) ([]*localTask, []agent.TaskResult, error) {
	h := s.agent.v2
	next := slices.Clone(base)
	var refused []agent.TaskResult
	steps := map[string]bool{}
	pending := map[agent.TaskID]bool{}
	for _, r := range refusals {
		pending[r.ID] = true
	}
	now := s.taskNow()
	// Reserve the complete callback's compact outcomes before admitting a
	// task. Admission never evicts an already admitted task to fit refusals.
	var anticipated []taskRefusal
	for _, spec := range effects.StartTasks {
		id := deriveTaskID(s.id, h.registration.Name, cause.ID, spec.Step)
		if !pending[id] && !slices.ContainsFunc(base, func(t *localTask) bool { return t.intent.ID == id }) {
			anticipated = append(anticipated, taskRefusal{ID: id, Turn: spec.Turn})
		}
	}
	for _, spec := range effects.StartTasks {
		if spec.Step == "" || spec.Kind == "" || spec.Timeout < 0 || steps[spec.Step] ||
			(spec.Class != agent.ReadOnly && spec.Class != agent.SideEffecting) ||
			(spec.Class == agent.SideEffecting && spec.Resume != agent.Retry && spec.Resume != agent.Abandon) ||
			(spec.Resume != "" && spec.Resume != agent.Retry && spec.Resume != agent.Abandon) {
			return nil, nil, fmt.Errorf("%w: invalid task specification or duplicate Step", agent.ErrIncompatibleAgent)
		}
		if h.registration.Tasks[spec.Kind] == nil {
			return nil, nil, fmt.Errorf("%w: missing task handler %q", agent.ErrIncompatibleAgent, spec.Kind)
		}
		steps[spec.Step] = true
		id := deriveTaskID(s.id, h.registration.Name, cause.ID, spec.Step)
		anticipated = slices.DeleteFunc(anticipated, func(r taskRefusal) bool { return r.ID == id })
		if pending[id] {
			continue
		}
		capacity := agent.TaskResult{TaskID: id, Sequence: 1, Turn: spec.Turn, Outcome: agent.Capacity, Final: true, Detail: "task admission capacity"}
		floor := before.CancelFloor
		if effects.CancelBefore != nil {
			floor = max(floor, *effects.CancelBefore)
		}
		if spec.Turn < floor {
			return nil, nil, fmt.Errorf("%w: task below cancellation turn floor", agent.ErrIncompatibleAgent)
		}
		if len(spec.Request) > h.registration.Bounds.MaxMailboxBytes || len(spec.Step)+len(spec.Kind) > h.registration.Bounds.MaxPendingBytes || (spec.Class == agent.SideEffecting && len(spec.Request) > h.registration.Bounds.MaxPendingBytes) {
			refused = append(refused, capacity)
			continue
		}
		digest := taskDigest(spec)
		duplicate := false
		for _, t := range next {
			if t.intent.ID == id {
				if !taskDigestMatches(spec, t.intent.Digest) {
					return nil, nil, fmt.Errorf("%w: conflicting task arguments", agent.ErrIncompatibleAgent)
				}
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		intent := taskIntent{ID: id, Step: spec.Step, Kind: spec.Kind, Turn: spec.Turn, Class: spec.Class, Resume: spec.Resume, RequestLength: len(spec.Request), Digest: digest, Timeout: spec.Timeout, Phase: taskNever}
		if cause.Kind == agent.AudioInput {
			intent.InputCause = &taskCause{SSRC: cause.Input.Progress.SSRC, Index: cause.Input.Progress.Index}
		}
		if spec.Timeout > 0 {
			intent.Deadline = now.Add(spec.Timeout)
		}
		if spec.Class == agent.SideEffecting {
			intent.Request = spec.Request
		}
		t := &localTask{intent: intent, request: spec.Request}
		t.terminalBytes = taskTerminalBytes(intent)
		next = append(next, t)
		guard := slices.Concat(refusals, anticipated)
		for _, r := range refused {
			guard = append(guard, taskRefusal{ID: r.TaskID, Turn: r.Turn})
		}
		if err := s.taskAdmissionBudget(before, next, before.MoveOnlyLength, guard, projection...); err != nil {
			if errors.Is(err, agent.ErrCapacity) || errors.Is(err, ErrMoveBudget) {
				next = next[:len(next)-1]
				refused = append(refused, capacity)
				continue
			}
			return nil, nil, err
		}
		t.request = slices.Clone(t.request)
		t.intent.Request = slices.Clone(t.intent.Request)
	}
	return next, refused, nil
}

// uniqueTaskRefusals repairs old checkpoints without changing the first result.
func uniqueTaskRefusals(in []taskRefusal) []taskRefusal {
	if len(in) == 0 {
		return nil
	}
	out := make([]taskRefusal, 0, len(in))
	seen := map[agent.TaskID]bool{}
	for _, r := range in {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	return out
}

func (s *session) fitTaskRefusals(before v2Envelope, _, tasks []*localTask, prior, refusals []taskRefusal, projection ...playbackState) ([]*localTask, []taskRefusal, error) {
	intents := make([]taskIntent, len(tasks))
	for i, t := range tasks {
		intents[i] = t.intent
	}
	if err := s.taskMetadataBudget(before, intents, refusals, true, projection...); err == nil {
		return tasks, refusals, nil
	}
	old := map[agent.TaskID]bool{}
	for _, r := range prior {
		old[r.ID] = true
	}
	var fresh []taskRefusal
	for _, r := range refusals {
		if !old[r.ID] {
			fresh = append(fresh, r)
		}
	}
	if err := s.taskMetadataBudget(before, nil, fresh, false, projection...); err != nil {
		// Retained Control may delay this transaction, but must never turn a
		// representable refusal-only callback into a terminal capacity failure.
		// Preserve prior state and cursors; the actor retries after backpressure.
		if before.ControlHigh > 0 && s.taskMetadataBudgetMode(before, nil, fresh, false, true, projection...) == nil {
			return nil, nil, errControlReceiptBackpressure
		}
		return nil, nil, fmt.Errorf("%w: callback task refusals exceed host metadata", agent.ErrCapacity)
	}
	return nil, nil, errTaskAdmissionBackpressure
}

func (s *session) acceptTaskEffects(tasks []*localTask, effects agent.Effects, before v2Envelope) []taskIntent {
	h := s.agent.v2
	h.tasks = tasks
	floor := before.CancelFloor
	if effects.CancelBefore != nil {
		floor = max(floor, *effects.CancelBefore)
	}
	for _, t := range h.tasks {
		if t.intent.Turn < floor && (!t.intent.Final || len(t.results) > 0) && t.intent.Phase != taskCanceled {
			s.cancelTask(t)
		}
	}
	for _, id := range effects.CancelTasks {
		for _, t := range h.tasks {
			if t.intent.ID == id && !t.intent.Final {
				s.cancelTask(t)
			}
		}
	}
	s.publishTaskIntents(&before)
	return before.Tasks
}

func (s *session) cancelTask(t *localTask) {
	if t.cancel != nil {
		t.cancel()
	}
	t.attempt++ // fence all parked and late results from this attempt
	t.launched = false
	t.prepared = false
	s.mu.Lock()
	for _, r := range t.results {
		delete(s.agent.v2.playback.payloads, audioKey{r.TaskID, r.Sequence})
	}
	s.mu.Unlock()
	t.results = nil
	t.mailboxBytes = 0
	for _, r := range t.audio {
		t.mailboxBytes += taskRetainedAudioBytes(r)
	}
	t.intent.Published = t.intent.Cursor
	t.intent.Phase = taskCanceled
	s.terminalTask(t, agent.Canceled, "")
}
