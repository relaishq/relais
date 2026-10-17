// Package loopback is a keyless v2 agent. It keeps encoded audio only in move
// memory; Save describes that memory without reading, copying or hashing it.
package loopback

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/bits"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/workerapp"
)

const (
	Name              = "delayed-loopback/v1"
	TaskKind          = "delayed-loopback/play/v1"
	MaxRecordingBytes = 64 << 10
	RecordTime        = 2 * time.Second
	recordTimer       = "record/v1"
)

// State is bounded metadata, suitable for the default 1 KiB app budget.
// Cursor counts task chunks accepted by the app, not packets heard by a caller.
type State struct {
	Refused uint64              `json:"refused,omitempty"`
	Turn    agent.Turn          `json:"turn"`
	Phase   string              `json:"phase"`
	Cursor  uint64              `json:"cursor"`
	Packets uint32              `json:"packets"`
	Seen    Window              `json:"seen"`
	Outcome agent.Outcome       `json:"outcome,omitempty"`
	Resume  *agent.ResumeNotice `json:"resume,omitempty"`
}

// Window remembers sparse extended RTP indexes, including gaps and sequence
// wraps. Older indexes fall below a monotonic floor and cannot be recorded again.
type Window struct {
	SSRC uint32    `json:"ssrc"`
	High uint64    `json:"high"`
	Mask [2]uint64 `json:"mask"`
	Set  bool      `json:"set"`
}

func (w *Window) Accept(ssrc uint32, index uint64) bool {
	if !w.Set {
		*w = Window{SSRC: ssrc, High: index, Mask: [2]uint64{1, 0}, Set: true}
		return true
	}
	// One negotiated audio source per call. Do not mistake a different source
	// for a duplicate of the current one.
	if w.SSRC != ssrc {
		return false
	}
	if index > w.High {
		delta := index - w.High
		switch {
		case delta >= 128:
			w.Mask = [2]uint64{}
		case delta >= 64:
			w.Mask = [2]uint64{0, w.Mask[0] << (delta - 64)}
		default:
			w.Mask = [2]uint64{w.Mask[0] << delta, w.Mask[1]<<delta | w.Mask[0]>>(64-delta)}
		}
		w.High = index
	}
	delta := w.High - index
	if delta >= 128 {
		return false
	}
	word, bit := delta/64, uint(delta%64)
	if w.Mask[word]&(uint64(1)<<bit) != 0 {
		return false
	}
	w.Mask[word] |= uint64(1) << bit
	return true
}

// Agent has no handler goroutines. The host serializes all its callbacks.
type Agent struct {
	state     State
	recording []byte // version byte, then (length uint16, encoded packet) records
	length    int
}

func New(agent.FactoryArgs) agent.Agent { return &Agent{state: State{Turn: 1, Phase: "recording"}} }
func Registration() workerapp.Registration {
	return workerapp.Registration{Name: Name, Factory: New, Tasks: map[string]agent.TaskHandler{TaskKind: Play}}
}
func (a *Agent) next(outcome agent.Outcome) agent.Effects {
	if outcome == agent.Capacity {
		a.state.Refused++
	}
	a.state.Turn++
	a.state.Phase = "recording"
	a.state.Cursor = 0
	a.state.Packets = 0
	a.state.Outcome = outcome
	a.recording = nil
	a.length = 0
	turn := a.state.Turn
	return agent.Effects{CancelBefore: &turn, ClearAudioBefore: &turn, CancelTimers: []string{recordTimer}}
}
func (a *Agent) finish() agent.Effects {
	if len(a.recording) <= 1 {
		return a.next(agent.OK)
	}
	// Replace the recording with the request. The host takes its own immutable
	// copy at admission; the app no longer claims these bytes in its next Save.
	request := a.recording
	// Keep the immutable buffer until the next callback confirms acceptance.
	// Restore can then roll back a refused task admission without losing bytes.
	a.length = 0
	a.state.Phase = "processing"
	return agent.Effects{CancelTimers: []string{recordTimer}, StartTasks: []agent.TaskSpec{{Step: "play", Kind: TaskKind, Turn: a.state.Turn, Class: agent.ReadOnly, Resume: agent.Retry, Request: request, Timeout: 15 * time.Second}}}
}
func (a *Agent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if err := ctx.Err(); err != nil {
		return agent.Effects{}, err
	}
	if a.state.Phase != "recording" {
		a.recording = nil
	}
	switch e.Kind {
	case agent.Started: // Started can be redelivered after an uncheckpointed start.
	case agent.Resumed:
		if e.Resume != nil {
			notice := *e.Resume
			// Duplicate ranges are host evidence, bounded independently. Keep compact
			// notice metadata here; the sparse app window already tracks accepted input.
			notice.DuplicateWindows = nil
			a.state.Resume = &notice
			if notice.MoveOnly == agent.OutcomeUnknown {
				return a.next(agent.OutcomeUnknown), nil
			}
		}
	case agent.AudioInput:
		if e.Input == nil {
			return agent.Effects{}, nil
		}
		if a.state.Seen.Set && a.state.Seen.SSRC != e.Input.Progress.SSRC {
			return a.next(agent.Failed), nil
		}
		if !a.state.Seen.Accept(e.Input.Progress.SSRC, e.Input.Progress.Index) {
			return agent.Effects{}, nil
		}
		if a.state.Phase != "recording" {
			return agent.Effects{}, nil
		}
		packet := e.Input.Payload
		// The host accepts encoded Opus; this example bounds individual records too.
		// An input which cannot fit is reported as Capacity, never silently "empty".
		if len(packet) == 0 || len(packet) > MaxRecordingBytes-3 {
			return a.next(agent.Capacity), nil
		}
		if a.length+2+len(packet) > MaxRecordingBytes {
			// This packet is consumed with an explicit capacity outcome. Existing
			// recording is still submitted; the diagnostic makes the loss visible.
			a.state.Outcome = agent.Capacity
			a.state.Refused++
			return a.finish(), nil
		}
		first := len(a.recording) == 0
		if first {
			a.recording = []byte{1}
		}
		a.recording = binary.BigEndian.AppendUint16(a.recording, uint16(len(packet)))
		a.recording = append(a.recording, packet...)
		a.length = len(a.recording)
		a.state.Packets++
		if a.length >= MaxRecordingBytes-1202 {
			return a.finish(), nil
		}
		if first {
			return agent.Effects{SetTimers: []agent.TimerSpec{{ID: recordTimer, After: RecordTime}}}, nil
		}
	case agent.TimerFired:
		if e.Timer != nil && e.Timer.ID == recordTimer && a.state.Phase == "recording" {
			return a.finish(), nil
		}
	case agent.TaskMessage:
		r := e.Result
		if r != nil && r.Turn == 0 && r.Outcome == agent.Capacity {
			// Non-task snapshot admission refusals have a host-derived ID and
			// no task turn. They still consume input and must remain visible.
			return a.next(agent.Capacity), nil
		}
		if r == nil || r.Turn != a.state.Turn {
			return agent.Effects{}, nil
		}
		if r.Sequence <= a.state.Cursor {
			return agent.Effects{}, nil
		}
		a.state.Cursor = r.Sequence
		if r.Final && r.Outcome != agent.OK {
			return a.next(r.Outcome), nil
		}
		if r.Chunk.Audio != nil {
			a.state.Phase = "playout"
			turn := a.state.Turn
			effects := agent.Effects{NotifyPlaybackFinished: &turn}
			for i := range r.Chunk.Audio.Packets {
				effects.Audio = append(effects.Audio, agent.Audio{Turn: turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence, PacketOffset: uint32(i)}})
			}
			return effects, nil
		}
	case agent.PlaybackFinished:
		if e.Playback != nil && e.Playback.Turn == a.state.Turn {
			return a.next(agent.OK), nil
		}
	default: // Unknown and reserved Control events have no effect.
	}
	return agent.Effects{}, nil
}
func (a *Agent) Save(context.Context) (agent.State, error) {
	data, err := json.Marshal(a.state)
	return agent.State{Version: 1, Bytes: data, HasMoveOnly: a.length > 0, MoveOnlyLength: a.length}, err
}
func (a *Agent) Restore(_ context.Context, s agent.State) error {
	if s.Version != 1 {
		return agent.ErrVersion
	}
	var state State
	if err := json.Unmarshal(s.Bytes, &state); err != nil {
		return err
	}
	if state.Turn == 0 || (state.Phase != "recording" && state.Phase != "processing" && state.Phase != "playout") {
		return errors.New("loopback: invalid state")
	}
	// Preserve an existing payload on callback rollback when its accepted length
	// matches. A fresh crash instance has no bytes until an exact RestoreMoveOnly.
	if len(a.recording) >= s.MoveOnlyLength {
		a.recording = a.recording[:s.MoveOnlyLength]
	} else {
		a.recording = nil
	}
	a.state, a.length = state, s.MoveOnlyLength
	return nil
}
func (a *Agent) SaveMoveOnly(context.Context) ([]byte, error) {
	if len(a.recording) != a.length {
		return nil, errors.New("loopback: missing recording")
	}
	return a.recording, nil
}
func (a *Agent) RestoreMoveOnly(_ context.Context, data []byte) error {
	if len(data) != a.length {
		return errors.New("loopback: recording length mismatch")
	}
	if _, err := packets(data); err != nil {
		return err
	}
	a.recording = append([]byte(nil), data...)
	return nil
}

func packets(data []byte) ([][]byte, error) {
	if len(data) == 0 || len(data) > MaxRecordingBytes || data[0] != 1 {
		return nil, errors.New("loopback: invalid recording version/size")
	}
	var out [][]byte
	for data = data[1:]; len(data) > 0; {
		if len(data) < 2 {
			return nil, errors.New("loopback: short packet header")
		}
		n := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if n == 0 || n > len(data) {
			return nil, errors.New("loopback: short packet")
		}
		out = append(out, data[:n])
		data = data[n:]
	}
	return out, nil
}

// duration reads the Opus TOC. It does not decode or claim full codec framing
// validation. The media host independently validates emitted packets.
func duration(p []byte) (time.Duration, error) {
	if len(p) == 0 {
		return 0, errors.New("empty Opus packet")
	}
	config := p[0] >> 3
	var us int
	switch {
	case config >= 16:
		us = 2500 << (config & 3)
	case config >= 12:
		us = 10000 << (config & 1)
	default:
		us = []int{10000, 20000, 40000, 60000}[config&3]
	}
	frames := 1
	switch p[0] & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 {
			return 0, errors.New("short Opus count")
		}
		frames = int(p[1] & 63)
	}
	if frames == 0 || frames*us > 120000 {
		return 0, errors.New("invalid Opus duration")
	}
	return time.Duration(frames*us) * time.Microsecond, nil
}

// Count reports the number of retained sparse indexes.
func (w Window) Count() int { return bits.OnesCount64(w.Mask[0]) + bits.OnesCount64(w.Mask[1]) }

var _ agent.MoveOnlyState = (*Agent)(nil)
