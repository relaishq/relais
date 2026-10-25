package mediaworker

import (
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

func prepareTimers(before []hostTimer, generation uint64, event agent.Event, effects agent.Effects, now time.Time) ([]hostTimer, uint64, error) {
	// Older experimental checkpoints stored per-ID tombstones. Retain their
	// highest generation, then discard fired/cancelled entries at this boundary.
	timers := make([]hostTimer, 0, len(before))
	for _, timer := range before {
		generation = max(generation, timer.Generation)
		if timer.At.IsZero() {
			continue
		}
		if event.Kind == agent.TimerFired && timer.ID == event.Timer.ID && timer.Generation == event.Timer.Generation {
			continue
		}
		timers = append(timers, timer)
	}
	update := func(id string, after time.Duration, armed bool) error {
		if id == "" || len(id) > timerIDMaxBytes || !utf8.ValidString(id) || after < 0 {
			return fmt.Errorf("%w: invalid timer ID or duration", agent.ErrIncompatibleAgent)
		}
		if !armed {
			timers = slices.DeleteFunc(timers, func(t hostTimer) bool { return t.ID == id })
			return nil
		}
		if generation == ^uint64(0) {
			return fmt.Errorf("%w: timer generation exhausted", agent.ErrCapacity)
		}
		generation++
		timers = slices.DeleteFunc(timers, func(t hostTimer) bool { return t.ID == id })
		timers = append(timers, hostTimer{ID: id, Generation: generation, At: now.Add(after)})
		return nil
	}
	for _, id := range effects.CancelTimers {
		if err := update(id, 0, false); err != nil {
			return nil, generation, err
		}
	}
	for _, spec := range effects.SetTimers {
		if err := update(spec.ID, spec.After, true); err != nil {
			return nil, generation, err
		}
	}
	return timers, generation, nil
}

func filterAudioEffects(e agent.Effects, floor agent.Turn) agent.Effects {
	if e.CancelBefore != nil {
		floor = max(floor, *e.CancelBefore)
	}
	if e.ClearAudioBefore != nil {
		floor = max(floor, *e.ClearAudioBefore)
	}
	if len(e.Audio) > 0 {
		audio := make([]agent.Audio, 0, len(e.Audio))
		for _, ref := range e.Audio {
			if ref.Turn >= floor {
				audio = append(audio, ref)
			}
		}
		e.Audio = audio
	}
	return e
}

func (s *session) validateAudioEffects(e agent.Effects, event agent.Event, tasks []*localTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &s.agent.v2.playback
	if s.agent.v2.bridge != nil && len(e.Audio) > 0 {
		return fmt.Errorf("%w: bridge audio requires host ingress", agent.ErrIncompatibleAgent)
	}
	for _, ref := range e.Audio {
		if _, _, ok := p.packet(ref); !ok {
			return fmt.Errorf("%w: audio reference has no payload", agent.ErrIncompatibleAgent)
		}
		task := slices.IndexFunc(tasks, func(t *localTask) bool { return t.intent.ID == ref.Source.TaskID })
		if task < 0 || tasks[task].intent.Turn != ref.Turn {
			return fmt.Errorf("%w: audio reference turn mismatch", agent.ErrIncompatibleAgent)
		}
		if event.Kind != agent.TaskMessage || event.Result == nil || event.Result.Chunk.Audio == nil || ref.Source.TaskID != event.Result.TaskID || ref.Source.Sequence != event.Result.Sequence || ref.Turn != event.Result.Turn {
			return fmt.Errorf("%w: audio must reference the current task result", agent.ErrIncompatibleAgent)
		}
	}
	return nil
}

func (s *session) projectedPlaybackLocked(e agent.Effects, event agent.Event) playbackRuntime {
	p := s.agent.v2.playback
	if s.agent.v2.bridge != nil {
		p.notify = slices.Clone(p.notify)
		if event.Kind == agent.PlaybackFinished {
			p.notify = slices.DeleteFunc(p.notify, func(t agent.Turn) bool { return t == event.Playback.Turn })
		}
		if e.NotifyPlaybackFinished != nil && !slices.Contains(p.notify, *e.NotifyPlaybackFinished) {
			p.notify = append(p.notify, *e.NotifyPlaybackFinished)
		}
		return p
	}
	if e.ClearAudioBefore != nil {
		p.queue = make([]agent.Audio, 0, len(p.queue))
		for _, ref := range s.agent.v2.playback.queue {
			if ref.Turn >= *e.ClearAudioBefore {
				p.queue = append(p.queue, ref)
			}
		}
	} else {
		p.queue = slices.Clone(p.queue)
	}
	p.queue = append(p.queue, e.Audio...)
	p.length = 0
	p.duration = 0
	p.referenceBytes = 0
	for _, ref := range p.queue {
		data, d, _ := p.packet(ref)
		p.length += len(data)
		p.duration += d
		p.referenceBytes += len(ref.Source.TaskID) + 32
	}
	p.notify = slices.Clone(p.notify)
	if event.Kind == agent.PlaybackFinished {
		p.notify = slices.DeleteFunc(p.notify, func(t agent.Turn) bool { return t == event.Playback.Turn })
	}
	if e.NotifyPlaybackFinished != nil && !slices.Contains(p.notify, *e.NotifyPlaybackFinished) {
		p.notify = append(p.notify, *e.NotifyPlaybackFinished)
	}
	return p
}

func (s *session) hostEffectsBudget(before v2Envelope, e agent.Effects, event agent.Event, timers []hostTimer, tasks []*localTask, refusals []taskRefusal, appBytes int, timerGeneration ...uint64) error {
	s.mu.Lock()
	p := s.projectedPlaybackLocked(e, event)
	envelope := before
	envelope.Playback = p.snapshot()
	queueBytes := audioRefsBytes(p.queue)
	s.mu.Unlock()
	stageControl(&envelope, event)
	envelope.Timers = timers
	if len(timerGeneration) > 0 {
		envelope.TimerGeneration = timerGeneration[0]
	} else {
		for _, timer := range timers {
			envelope.TimerGeneration = max(envelope.TimerGeneration, timer.Generation)
		}
	}
	envelope.Capacity = refusals
	if e.CancelBefore != nil {
		envelope.CancelFloor = max(envelope.CancelFloor, *e.CancelBefore)
	}
	envelope.Tasks = make([]taskIntent, len(tasks))
	for i, t := range tasks {
		envelope.Tasks[i] = t.intent
	}
	if _, err := s.checkpoint.admit(checkpointAdmission{host: &checkpointHostPreflight{envelope: envelope, effects: &e, event: event}}); err != nil {
		return err
	}
	if err := s.bridgeActorBudget(tasks, appBytes); err != nil {
		return err
	}
	n := int64(appBytes + queueBytes)
	var requestBytes int64
	for _, t := range tasks {
		requestBytes += int64(len(t.request))
		n += int64(2*len(t.request) + t.mailboxBytes)
		if !t.intent.Final {
			n += int64(t.terminalBytes)
		}
	}
	if n > int64(s.agent.v2.registration.Bounds.MaxMailboxBytes) {
		if len(e.Audio) > 0 {
			return errTaskAdmissionBackpressure
		}
		return agent.ErrCapacity
	}
	if err := s.reserveMoveBytes(n, requestBytes); err != nil {
		if len(e.Audio) > 0 {
			return errTaskAdmissionBackpressure
		}
		return err
	}
	return nil
}
