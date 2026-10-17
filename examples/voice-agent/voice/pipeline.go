package voice

import (
	"encoding/json"
	"strconv"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

const (
	retryTimer      = "voice/capacity-retry/v1"
	capacityTimer   = "voice/capacity-deadline/v1"
	restartTimer    = "voice/restart-notice/v1"
	capacityWaiting = "Waiting for task capacity; the reply will continue when the store recovers."
)

func (a *Agent) segmentTurn(index int) agent.Turn { return a.state.Turn + agent.Turn(index+1) }
func (a *Agent) render(index int) agent.TaskSpec {
	a.state.Render, a.state.RenderTask, a.state.RenderCursor, a.state.RenderReady = index, "", 0, false
	request, _ := json.Marshal(speechRequest{Text: Segments(a.state.Reply)[index], Bytes: min(MaxSegmentBytes, MaxSpeechBytes-a.state.SpeechBytes), Nanos: min(8*time.Second, SpeechTime-a.state.SpeechNanos)})
	return agent.TaskSpec{Step: "speech-" + strconv.Itoa(index), Kind: SpeechKind, Turn: a.segmentTurn(index), Class: agent.ReadOnly, Resume: agent.Retry, Timeout: 45 * time.Second, Request: request}
}
func (a *Agent) playSpec() agent.TaskSpec {
	a.state.PlayTask, a.state.PlayCursor = "", 0
	return agent.TaskSpec{Step: "play-" + strconv.Itoa(a.state.Segment), Kind: PlayKind, Turn: a.segmentTurn(a.state.Segment), Class: agent.ReadOnly, Resume: agent.Retry, Timeout: 45 * time.Second, Request: a.playBuffer}
}
func (a *Agent) canRender() bool {
	return a.state.SpeechBytes < MaxSpeechBytes-1 && a.state.SpeechNanos < SpeechTime
}
func (a *Agent) truncated() {
	a.state.StopAfterPlayback = true
	a.state.Outcome, a.state.Error, a.state.Notice = agent.OK, "", "Reply truncated at its speech limit. Hold to talk for more."
}
func (a *Agent) play() agent.Effects {
	a.state.Phase = "playing"
	// Keep these accepted bytes until Play produces its first audio chunk. A
	// refused Play request can then retry without synthesizing or changing bytes.
	a.playBuffer, a.state.PlayLength = a.buffer, a.length
	a.length = 0
	effects := agent.Effects{StartTasks: []agent.TaskSpec{a.playSpec()}}
	if a.state.Segment+1 < len(Segments(a.state.Reply)) {
		if a.canRender() {
			effects.StartTasks = append(effects.StartTasks, a.render(a.state.Segment+1))
		} else {
			a.truncated()
		}
	}
	return effects
}

// A later task's error never clears audio already requested for playback.
func (a *Agent) failAfterPlayback(outcome agent.Outcome, code, notice string) agent.Effects {
	if a.state.Phase != "playing" {
		return a.reset(outcome, code, notice)
	}
	a.state.StopAfterPlayback = true
	a.state.Outcome, a.state.Error, a.state.Notice = outcome, code, notice
	a.state.RetryRender, a.state.RenderRetryInFlight = false, false
	return agent.Effects{}
}
func (a *Agent) capacityExpired() agent.Effects {
	if !a.state.RetryPlay && !a.state.RetryRender {
		return agent.Effects{}
	}
	notice := "The reply could not continue after 30 s of task-capacity or store outage. Hold to talk again."
	if a.state.Phase == "playing" && a.state.PlayLength == 0 {
		return a.failAfterPlayback(agent.Capacity, "capacity", notice)
	}
	return a.reset(agent.Capacity, "capacity", notice)
}
func (a *Agent) refused(play bool) agent.Effects {
	if play {
		a.state.RetryPlay, a.state.PlayRetryInFlight = true, false
	} else {
		a.state.RetryRender, a.state.RenderRetryInFlight = true, false
	}
	if a.state.CapacityExpired {
		return a.capacityExpired()
	}
	effects := agent.Effects{SetTimers: []agent.TimerSpec{{ID: retryTimer, After: 500 * time.Millisecond}}}
	if !a.state.CapacityWindow {
		a.state.CapacityWindow = true
		effects.SetTimers = append(effects.SetTimers, agent.TimerSpec{ID: capacityTimer, After: 30 * time.Second})
	}
	if !a.state.StopAfterPlayback {
		a.state.Notice = capacityWaiting
	}
	return effects
}
func (a *Agent) clearCapacityNotice() {
	if !a.state.RetryPlay && !a.state.RetryRender && a.state.Notice == capacityWaiting {
		a.state.Notice = ""
	}
}
func (a *Agent) retryPending() agent.Effects {
	if a.state.CapacityExpired {
		return a.capacityExpired()
	}
	var effects agent.Effects
	if a.state.RetryPlay && !a.state.PlayRetryInFlight {
		a.state.PlayRetryInFlight = true
		effects.StartTasks = append(effects.StartTasks, a.playSpec())
	}
	if a.state.RetryRender && !a.state.RenderRetryInFlight && !a.state.StopAfterPlayback {
		a.state.RenderRetryInFlight = true
		effects.StartTasks = append(effects.StartTasks, a.render(a.state.Render))
	}
	return effects
}
func (a *Agent) speechResult(r *agent.TaskResult) agent.Effects {
	index := int(r.Turn-a.state.Turn) - 1
	rendering := index == a.state.Render && !a.state.RenderReady
	playing := index == a.state.Segment && a.state.Phase == "playing"
	if !rendering && !playing {
		return agent.Effects{}
	}
	task, cursor := &a.state.RenderTask, &a.state.RenderCursor
	if playing {
		task, cursor = &a.state.PlayTask, &a.state.PlayCursor
	}
	if (*task != "" && *task != r.TaskID) || r.Sequence <= *cursor {
		return agent.Effects{}
	}
	*task, *cursor = r.TaskID, r.Sequence
	if r.Final {
		if r.Outcome == agent.Capacity {
			if playing && a.state.PlayLength > 0 {
				return a.refused(true)
			}
			if rendering && !playing && a.length == 0 {
				return a.refused(false)
			}
			// Speech's sole Data result is already a complete accepted segment.
			// A refused terminal must not trigger another synthesis of those bytes.
			if !rendering || playing || a.length == 0 {
				return a.failAfterPlayback(r.Outcome, "capacity", "Playback did not complete. Hold to talk again.")
			}
		} else if r.Outcome != agent.OK {
			if r.Detail == "speech_limit" {
				a.truncated()
				if a.state.Phase != "playing" {
					return a.reset(agent.OK, "", a.state.Notice)
				}
				return agent.Effects{}
			}
			if playing && a.state.PlayLength > 0 {
				return a.reset(r.Outcome, safeCode(r.Detail), "Playback did not complete. Hold to talk again.")
			}
			return a.failAfterPlayback(r.Outcome, safeCode(r.Detail), "The turn did not complete ("+string(r.Outcome)+"). Hold to talk again.")
		}
		if rendering && !playing {
			if a.length == 0 {
				return a.failAfterPlayback(agent.Failed, "empty_speech", "The speech provider returned no audio. Hold to talk again.")
			}
			a.state.RetryRender, a.state.RenderRetryInFlight = false, false
			a.clearCapacityNotice()
			a.state.RenderReady = true
			if a.state.Phase == "synthesizing" {
				return a.play()
			}
		}
		return agent.Effects{}
	}
	if playing {
		a.state.RetryPlay, a.state.PlayRetryInFlight = false, false
		a.clearCapacityNotice()
		a.state.PlayLength = 0 // the host owns the complete accepted Play request now
		effects := agent.Effects{NotifyPlaybackFinished: &r.Turn}
		if r.Chunk.Audio != nil {
			for i := range r.Chunk.Audio.Packets {
				effects.Audio = append(effects.Audio, agent.Audio{Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence, PacketOffset: uint32(i)}})
			}
		}
		return effects
	}
	packets, err := decodePackets(r.Chunk.Data, MaxSegmentBytes)
	var span time.Duration
	for _, packet := range packets {
		d, _ := packetDuration(packet)
		span += d
	}
	if err != nil || span > 8*time.Second || a.state.SpeechBytes+len(r.Chunk.Data) > MaxSpeechBytes || a.state.SpeechNanos+span > SpeechTime {
		return a.failAfterPlayback(agent.Failed, "invalid_speech", "The speech provider exceeded the turn's audio limit. Hold to talk again.")
	}
	a.state.RetryRender, a.state.RenderRetryInFlight = false, false
	a.clearCapacityNotice()
	a.state.SpeechBytes += len(r.Chunk.Data)
	a.state.SpeechNanos += span
	a.buffer = append([]byte(nil), r.Chunk.Data...)
	a.length = len(a.buffer)
	return agent.Effects{}
}
