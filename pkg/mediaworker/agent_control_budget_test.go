package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestControlBackpressureWireCode(t *testing.T) {
	response := httptest.NewRecorder()
	workerAPIError(response, errTaskAdmissionBackpressure)
	var failure struct{ Code string }
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
	require.Equal(t, "agent_backpressure", failure.Code)
	require.True(t, controlRefusal(errTaskAdmissionBackpressure))
}

type controlSlowAgent struct {
	v2UnitAgent
	mode string
}

func (a *controlSlowAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	effects, err := a.v2UnitAgent.Handle(ctx, e)
	if e.Kind == agent.Control && a.mode != "save" {
		<-ctx.Done()
		time.Sleep(time.Millisecond)
	}
	return effects, err
}
func (a *controlSlowAgent) Save(ctx context.Context) (agent.State, error) {
	if a.lastEvent.Kind == agent.Control && a.mode == "save" {
		<-ctx.Done()
		time.Sleep(time.Millisecond)
	}
	return a.v2UnitAgent.Save(ctx)
}
func (a *controlSlowAgent) Restore(ctx context.Context, state agent.State) error {
	if a.mode == "restore" {
		return errors.New("restore broken")
	}
	return a.v2UnitAgent.Restore(ctx, state)
}

func TestControlTerminalCausesOutrankRefusals(t *testing.T) {
	require.True(t, controlRefusal(agent.ErrBackpressure))
	require.False(t, controlRefusal(errors.Join(legacy.ErrRestore, agent.ErrCapacity)))
	require.False(t, controlRefusal(errors.Join(legacy.ErrSave, agent.ErrBackpressure)))
}

func TestControlSmallPendingUnsupported(t *testing.T) {
	for _, limit := range []int{250, 350, 400} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{MaxPendingBytes: limit}, nil))
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			if limit > 250 {
				_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
				require.NoError(t, err)
			}
			handles := a.handles

			before := cloneAgentState(s.state.Agent)
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "1", Sequence: 1, Name: "stop"}}, eventLive)
			require.ErrorIs(t, err, agent.ErrControlUnsupported)
			require.Equal(t, handles, a.handles)
			require.Equal(t, before, s.state.Agent)
		})
	}
}

func TestControlFanOutBackpressureKeepsAcceptedState(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{MaxPendingBytes: 900}, map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}), func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
	require.NoError(t, initControlUnit(s))

	for i := uint64(1); i <= 2; i++ {
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"}}, eventLive)
		require.NoError(t, err)
	}

	for i := range 10 {
		a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "wait/v1", Class: agent.ReadOnly})
	}
	before := cloneAgentState(s.state.Agent)
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "fan"}, eventLive)
	require.ErrorIs(t, err, errTaskAdmissionBackpressure)
	require.Equal(t, before, s.state.Agent)
	require.Empty(t, s.agent.v2.tasks)
}

func TestControlQuarantine(t *testing.T) {
	for _, mode := range []string{"handle", "save", "restore"} {
		t.Run(mode, func(t *testing.T) {
			a := &controlSlowAgent{v2UnitAgent: v2UnitAgent{state: agent.State{Version: 1}}, mode: mode}
			_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, nil), func(c *AgentConfig) { c.CallbackTimeout = 250 * time.Millisecond })
			require.NoError(t, initControlUnit(s))
			_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "1", Sequence: 1, Name: "x"}}, eventLive)
			require.Error(t, err)
			require.Zero(t, s.agent.latest.V2.ControlHigh)
			require.False(t, s.agent.overdue.Load())
			switch mode {
			case "handle":
				require.ErrorIs(t, err, agent.ErrControlRejected)
				require.True(t, controlRefusal(err))
			case "save":
				require.ErrorIs(t, err, legacy.ErrSave)
				require.False(t, controlRefusal(err))
			case "restore":
				require.ErrorIs(t, err, legacy.ErrRestore)
				require.False(t, controlRefusal(err))
			}
		})
	}
}

func TestControlBudgetMath(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, nil))
	require.NoError(t, initControlUnit(s))
	count := 0
	for i := uint64(1); i <= 10; i++ {
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"}}, eventLive)
		if err != nil {
			require.ErrorIs(t, err, agent.ErrCapacity)
			break
		}
		count++
		raw, err := json.Marshal(s.agent.latest.V2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(raw), v2MetadataCharge(*s.agent.latest.V2, 2048))
	}
	require.Equal(t, 2, count)
}

func TestControlMoveBudgetRemainsTerminal(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	w, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, nil))
	require.NoError(t, initControlUnit(s))
	otherSessions := w.cfg.SnapshotLimits.MoveBytesTotal
	w.moveBytes.Add(otherSessions)
	defer w.moveBytes.Add(-otherSessions)
	a.state.HasMoveOnly = true
	a.state.MoveOnlyLength = 1
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "1", Sequence: 1, Name: "x"}}, eventLive)
	require.ErrorIs(t, err, ErrMoveBudget)
	require.False(t, controlRefusal(err), "existing terminal contract determines the public definite class")
	require.Zero(t, s.state.Agent.V2.ControlHigh)
}

// Adapted from TestZZR2FanOutHeadroom. The maximum case holds all receipts
// unacknowledged, as during a store stall. Test the same effects in every case.
func TestControlFanOutHeadroom(t *testing.T) {
	for _, held := range []string{"zero", "one", "maximum", "maximum-counters"} {
		for _, width := range []int{20, 24, 33} {
			t.Run(fmt.Sprintf("%s/%d", held, width), func(t *testing.T) {
				a := &v2UnitAgent{state: agent.State{Version: 1}}
				_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}), func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
				require.NoError(t, initControlUnit(s))
				if held != "zero" {
					for i := uint64(1); i <= 40; i++ {
						_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"}}, eventLive)
						if err != nil {
							require.ErrorIs(t, err, agent.ErrCapacity)
							break
						}
						if held == "one" {
							break
						}
					}
				}
				if held == "maximum-counters" {
					// Counter exhaustion is an encoded adoption input: no live
					// host can reach it by executing 2^64 submissions in a test.
					wire, err := json.Marshal(s.state)
					require.NoError(t, err)
					var state sessionState
					require.NoError(t, json.Unmarshal(wire, &state))
					e := state.Agent.V2
					e.ControlHigh = math.MaxUint64
					e.ControlFloor = math.MaxUint64 - 2
					for i := range e.Controls {
						m := agent.ControlMessage{ID: fmt.Sprint(i + 1), Sequence: math.MaxUint64 - 1 + uint64(i), Name: "x"}
						e.Controls[i].Sequence = m.Sequence
						e.Controls[i].Digest = controlDigest(m)
					}
					a = &v2UnitAgent{state: agent.State{Version: 1}}
					_, s = v2UnitSessionFromState(t, a, state, v2UnitRegistration(a, agent.Limits{}, map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}), func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
					require.NoError(t, s.initAgent(true, ResumeOptions{}, &movePayload{ControlAck: math.MaxUint64 - 2}))
				}
				raw, _ := json.Marshal(s.agent.latest.V2)
				t.Logf("held=%d metadata=%d", len(s.agent.latest.V2.Controls), len(raw))
				for i := range width {
					a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprintf("s%02d", i), Kind: "wait/v1", Class: agent.ReadOnly})
				}
				_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "fan"}, eventLive)
				require.NoError(t, err, "Control must not make a refusal-only callback terminal")
				require.Equal(t, width, len(s.agent.v2.tasks)+len(s.agent.latest.V2.Capacity))
				t.Logf("admitted=%d refused=%d", len(s.agent.v2.tasks), len(s.agent.latest.V2.Capacity))
				if width == 33 && (held == "maximum" || held == "maximum-counters") {
					wire, err := json.Marshal(s.state)
					require.NoError(t, err)
					var restored sessionState
					require.NoError(t, json.Unmarshal(wire, &restored))
					b := &v2UnitAgent{state: agent.State{Version: 1}}
					_, target := v2UnitSessionFromState(t, b, restored, v2UnitRegistration(b, agent.Limits{}, map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}), func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
					require.NoError(t, target.initAgent(true, ResumeOptions{}, &movePayload{ControlAck: checkpointObservation(s).controlAck}))
					require.Equal(t, s.agent.latest.V2.Capacity, target.agent.latest.V2.Capacity)
					require.Equal(t, s.agent.latest.V2.Controls, target.agent.latest.V2.Controls)
				}

				a.effects = agent.Effects{}
				t.Cleanup(s.stopTaskAttempts)
				for range s.state.Agent.V2.Tasks {
					s.launchTasks()
					s.acceptEmission(<-s.agent.v2.emissions)
					delivered, err := s.deliverTaskResult()
					require.NoError(t, err, "terminal publication must fit beside refusals")
					require.True(t, delivered)
				}
				for range width {
					_, err := s.deliverTaskResult()
					require.NoError(t, err)
				}
				require.Empty(t, s.agent.latest.V2.Capacity)
			})
		}
	}
}
