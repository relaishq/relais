// Package history supplies a keyless, bounded Demo A-sized traffic fixture.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relaishq/relais/examples/delayed-loopback/loopback"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/workerapp"
)

const Name = "history-loopback/v1"
const ChurnName = "history-churn/v1"
const WorkingBytes = 2871
const InputsPerTurn = 250 // 50 input packets/s: one turn every five seconds.

type exchange struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
}
type saved struct {
	Loopback             json.RawMessage `json:"loopback"`
	Inputs               uint64          `json:"inputs"`
	Refused              uint64          `json:"refused"`
	Padding              string          `json:"padding"`
	History              json.RawMessage `json:"history,omitempty"`
	RestoredWorkingBytes int             `json:"restored_working_bytes,omitempty"`
	RestoredHistoryBytes int             `json:"restored_history_bytes,omitempty"`
}
type Agent struct {
	base                       *loopback.Agent
	state                      saved
	history                    []byte
	split, churn               bool
	workingBytes, historyBytes int
	fullSize                   bool
}

// Full-size measurement is opt-in; ordinary Demo A-sized fixtures stay exact.
func Registration(split, churn, fullSize bool) workerapp.Registration {
	name := Name
	if churn {
		name = ChurnName
	}
	limits := agent.Limits{MaxStateBytes: 8192}
	full := fullSize
	workingBytes, historyBytes := WorkingBytes, 4205
	if full {
		workingBytes, historyBytes = 3072, 8192
		limits.MaxStateBytes = workingBytes + historyBytes + len(`,"history":`)
	}
	if split {
		limits = agent.Limits{}
	}
	return workerapp.Registration{Name: name, Limits: limits, Factory: func(args agent.FactoryArgs) agent.Agent {
		return &Agent{base: loopback.New(args).(*loopback.Agent), history: conversationSize(0, historyBytes), split: args.MaxHistoryBytes > 0, churn: churn, workingBytes: workingBytes, historyBytes: historyBytes, fullSize: full}
	}, Tasks: map[string]agent.TaskHandler{loopback.TaskKind: loopback.Play, "history/intent/v1": func(context.Context, *agent.TaskRun) error { return nil }}}
}
func conversationSize(turn uint64, size int) []byte {
	exchanges := make([]exchange, 4)
	for i := range exchanges {
		prefix := fmt.Sprintf("%020d-%d", turn, i)
		exchanges[i] = exchange{User: prefix + strings.Repeat("u", 512-len(prefix)), Assistant: prefix + strings.Repeat("a", 512-len(prefix))}
	}
	exchanges[len(exchanges)-1].Assistant += strings.Repeat("a", size-4205)
	data, _ := json.Marshal(exchanges)
	return data
}
func (a *Agent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.TaskMessage && e.Result != nil && e.Result.Outcome == agent.Capacity && e.Result.Turn == 0 {
		a.state.Refused++
		// A refused candidate has already been rolled back. Keep that prior history
		// in this compact acknowledgement; never retry it inside the refusal callback.
		return agent.Effects{}, nil
	}
	effects, err := a.base.Handle(ctx, e)
	if err == nil && e.Kind == agent.AudioInput && e.Input != nil {
		a.state.Inputs++
		if a.state.Inputs%InputsPerTurn == 0 || (a.churn && a.state.Inputs >= 400) {
			turn := a.state.Inputs / InputsPerTurn
			if a.churn && a.state.Inputs >= 400 {
				turn = a.state.Inputs
			}
			a.history = conversationSize(turn, a.historyBytes)
			if a.state.Inputs%InputsPerTurn == 0 {
				effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: "intent", Kind: "history/intent/v1", Turn: agent.Turn(1000000 + turn), Class: agent.SideEffecting, Resume: agent.Abandon, Timeout: time.Second})
			}
		}
	}
	return effects, err
}
func (a *Agent) Save(ctx context.Context) (agent.State, error) {
	s, err := a.base.Save(ctx)
	if err != nil {
		return s, err
	}
	state := a.state
	state.Loopback = s.Bytes
	state.History = nil
	state.Padding = ""
	data, err := json.Marshal(state)
	if err != nil {
		return s, err
	}
	if len(data) > a.workingBytes {
		return s, errors.New("history fixture: working metadata exceeds fixture bound")
	}
	state.Padding = strings.Repeat("w", a.workingBytes-len(data))
	if !a.split {
		state.History = a.history
	}
	s.Bytes, err = json.Marshal(state)
	s.Version = 1
	if a.split {
		s.History = append([]byte(nil), a.history...)
	}
	return s, err
}
func (a *Agent) Restore(ctx context.Context, s agent.State) error {
	if s.Version != 1 {
		return agent.ErrVersion
	}
	var state saved
	if err := json.Unmarshal(s.Bytes, &state); err != nil {
		return err
	}
	history := s.History
	if !a.split {
		history = state.History
	}
	var exchanges []exchange
	if len(history) != a.historyBytes || json.Unmarshal(history, &exchanges) != nil || len(exchanges) != 4 {
		return errors.New("history fixture: invalid conversation")
	}
	if a.fullSize {
		workingBytes := len(s.Bytes)
		if !a.split {
			workingBytes -= len(history) + len(`,"history":`)
		}
		if workingBytes != a.workingBytes {
			return errors.New("history fixture: invalid working size")
		}
		state.RestoredWorkingBytes, state.RestoredHistoryBytes = workingBytes, len(history)
	}
	s.Version = 2
	s.Bytes = state.Loopback
	s.History = nil
	if err := a.base.Restore(ctx, s); err != nil {
		return err
	}
	a.state = state
	a.history = append([]byte(nil), history...)
	return nil
}
func (a *Agent) SaveMoveOnly(ctx context.Context) ([]byte, error) { return a.base.SaveMoveOnly(ctx) }
func (a *Agent) RestoreMoveOnly(ctx context.Context, data []byte) error {
	return a.base.RestoreMoveOnly(ctx, data)
}

var _ agent.MoveOnlyState = (*Agent)(nil)
