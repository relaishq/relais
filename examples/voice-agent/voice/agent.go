// Package voice is a bounded push-to-talk tutorial. Audio exists only in move
// memory. Four ReadOnly stages separate paid work from deterministic playback.
package voice

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/workerapp"
)

const (
	Name              = "voice-agent/v1"
	TranscribeKind    = "voice/stt/v1"
	ReplyKind         = "voice/reply/v1"
	SpeechKind        = "voice/speech/v1"
	PlayKind          = "voice/play/v1"
	MaxStateBytes     = 8 << 10
	MaxRecordingBytes = 64 << 10
	MaxSpeechBytes    = 128 << 10
	RecordTime        = 8 * time.Second
	SpeechTime        = 20 * time.Second
	recordTimer       = "voice/record/v1"
	stateVersion      = 1
)

type Exchange struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}

// State is the complete durable schema. It contains no provider credentials,
// audio, provider response IDs, or host notices. Text limits count JSON bytes.
type State struct {
	Turn       agent.Turn    `json:"turn"`
	Phase      string        `json:"phase"`
	Transcript string        `json:"transcript,omitempty"`
	Reply      string        `json:"reply,omitempty"`
	History    []Exchange    `json:"history,omitempty"`
	Outcome    agent.Outcome `json:"outcome,omitempty"`
	Error      string        `json:"error,omitempty"`
	Notice     string        `json:"notice,omitempty"`
	Cursor     uint64        `json:"cursor,omitempty"`
	Task       agent.TaskID  `json:"task,omitempty"`
	Seen       Window        `json:"seen"`
}

type Agent struct {
	state     State
	available bool
	buffer    []byte // immutable between callback transactions; recording or speech
	rollback  []byte
	length    int // Save reads this O(1) metadata, never the payload
	limit     int
}

func Registration(provider Provider) workerapp.Registration {
	return workerapp.Registration{Name: Name, Factory: func(args agent.FactoryArgs) agent.Agent {
		limit := args.MaxStateBytes
		if limit == 0 {
			limit = MaxStateBytes
		}
		return &Agent{state: State{Phase: "idle"}, available: provider != nil && provider.Available(), limit: limit}
	}, Tasks: handlers(provider), Limits: agent.Limits{MaxStateBytes: MaxStateBytes}}
}
func (a *Agent) reset(outcome agent.Outcome, code, notice string) agent.Effects {
	a.state.Phase, a.state.Outcome, a.state.Error, a.state.Notice = "idle", outcome, code, notice
	a.state.Task, a.state.Cursor = "", 0
	a.buffer, a.length = nil, 0
	floor := a.state.Turn + 1
	return agent.Effects{CancelBefore: &floor, ClearAudioBefore: &floor, CancelTimers: []string{recordTimer}}
}
func (a *Agent) task(kind, phase string, request []byte) agent.Effects {
	a.state.Phase, a.state.Cursor, a.state.Task = phase, 0, ""
	a.length = 0
	// Keep the immutable app buffer until acceptance. Restore can roll back a
	// refused callback without losing the bytes of the prior recording/speech.
	return agent.Effects{CancelTimers: []string{recordTimer}, StartTasks: []agent.TaskSpec{{Step: phase, Kind: kind, Turn: a.state.Turn, Class: agent.ReadOnly, Resume: agent.Retry, Timeout: 45 * time.Second, Request: request}}}
}
func (a *Agent) finish() agent.Effects {
	if a.length <= 1 {
		return a.reset(agent.Failed, "empty_recording", "No speech was recorded. Hold to talk again.")
	}
	return a.task(TranscribeKind, "transcribing", a.buffer)
}
func (a *Agent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if err := ctx.Err(); err != nil {
		return agent.Effects{}, err
	}
	a.rollback = a.buffer
	if a.length == 0 {
		a.buffer = nil
	}
	switch e.Kind {
	case agent.Control:
		if e.Control == nil {
			break
		}
		switch e.Control.Name {
		case "ptt.start":
			if a.state.Turn >= agent.Turn(math.MaxUint64-2) {
				return a.reset(agent.Capacity, "turn_limit", "Start a new call."), nil
			}
			a.state.Turn++
			effects := a.reset(agent.OK, "", "")
			floor := a.state.Turn
			effects.CancelBefore, effects.ClearAudioBefore = &floor, &floor
			if !a.available {
				return a.reset(agent.Failed, "provider_unavailable", "OPENAI_API_KEY is not configured on the worker. No turn was sent."), nil
			}
			a.state.Phase, a.state.Transcript, a.state.Reply = "recording", "", ""
			effects.SetTimers = []agent.TimerSpec{{ID: recordTimer, After: RecordTime}}
			return effects, nil
		case "ptt.stop":
			if a.state.Phase == "recording" {
				return a.finish(), nil
			}
		}
	case agent.AudioInput:
		if e.Input == nil || !a.state.Seen.Accept(e.Input.Progress.SSRC, e.Input.Progress.Index) || a.state.Phase != "recording" {
			break
		}
		p := e.Input.Payload
		if _, err := packetDuration(p); err != nil || len(p) > 1200 {
			return a.reset(agent.Failed, "invalid_audio", "The recording contained invalid Opus. Hold to talk again."), nil
		}
		if a.length+len(p)+2 > MaxRecordingBytes {
			a.state.Notice = "Recording reached its byte limit; the last packet was not recorded."
			return a.finish(), nil
		}
		if a.length == 0 {
			a.buffer = []byte{1}
		}
		// Append only. The rollback slice still names the unchanged prefix.
		a.buffer = binary.BigEndian.AppendUint16(a.buffer, uint16(len(p)))
		a.buffer = append(a.buffer, p...)
		a.length = len(a.buffer)
		if a.length >= MaxRecordingBytes-1202 {
			a.state.Notice = "Recording reached its byte limit."
			return a.finish(), nil
		}
	case agent.TimerFired:
		if e.Timer != nil && e.Timer.ID == recordTimer && a.state.Phase == "recording" {
			a.state.Notice = "Recording stopped at its time limit."
			return a.finish(), nil
		}
	case agent.Resumed:
		if e.Resume != nil && (e.Resume.MoveOnly == agent.OutcomeUnknown || string(e.Resume.Kind) == "takeover") {
			return a.reset(agent.OutcomeUnknown, "turn_interrupted", "The worker stopped. Uncheckpointed speech or audio may be lost; saved text is shown below. Hold to talk for a new turn."), nil
		}
	case agent.TaskMessage:
		r := e.Result
		if r == nil {
			break
		}
		if r.Turn == 0 && r.Outcome == agent.Capacity {
			return a.reset(agent.Capacity, "capacity", "Input or work was refused by a resource limit. Hold to talk again."), nil
		}
		if r.Turn != a.state.Turn || a.state.Phase == "idle" || a.state.Phase == "recording" || r.Sequence <= a.state.Cursor {
			break
		}
		if a.state.Task != "" && a.state.Task != r.TaskID {
			break
		}
		a.state.Task, a.state.Cursor = r.TaskID, r.Sequence
		if r.Final {
			if r.Outcome != agent.OK {
				return a.reset(r.Outcome, safeCode(r.Detail), "The turn did not complete ("+string(r.Outcome)+"). Hold to talk again."), nil
			}
			switch a.state.Phase {
			case "transcribing":
				if a.state.Transcript == "" {
					return a.reset(agent.Failed, "empty_transcript", "No words were recognized. Hold to talk again."), nil
				}
				b, _ := json.Marshal(replyRequest{Transcript: a.state.Transcript, History: a.state.History})
				return a.task(ReplyKind, "replying", b), nil
			case "replying":
				if a.state.Reply == "" {
					return a.reset(agent.Failed, "empty_reply", "No reply was returned. Hold to talk again."), nil
				}
				a.state.History = append(a.state.History, Exchange{boundedText(a.state.Transcript, 512), boundedText(a.state.Reply, 512)})
				if len(a.state.History) > 4 {
					a.state.History = append([]Exchange(nil), a.state.History[len(a.state.History)-4:]...)
				}
				return a.task(SpeechKind, "synthesizing", []byte(a.state.Reply)), nil
			case "synthesizing":
				if a.length == 0 {
					return a.reset(agent.Failed, "empty_speech", "The speech provider returned no audio. Hold to talk again."), nil
				}
				return a.task(PlayKind, "playing", a.buffer), nil
			}
			break
		}
		switch a.state.Phase {
		case "transcribing":
			a.state.Transcript = boundedText(string(r.Chunk.Data), 1024)
		case "replying":
			a.state.Reply = boundedText(string(r.Chunk.Data), 1024)
		case "synthesizing":
			if _, err := decodePackets(r.Chunk.Data, MaxSpeechBytes); err != nil {
				return a.reset(agent.Failed, "invalid_speech", "The speech provider returned invalid audio."), nil
			}
			a.buffer = append([]byte(nil), r.Chunk.Data...)
			a.length = len(a.buffer)
		case "playing":
			effects := agent.Effects{}
			if r.Chunk.Audio != nil {
				turn := a.state.Turn
				effects.NotifyPlaybackFinished = &turn
				for i := range r.Chunk.Audio.Packets {
					effects.Audio = append(effects.Audio, agent.Audio{Turn: turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence, PacketOffset: uint32(i)}})
				}
			}
			return effects, nil
		}
	case agent.PlaybackFinished:
		if e.Playback != nil && e.Playback.Turn == a.state.Turn && a.state.Phase == "playing" {
			return a.reset(agent.OK, "", a.state.Notice), nil
		}
	}
	return agent.Effects{}, nil
}
func (a *Agent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a.state)
	if len(b) > a.limit {
		return agent.State{}, agent.ErrStateTooLarge
	}
	return agent.State{Version: stateVersion, Bytes: b, HasMoveOnly: a.length > 0, MoveOnlyLength: a.length}, err
}
func (a *Agent) Restore(_ context.Context, s agent.State) error {
	if s.Version != stateVersion {
		return agent.ErrVersion
	}
	if len(s.Bytes) > a.limit || !utf8.Valid(s.Bytes) {
		return agent.ErrStateTooLarge
	}
	var state State
	if err := json.Unmarshal(s.Bytes, &state); err != nil {
		return err
	}
	switch state.Phase {
	case "idle", "recording", "transcribing", "replying", "synthesizing", "playing":
	default:
		return errors.New("voice: invalid phase")
	}
	if (state.Phase == "recording" && s.MoveOnlyLength > MaxRecordingBytes) || (s.HasMoveOnly && state.Phase != "recording" && state.Phase != "synthesizing") {
		return errors.New("voice: invalid move phase/size")
	}
	if state.Turn >= agent.Turn(math.MaxUint64-1) || len(state.History) > 4 || s.MoveOnlyLength < 0 || s.MoveOnlyLength > MaxSpeechBytes || s.HasMoveOnly != (s.MoveOnlyLength > 0) {
		return errors.New("voice: invalid state")
	}
	a.state, a.length = state, s.MoveOnlyLength
	// Preserve a matching immutable buffer on callback rollback; a fresh crash
	// instance has no buffer and clears its lost length during Resumed.
	if len(a.rollback) == a.length {
		a.buffer = a.rollback
	} else if len(a.buffer) != a.length {
		a.buffer = nil
	}
	return nil
}
func (a *Agent) SaveMoveOnly(context.Context) ([]byte, error) {
	if len(a.buffer) != a.length {
		return nil, errors.New("voice: missing move audio")
	}
	return a.buffer, nil
}
func (a *Agent) RestoreMoveOnly(_ context.Context, b []byte) error {
	if len(b) != a.length {
		return errors.New("voice: move length mismatch")
	}
	if _, err := decodePackets(b, MaxSpeechBytes); err != nil {
		return err
	}
	a.buffer = append([]byte(nil), b...)
	return nil
}

// boundedText truncates only at rune boundaries and bounds the JSON encoding,
// including expansion of quotes, control characters and HTML escapes.
func boundedText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if b, _ := json.Marshal(s); len(b) <= n+2 {
		return s
	}
	end := 0
	used := 0
	for i, r := range s {
		b, _ := json.Marshal(string(r))
		cost := len(b) - 2
		if used+cost > n {
			break
		}
		used += cost
		end = i + utf8.RuneLen(r)
	}
	return s[:end]
}

var _ agent.MoveOnlyState = (*Agent)(nil)
