package mediaworker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
)

func validateV2Effects(kind agent.EventKind, e agent.Effects) error {
	if kind == agent.Resumed && len(e.StartTasks) != 0 {
		return errors.New("agent: StartTasks from Resumed is forbidden")
	}

	return nil
}

// acceptedEvents owns the complete local event transaction. The actor serializes
// submit; adoption submits before readiness. Its observations are the published
// pair, receipts, export and playback, not the staging implementation.
//
// Handle/Save and admission stay off the packet lock and share the existing
// deadline. A late callback settles before rollback. Commit retains the live
// pacer cursor and uses the existing lock acquisitions; local acceptance is not
// checkpoint acknowledgement. No stage or mutable bookkeeping escapes submit.
type acceptedEvents struct{ session *session }
type eventMode bool

const (
	eventLive     eventMode = false
	eventAdoption eventMode = true
)

func (m acceptedEvents) submit(event agent.Event, mode eventMode) (agent.ControlReceipt, error) {
	if event.Kind == agent.Control {
		if event.Control == nil {
			return agent.ControlReceipt{}, agent.ErrControlInvalid
		}
		return m.control(*event.Control, mode)
	}
	return agent.ControlReceipt{}, m.accept(event, mode, nil)
}

func (m acceptedEvents) accept(event agent.Event, mode eventMode, control *v2Envelope) error {
	s := m.session
	adoption := mode == eventAdoption

	// Future kinds have no delivery route. Do not call
	// the agent or Save, so an unknown event cannot alter accepted state.
	switch event.Kind {
	case agent.Started, agent.AudioInput, agent.Resumed, agent.TaskMessage, agent.TimerFired, agent.PlaybackFinished, agent.Control:
	default:
		return nil
	}
	if event.Kind == agent.Control {
		if event.Control == nil {
			return agent.ErrControlInvalid
		}
		owned := *event.Control
		owned.Data = slices.Clone(owned.Data)
		event.Control = &owned
	}
	h := s.agent
	if !adoption && len(h.v2.receiptRetries) > 0 && s.receiptEventThrottled(event) {
		return errControlReceiptBackpressure
	}

	event.At = s.taskNow()
	before := cloneAgentState(h.latest)
	if control != nil {
		before.V2.Controls, before.V2.ControlFloor = control.Controls, control.ControlFloor
	}
	// The actor can stop attempts as soon as session cancellation releases
	// agentCall, even if its callback goroutine is still running. Copy task
	// nodes before dispatch so its staging never copies live attempt bookkeeping.
	// Payloads are immutable here; only their headers and task fields need copies.
	base := slices.Clone(h.v2.tasks)
	for i, t := range base {
		node := *t
		base[i] = &node
	}
	var saved agent.State
	var effects agent.Effects
	var tasks []*localTask
	var saving atomic.Bool
	var handleFailed atomic.Bool
	var refusals []taskRefusal
	var timers []hostTimer
	var timerGeneration uint64
	var candidate agentState
	err := s.agentCall(func(ctx context.Context) error {
		var err error
		s.worker.metrics.agentEvents.Add(1)
		callbackEvent := event
		if event.Control != nil {
			message := *event.Control
			message.Data = slices.Clone(message.Data)
			callbackEvent.Control = &message
		}
		effects, err = h.v2.instance.Handle(ctx, callbackEvent)
		if err != nil {
			handleFailed.Store(true)
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateV2Effects(event.Kind, effects); err != nil {
			return err
		}
		effects = filterAudioEffects(effects, before.V2.CancelFloor)
		timers, timerGeneration, err = prepareTimers(before.V2.Timers, before.V2.TimerGeneration, event, effects, s.taskNow())
		if err != nil {
			return err
		}
		if err = s.validateAudioEffects(effects, event, base); err != nil {
			return err
		}
		s.mu.Lock()
		projection := s.projectedPlaybackLocked(effects, event).snapshot()
		s.mu.Unlock()
		// Refusals are compact terminal intents, delivered as later transactions.
		// Never recursively call Handle while accepting another callback's effects.
		refusals = uniqueTaskRefusals(before.V2.Capacity)
		if event.Kind == agent.TaskMessage && len(refusals) > 0 && refusals[0].ID == event.Result.TaskID {
			refusals = refusals[1:]
		}
		var refused []agent.TaskResult
		tasks, refused, err = s.prepareTaskEffects(*before.V2, event, effects, base, refusals, projection)
		if err != nil {
			return err
		}
		for _, r := range refused {
			refusals = append(refusals, taskRefusal{ID: r.TaskID, Turn: r.Turn})
		}
		// Stage consumption on an owned node before checking the complete metadata.
		// The accepted cursor must not make a previously checked envelope overflow.
		if event.Kind == agent.TaskMessage {
			for i, t := range tasks {
				if t.intent.ID == event.Result.TaskID {
					next := *t
					next.intent.Cursor = event.Result.Sequence
					if t.results[0].Chunk.Audio != nil {
						next.audio = append(slices.Clone(t.audio), t.results[0])
						next.mailboxBytes -= taskAudioRefBytes(t.results[0])
					} else {
						next.mailboxBytes -= taskResultBytes(t.results[0])
					}
					next.results = t.results[1:]
					s.retireTaskRequest(&next)
					tasks[i] = &next
				}
			}
		}
		prior := uniqueTaskRefusals(before.V2.Capacity)
		if event.Kind == agent.TaskMessage && len(prior) > 0 && prior[0].ID == event.Result.TaskID {
			prior = prior[1:]
		}
		tasks, refusals, err = s.fitTaskRefusals(*before.V2, base, tasks, prior, refusals, projection)
		if err != nil {
			return err
		}
		saving.Store(true)
		saved, err = s.measuredSave(ctx, h.v2.instance)
		if err != nil {
			return fmt.Errorf("%w: %w", legacy.ErrSave, err)
		}
		if err := s.validateV2StateMetadata(saved); err != nil {
			if errors.Is(err, agent.ErrStateTooLarge) || errors.Is(err, agent.ErrHistoryTooLarge) {
				return fmt.Errorf("%w: %w", errSnapshotAdmissionCapacity, err)
			}
			return err
		}
		if err := s.hostEffectsBudget(*before.V2, effects, event, timers, tasks, refusals, saved.MoveOnlyLength, timerGeneration); err != nil {
			return err
		}
		candidate = cloneAgentState(before)
		candidate.State = legacy.State{Version: saved.Version, Bytes: saved.Bytes}
		candidate.History = saved.History
		candidate.V2.HasMoveOnly, candidate.V2.MoveOnlyLength = saved.HasMoveOnly, saved.MoveOnlyLength
		candidate.V2.Timers, candidate.V2.TimerGeneration = timers, timerGeneration
		candidate.V2.Playback = projection
		candidate.V2.Capacity = refusals
		candidate.V2.Tasks = make([]taskIntent, len(tasks))
		for i, t := range tasks {
			candidate.V2.Tasks[i] = t.intent
		}
		if effects.CancelBefore != nil {
			candidate.V2.CancelFloor = max(candidate.V2.CancelFloor, *effects.CancelBefore)
		}
		if event.Kind == agent.Started {
			candidate.V2.Started = true
		}
		if event.Kind == agent.AudioInput {
			candidate.Progress = event.Input.Progress
			candidate.Progress.Consumed = before.Progress.Consumed + 1
		}
		stageControl(candidate.V2, event)
		// Reserve future task terminal fields as well as the immediate effects.
		// Compaction remains local until the whole callback transaction commits.
		if len(candidate.V2.Controls) > 0 {
			guarded := taskMetadataGuard(*candidate.V2)
			s.compactControlMetadata(&guarded, min(h.v2.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes))
			candidate.V2.Controls, candidate.V2.ControlFloor = guarded.Controls, guarded.ControlFloor
		}
		candidate.Revision++
		_, err = s.checkpoint.admit(checkpointAdmission{candidate: candidate, prior: before, event: true, sideEffect: slices.ContainsFunc(effects.StartTasks, func(t agent.TaskSpec) bool { return t.Class == agent.SideEffecting })})
		if err != nil {
			return err
		}
		saved = cloneV2State(saved)
		candidate.State = legacy.State{Version: saved.Version, Bytes: saved.Bytes}
		candidate.History = saved.History
		return nil
	}, !adoption)
	if err != nil {
		if adoption && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return err
		}
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		if restoreErr := s.agentCall(func(ctx context.Context) error { return h.v2.instance.Restore(ctx, cloneV2State(v2State(before))) }); restoreErr != nil {
			return fmt.Errorf("%w: %w", legacy.ErrRestore, restoreErr)
		}
		s.checkpoint.rollback(before)
		if reserveErr := s.taskBudgetWithRefusals(*before.V2, h.v2.tasks, before.V2.MoveOnlyLength, before.V2.Capacity); reserveErr != nil {
			return reserveErr
		}
		if event.Kind == agent.Control && errors.Is(err, errSnapshotAdmissionCapacity) {
			return agent.ErrCapacity
		}
		if errors.Is(err, errSnapshotAdmissionCapacity) && !adoption {
			return s.refuseSnapshotEvent(event, before)
		}
		// Control has no consumed input cursor. Quarantine a failed Handle and
		// restore accepted state before refusing without advancing its sequence.
		// Save/Restore failures and invalid effects remain terminal.
		if event.Kind == agent.Control && !saving.Load() && (handleFailed.Load() || errors.Is(err, context.DeadlineExceeded)) {
			return fmt.Errorf("%w: %v", agent.ErrControlRejected, err)
		}
		// A late AudioInput Handle is quarantined before Restore, just as in the
		// legacy host. It cannot advance accepted state or consumed-input progress.
		// Capacity TaskMessages get their own transaction and fail closed too.
		if !adoption && event.Kind == agent.AudioInput && errors.Is(err, context.DeadlineExceeded) && !saving.Load() {
			s.worker.agents.drops.Add(1)
			return nil
		}
		// Save deadlines, other callback failures, invalid state and rejected effects
		// remain terminal. No later event may publish a rejected callback's changes.
		if errors.Is(err, context.DeadlineExceeded) && saving.Load() {
			return fmt.Errorf("%w: %w", legacy.ErrSave, err)
		}
		if !adoption && errors.Is(err, errControlReceiptBackpressure) {
			s.throttleReceiptEvent(event)
		}
		return err
	}
	// Finalize the same admitted candidate. Playback is intentionally projected
	// from the live runtime here: the pacer may have sent packets during Save.
	// Cancellation removes only unconsumed payloads, under its original locks,
	// inside this commit; it cannot invalidate a queued playback reference.
	intents := s.acceptTaskEffects(tasks, effects, *before.V2)
	s.mu.Lock()
	h.v2.playback = s.projectedPlaybackLocked(effects, event)
	candidate.V2.Playback = s.playbackSnapshotLocked()
	candidate.V2.Tasks = cloneTaskIntents(intents)
	h.latest = candidate
	if !s.fenced.Load() && s.ctx.Err() == nil {
		s.state.Agent = cloneAgentState(candidate)
	}
	s.mu.Unlock()
	s.wakePacer()
	if !agentStateEqual(before, candidate) {
		s.checkpoint.request()
	}
	return nil
}
