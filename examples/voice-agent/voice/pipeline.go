package voice

import (
	"encoding/json"
	"strconv"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
)

func (a *Agent) segmentTurn(index int) agent.Turn { return a.state.Turn + agent.Turn(index+1) }

// Separate segment turns let PlaybackFinished report actual local send completion
// while the next speech task is still running. Barge-in cancels the entire
// reserved turn range. There is one playing request and at most one render.
func (a *Agent) render(index int) agent.TaskSpec {
	a.state.Render, a.state.RenderTask, a.state.RenderCursor, a.state.RenderReady = index, "", 0, false
	request, _ := json.Marshal(speechRequest{Text: Segments(a.state.Reply)[index], Bytes: min(MaxSegmentBytes, MaxSpeechBytes-a.state.SpeechBytes), Nanos: min(8*time.Second, SpeechTime-a.state.SpeechNanos)})
	return agent.TaskSpec{Step: "speech-" + strconv.Itoa(index), Kind: SpeechKind, Turn: a.segmentTurn(index), Class: agent.ReadOnly, Resume: agent.Retry, Timeout: 45 * time.Second, Request: request}
}

func (a *Agent) play() agent.Effects {
	index := a.state.Segment
	a.state.Phase, a.state.PlayTask, a.state.PlayCursor = "playing", "", 0
	effects := agent.Effects{StartTasks: []agent.TaskSpec{{Step: "play-" + strconv.Itoa(index), Kind: PlayKind, Turn: a.segmentTurn(index), Class: agent.ReadOnly, Resume: agent.Retry, Timeout: 45 * time.Second, Request: a.buffer}}}
	// Acceptance transfers these exact bytes to the playback request. Keep the
	// immutable buffer for rollback until the callback transaction is accepted.
	a.length = 0
	if index+1 < len(Segments(a.state.Reply)) {
		effects.StartTasks = append(effects.StartTasks, a.render(index+1))
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
		if r.Outcome != agent.OK {
			return a.reset(r.Outcome, safeCode(r.Detail), "The turn did not complete ("+string(r.Outcome)+"). Hold to talk again.")
		}
		if rendering && !playing {
			if a.length == 0 {
				return a.reset(agent.Failed, "empty_speech", "The speech provider returned no audio. Hold to talk again.")
			}
			a.state.RenderReady = true
			if a.state.Phase == "synthesizing" {
				return a.play()
			}
		}
		return agent.Effects{}
	}
	if playing {
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
		return a.reset(agent.Failed, "invalid_speech", "The speech provider exceeded the turn's audio limit. Hold to talk again.")
	}
	a.state.SpeechBytes += len(r.Chunk.Data)
	a.state.SpeechNanos += span
	a.buffer = append([]byte(nil), r.Chunk.Data...)
	a.length = len(a.buffer)
	return agent.Effects{}
}
