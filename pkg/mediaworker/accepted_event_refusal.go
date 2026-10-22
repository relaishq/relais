package mediaworker

import (
	"slices"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

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
	if err := s.taskMetadataBudget(*s.agent.latest.V2, next.V2.Tasks, next.V2.Capacity, false); err != nil {
		return err
	}
	s.compactControlMetadata(next.V2, min(h.registration.Bounds.MaxPendingBytes, v2HostMetadataBytes))
	next.Revision++
	if _, err := s.checkpoint.admit(checkpointAdmission{candidate: next, prior: s.agent.latest}); err != nil {
		return err
	}
	s.mu.Lock()
	h.playback.notify = slices.Clone(next.V2.Playback.Notify)
	s.mu.Unlock()
	s.agent.latest = next
	s.publishAgent()
	s.checkpoint.request()
	return nil
}
