package mediaworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	"github.com/relaishq/relais/pkg/agent/demo"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestControlLegacyRejected(t *testing.T) {
	for _, factory := range []legacy.Factory{func(string) legacy.Agent { return &legacy.Echo{} }, func(string) legacy.Agent { return &demo.Agent{} }} {
		w, err := New(Config{Agent: AgentConfig{Factory: factory}})
		require.NoError(t, err)
		s := sessionFromState(w, sessionState{Version: 7, ID: "legacy"})
		require.NoError(t, s.initAgent(false, ResumeOptions{}))
		w.mu.Lock()
		w.sessions[s.id] = s
		w.mu.Unlock()
		before := cloneAgentState(s.state.Agent)
		_, err = w.AgentControl(context.Background(), s.id, agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
		require.ErrorIs(t, err, agent.ErrControlUnsupported)
		require.Equal(t, before, s.state.Agent)
		s.close()
		require.NoError(t, w.Close())
	}
}
func TestControlCanonicalIdentity(t *testing.T) {
	m := agent.ControlMessage{ID: "a/b", Sequence: 2, Name: "c", Data: []byte{255, 0}}
	copy := m
	copy.Data = slices.Clone(m.Data)
	require.Equal(t, controlDigest(m), controlDigest(copy))
	copy.ID = "a"
	copy.Name = "b/c"
	require.NotEqual(t, controlDigest(m), controlDigest(copy))
	require.True(t, controlRefusal(errors.Join(agent.ErrCapacity, errors.New("full"))))
}

// These focused receipt tests model the actor after its Started transaction.

func TestControlWaitsForStarted(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	message := agent.ControlMessage{ID: "before-start", Sequence: 1, Name: "start"}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &message}, eventLive)
	require.Error(t, err, "Control before Started must be refused")
	require.True(t, controlRefusal(err), "not-ready must not end the call")
	require.Zero(t, a.handles)
	require.Zero(t, s.agent.latest.V2.ControlHigh)
	_, eventErr15 := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, eventErr15)
	receipt, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &message}, eventLive)
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Sequence)
	require.Equal(t, 2, a.handles)
}

func TestControlAtomicReceiptAndConflict(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, nil))
	require.NoError(t, initControlUnit(s))
	m := agent.ControlMessage{ID: "press", Sequence: 1, Name: "ptt.start", Data: []byte{0, 255}}
	receipt, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.NoError(t, err)
	require.False(t, receipt.Checkpointed)
	require.Equal(t, agent.Control, a.lastEvent.Kind)
	require.Equal(t, receipt.EventID, a.lastEvent.ID)
	require.Equal(t, m, *a.lastEvent.Control)
	same, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.NoError(t, err)
	require.Equal(t, receipt, same)
	require.Equal(t, 2, a.handles)
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent, controlAck: ptrControlAck(1)}))
	same, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.NoError(t, err)
	require.True(t, same.Checkpointed)
	for _, change := range []func(*agent.ControlMessage){func(m *agent.ControlMessage) { m.ID = "other" }, func(m *agent.ControlMessage) { m.Sequence++ }, func(m *agent.ControlMessage) { m.Data = []byte{1} }, func(m *agent.ControlMessage) { m.Name = "stop" }} {
		changed := m
		change(&changed)
		_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &changed}, eventLive)
		require.ErrorIs(t, err, agent.ErrControlConflict)
	}
	require.Equal(t, 2, a.handles)
}

func TestControlLimitsFloorAndCopy(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, nil))
	require.NoError(t, initControlUnit(s))
	m := agent.ControlMessage{ID: "1", Sequence: 1, Name: "start", Data: make([]byte, 1024)}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.NoError(t, err)
	a.lastEvent.Control.Data[0] = 1
	require.Zero(t, m.Data[0], "callback owns a copy")
	m.ID = "2"
	m.Sequence = 2
	m.Data = make([]byte, 1025)
	_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.ErrorIs(t, err, agent.ErrControlInvalid)
	m.Data = nil
	for i := uint64(2); i < 20; i++ {
		m.ID = string(rune('a' + i))
		m.Sequence = i
		require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent, controlAck: ptrControlAck(i - 1)}))
		_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
		require.NoError(t, err)
	}
	require.Greater(t, s.agent.latest.V2.ControlFloor, uint64(0))
	require.LessOrEqual(t, len(s.agent.latest.V2.Controls), controlWindow+1)
	m.ID = "late"
	m.Sequence = 1
	_, err = (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.ErrorIs(t, err, agent.ErrControlStale)
	require.NoError(t, validateControlWindow(s.agent.latest.V2, checkpointObservation(s).controlAck))
}

func TestControlCapacityDoesNotSucceedOrConsume(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{MaxPendingBytes: 400}, nil))
	require.NoError(t, initControlUnit(s))

	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"}}, eventLive)
	require.ErrorIs(t, err, agent.ErrControlUnsupported)
	require.Equal(t, 1, a.handles)
	require.Zero(t, s.agent.latest.V2.ControlHigh)
}

func TestControlFailedCallbackHasNoReceipt(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, nil))
	require.NoError(t, initControlUnit(s))
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{}}}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"}}, eventLive)
	require.Error(t, err)
	require.Zero(t, s.agent.latest.V2.ControlHigh)
	require.Empty(t, s.state.Agent.V2.Controls)
}

func TestControlFullReceiptsLeaveTimerAndTaskCapacity(t *testing.T) {
	for _, withTask := range []bool{false, true} {
		t.Run(fmt.Sprint(withTask), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a, v2UnitRegistration(a, agent.Limits{}, map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}))
			require.NoError(t, initControlUnit(s))
			// No store acknowledgement: fill the same receipt state as a stalled store.
			accepted := 0
			for i := uint64(1); i <= 40; i++ {
				id := fmt.Sprintf("%03d", i) + strings.Repeat("x", 125)
				_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: id, Sequence: i, Name: "press"}}, eventLive)
				if err != nil {
					require.ErrorIs(t, err, agent.ErrCapacity)
					break
				}
				accepted++
			}
			require.Positive(t, accepted)
			require.Less(t, accepted, 40)
			a.effects = agent.Effects{SetTimers: []agent.TimerSpec{{ID: strings.Repeat("t", 64), After: time.Second}}}
			if withTask {

				a.effects.StartTasks = []agent.TaskSpec{{Step: "wait", Kind: "wait/v1", Class: agent.ReadOnly}}
			}
			_, eventErr20 := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "timer-task"}, eventLive)
			require.NoError(t, eventErr20, "full receipts cannot starve non-Control metadata")
			require.Len(t, s.state.Agent.V2.Timers, 1)
			if withTask {
				require.Len(t, s.agent.v2.tasks, 1)
			}
			t.Logf("unacknowledged Controls capped at %d", accepted)
		})
	}
}

func initControlUnit(s *session) error {
	if err := s.initAgent(false, ResumeOptions{}); err != nil {
		return err
	}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started, ID: "started/control"}, eventLive)
	return err
}

func TestControlExactRestoreDoesNotInventCheckpoint(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, source := v2UnitSession(t, a)
	require.NoError(t, initControlUnit(source))
	m := agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"}
	receipt, err := (acceptedEvents{source}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.NoError(t, err)
	require.False(t, receipt.Checkpointed)
	wire, err := json.Marshal(source.state)
	require.NoError(t, err)
	var state sessionState
	require.NoError(t, json.Unmarshal(wire, &state))
	_, target := v2UnitSessionFromState(t, &v2UnitAgent{state: agent.State{Version: 1}}, state)
	require.NoError(t, target.initAgent(true, ResumeOptions{}, &movePayload{ControlAck: 0}))
	receipt, err = (acceptedEvents{target}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
	require.NoError(t, err)
	require.False(t, receipt.Checkpointed, "an exact export is not an acknowledged checkpoint")
}

func TestControlReceiptEncodingIsFixedAndRestorable(t *testing.T) {
	for _, id := range []string{"short", strings.Repeat("x", 128)} {
		a := &v2UnitAgent{state: agent.State{Version: 1}}
		_, s := v2UnitSession(t, a)
		require.NoError(t, initControlUnit(s))
		m := agent.ControlMessage{ID: id, Sequence: 1, Name: "press"}
		original, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
		require.NoError(t, err)
		wire, err := json.Marshal(s.state.Agent.V2.Controls[0])
		require.NoError(t, err)
		require.Len(t, wire, 102)
		require.NotContains(t, string(wire), id)
		for _, legacyRecord := range []bool{false, true} {
			encoded, err := json.Marshal(s.state)
			require.NoError(t, err)
			var state sessionState
			require.NoError(t, json.Unmarshal(encoded, &state))
			record := wire
			if legacyRecord {
				record, err = json.Marshal(map[string]any{"i": id, "s": m.Sequence, "d": controlDigest(m)})
				require.NoError(t, err)
			}
			require.NoError(t, json.Unmarshal(record, &state.Agent.V2.Controls[0]))
			_, target := v2UnitSessionFromState(t, &v2UnitAgent{state: agent.State{Version: 1}}, state)
			require.NoError(t, target.initAgent(true, ResumeOptions{}, &movePayload{}))
			receipt, err := (acceptedEvents{target}).submit(agent.Event{Kind: agent.Control, Control: &m}, eventLive)
			require.NoError(t, err)
			require.Equal(t, original, receipt, "both receipt encodings must restore the same identity")
		}
	}
}
