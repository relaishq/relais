// Package demo plays an owned, deterministic Opus scale, one count per 500 ms
// of consumed audio. It requires no decoder, TTS service, or runtime encoder.
package demo

import (
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/relais/pkg/agent"
)

const (
	StateVersion = 1
	FrameSamples = 960 // 20 ms on the Opus 48 kHz clock.
	CountSamples = 24000
)

//go:embed assets/*.opuspackets
var assets embed.FS
var tones = loadTones()

func loadTones() [11][][]byte {
	var result [11][][]byte
	for i := range result {
		data, err := assets.ReadFile(fmt.Sprintf("assets/%02d.opuspackets", i))
		if err != nil {
			panic(err)
		}
		for len(data) > 0 {
			if len(data) < 2 {
				panic("demo: truncated asset length")
			}
			n := int(binary.BigEndian.Uint16(data))
			data = data[2:]
			if n == 0 || n > len(data) || data[0] != 0x98 {
				panic("demo: invalid Opus asset")
			}
			result[i] = append(result[i], slices.Clone(data[:n]))
			data = data[n:]
		}
		want := 25
		if i == 10 {
			want = 5
		}
		if len(result[i]) != want {
			panic("demo: invalid asset duration")
		}
	}
	return result
}

type Position struct {
	Count        uint64 `json:"count"`
	PhaseSamples uint32 `json:"phase_samples"`
}

// Resume keeps immutable evidence of where Restore put the application before
// any new input. Ages are measured by the host, never guessed from cadence.
type Resume struct {
	Kind                 agent.ResumeKind        `json:"kind"`
	Position             Position                `json:"position"`
	Progress             agent.Progress          `json:"progress"`
	CheckpointAgeMS      float64                 `json:"checkpoint_age_ms"`
	SnapshotAgeMS        float64                 `json:"snapshot_age_ms"`
	InputMayBeDuplicated bool                    `json:"input_may_be_duplicated"`
	DuplicateWindows     []agent.DuplicateWindow `json:"duplicate_windows"`
	Number               uint64                  `json:"number"`
}

type State struct {
	Position         Position       `json:"position"`
	LastInput        agent.Progress `json:"last_input"`
	HighWater        uint64         `json:"high_water"`
	ReplaySuppressed uint64         `json:"replay_suppressed"`
	SeenInput        bool           `json:"seen_input"`
	ChimeFrame       uint32         `json:"chime_frame"` // zero means no pending chime; 1..5 plays it.
	LastResume       *Resume        `json:"last_resume,omitempty"`
}

type Agent struct{ state State }

func New() *Agent                { return &Agent{state: State{Position: Position{Count: 1}}} }
func Factory(string) agent.Agent { return New() }

func Decode(saved agent.State) (State, error) {
	if saved.Version != StateVersion {
		return State{}, agent.ErrVersion
	}
	var state State
	if err := json.Unmarshal(saved.Bytes, &state); err != nil {
		return State{}, fmt.Errorf("demo: state: %w", err)
	}
	if state.Position.Count == 0 || state.Position.PhaseSamples >= CountSamples || state.Position.PhaseSamples%FrameSamples != 0 || state.ChimeFrame > 5 {
		return State{}, errors.New("demo: invalid position")
	}
	return state, nil
}
func (a *Agent) Save(ctx context.Context) (agent.State, error) {
	if err := ctx.Err(); err != nil {
		return agent.State{}, err
	}
	data, err := json.Marshal(a.state)
	return agent.State{Version: StateVersion, Bytes: data}, err
}
func (a *Agent) Restore(ctx context.Context, saved agent.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := Decode(saved)
	if err != nil {
		return err
	}
	a.state = state
	return nil
}
func (a *Agent) Resume(ctx context.Context, notice agent.ResumeNotice) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if notice.Kind != agent.PlannedMove && notice.Kind != agent.Takeover {
		return errors.New("demo: unknown resume kind")
	}
	// The host must pair our saved dedup floor with the same consumed progress.
	if a.state.SeenInput && a.state.LastInput != notice.Progress {
		return errors.New("demo: state/progress mismatch")
	}
	n := uint64(1)
	if a.state.LastResume != nil {
		n = a.state.LastResume.Number + 1
	}
	a.state.LastResume = &Resume{Kind: notice.Kind, Position: a.state.Position, Progress: notice.Progress,
		CheckpointAgeMS: float64(notice.CheckpointAge) / 1e6, SnapshotAgeMS: float64(notice.SnapshotAge) / 1e6,
		InputMayBeDuplicated: notice.InputMayBeDuplicated, DuplicateWindows: slices.Clone(notice.DuplicateWindows), Number: n}
	a.state.ChimeFrame = 1
	return nil
}

func (a *Agent) Process(ctx context.Context, in agent.Input) (agent.Output, error) {
	if err := ctx.Err(); err != nil {
		return agent.Output{}, err
	}
	// Resume's flag/window identifies replay. Only input at or below our saved
	// consumed floor is already represented. The rest of a replay window is new
	// work relative to this checkpoint and MUST count, even if the old worker saw it.
	if a.state.SeenInput && in.Progress.SSRC == a.state.LastInput.SSRC && in.Progress.Index <= a.state.HighWater {
		if r := a.state.LastResume; r != nil && r.InputMayBeDuplicated {
			inWindow := len(r.DuplicateWindows) == 0
			for _, window := range r.DuplicateWindows {
				if window.SSRC == in.Progress.SSRC && in.Progress.Index >= window.First && in.Progress.Index <= window.Last {
					inWindow = true
				}
			}
			if inWindow {
				a.state.ReplaySuppressed++
			}
		}
		a.state.LastInput = in.Progress
		return agent.Output{Changed: true}, nil
	}
	frames, err := inputFrames(in.Payload)
	if err != nil {
		return agent.Output{}, err
	}
	state := a.state
	packets := make([][]byte, 0, frames)
	for range frames {
		tone := state.Position.Count % 10
		frame := state.Position.PhaseSamples / FrameSamples
		packet := tones[tone][frame]
		if state.ChimeFrame > 0 {
			packet = tones[10][state.ChimeFrame-1]
			state.ChimeFrame++
			if state.ChimeFrame > 5 {
				state.ChimeFrame = 0
			}
		}
		packets = append(packets, packet)
		state.Position.PhaseSamples += FrameSamples
		if state.Position.PhaseSamples == CountSamples {
			state.Position.Count++
			state.Position.PhaseSamples = 0
		}
	}
	payload, err := join(packets)
	if err != nil {
		return agent.Output{}, err
	}
	state.LastInput = in.Progress
	state.HighWater = in.Progress.Index
	state.SeenInput = true
	a.state = state
	return agent.Output{Audio: payload, Changed: true}, nil
}

// inputFrames reads the Opus TOC duration. This demo accepts 20..120 ms packets
// in 20 ms units; unsupported short frames fail explicitly rather than drift.
func inputFrames(packet []byte) (int, error) {
	if len(packet) == 0 {
		return 0, errors.New("demo: empty Opus input")
	}
	config := int(packet[0] >> 3)
	samples := 0
	switch {
	case config >= 16:
		samples = 120 << (config & 3)
	case config >= 12:
		samples = 480 << (config & 1)
	case config&3 == 3:
		samples = 2880
	default:
		samples = 480 << (config & 3)
	}
	n := 1
	switch packet[0] & 3 {
	case 1, 2:
		n = 2
	case 3:
		if len(packet) < 2 {
			return 0, errors.New("demo: truncated Opus input")
		}
		n = int(packet[1] & 63)
	}
	total := samples * n
	if n == 0 || total > 5760 || total%FrameSamples != 0 {
		return 0, errors.New("demo: requires Opus duration in 20 ms units (20..120 ms)")
	}
	return total / FrameSamples, nil
}

func join(packets [][]byte) ([]byte, error) {
	if len(packets) == 1 {
		return slices.Clone(packets[0]), nil
	}
	// RFC 6716 code-3 VBR framing. All generated CELT frames share one TOC.
	out := []byte{packets[0][0] | 3, 0x80 | byte(len(packets))}
	for i, p := range packets {
		if p[0] != packets[0][0] {
			return nil, errors.New("demo: incompatible generated frames")
		}
		n := len(p) - 1
		if i == len(packets)-1 {
			break
		}
		if n < 252 {
			out = append(out, byte(n))
		} else {
			out = append(out, byte(252+n%4), byte((n-252)/4))
		}
	}
	for _, p := range packets {
		out = append(out, p[1:]...)
	}
	return out, nil
}

// Evidence projects only application state and input progress from the private
// version-7 snapshot. Transport keys never leave the worker's private API.
type Evidence struct {
	Position Position       `json:"position"`
	Progress agent.Progress `json:"progress"`
}

func FromSnapshot(data []byte) (Evidence, error) {
	var snap struct {
		Version int
		State   struct {
			Agent struct {
				State    agent.State
				Progress agent.Progress
			}
		}
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return Evidence{}, err
	}
	if snap.Version != 7 {
		return Evidence{}, agent.ErrVersion
	}
	state, err := Decode(snap.State.Agent.State)
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{state.Position, snap.State.Agent.Progress}, nil
}

// Status is safe to proxy to the page. Checkpoint is the input snapshot at the
// successful resume boundary; Exported is the flushed source of a planned move.
type Status struct {
	State
	Progress   agent.Progress `json:"progress"`
	Checkpoint *Evidence      `json:"checkpoint,omitempty"`
	Exported   *Evidence      `json:"exported,omitempty"`
}
