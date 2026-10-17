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

func TestControlAtomicReceiptAndConflict(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, initControlUnit(s))
	m := agent.ControlMessage{ID: "press", Sequence: 1, Name: "ptt.start", Data: []byte{0, 255}}
	receipt, err := s.processControl(m)
	require.NoError(t, err)
	require.False(t, receipt.Checkpointed)
	require.Equal(t, agent.Control, a.lastEvent.Kind)
	require.Equal(t, receipt.EventID, a.lastEvent.ID)
	require.Equal(t, m, *a.lastEvent.Control)
	same, err := s.processControl(m)
	require.NoError(t, err)
	require.Equal(t, receipt, same)
	require.Equal(t, 1, a.handles)
	s.controlAck = 1
	same, err = s.processControl(m)
	require.NoError(t, err)
	require.True(t, same.Checkpointed)
	for _, change := range []func(*agent.ControlMessage){func(m *agent.ControlMessage) { m.ID = "other" }, func(m *agent.ControlMessage) { m.Sequence++ }, func(m *agent.ControlMessage) { m.Data = []byte{1} }, func(m *agent.ControlMessage) { m.Name = "stop" }} {
		changed := m
		change(&changed)
		_, err = s.processControl(changed)
		require.ErrorIs(t, err, agent.ErrControlConflict)
	}
	require.Equal(t, 1, a.handles)
}
func TestControlLimitsFloorAndCopy(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, initControlUnit(s))
	m := agent.ControlMessage{ID: "1", Sequence: 1, Name: "start", Data: make([]byte, 1024)}
	_, err := s.processControl(m)
	require.NoError(t, err)
	a.lastEvent.Control.Data[0] = 1
	require.Zero(t, m.Data[0], "callback owns a copy")
	m.ID = "2"
	m.Sequence = 2
	m.Data = make([]byte, 1025)
	_, err = s.processControl(m)
	require.ErrorIs(t, err, agent.ErrControlInvalid)
	m.Data = nil
	for i := uint64(2); i < 20; i++ {
		m.ID = string(rune('a' + i))
		m.Sequence = i
		s.controlAck = i - 1
		_, err = s.processControl(m)
		require.NoError(t, err)
	}
	require.Greater(t, s.agent.latest.V2.ControlFloor, uint64(0))
	require.LessOrEqual(t, len(s.agent.latest.V2.Controls), controlWindow+1)
	m.ID = "late"
	m.Sequence = 1
	_, err = s.processControl(m)
	require.ErrorIs(t, err, agent.ErrControlStale)
	require.NoError(t, validateControlWindow(s.agent.latest.V2, s.controlAck))
}
func TestControlCapacityDoesNotSucceedOrConsume(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, initControlUnit(s))
	s.agent.v2.registration.Bounds.MaxPendingBytes = 1
	_, err := s.processControl(agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
	require.ErrorIs(t, err, agent.ErrCapacity)
	require.Zero(t, a.handles)
	require.Zero(t, s.agent.latest.V2.ControlHigh)
}
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
func TestControlFailedCallbackHasNoReceipt(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}, effects: agent.Effects{StartTasks: []agent.TaskSpec{{}}}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, initControlUnit(s))
	_, err := s.processControl(agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
	require.Error(t, err)
	require.Zero(t, s.agent.latest.V2.ControlHigh)
	require.Empty(t, s.state.Agent.V2.Controls)
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

func TestControlExactRestoreDoesNotInventCheckpoint(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, source := v2UnitSession(t, a)
	require.NoError(t, initControlUnit(source))
	m := agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"}
	receipt, err := source.processControl(m)
	require.NoError(t, err)
	require.False(t, receipt.Checkpointed)
	_, target := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
	target.state = source.state
	require.NoError(t, target.initAgent(true, ResumeOptions{}, &movePayload{ControlAck: 0}))
	receipt, err = target.processControl(m)
	require.NoError(t, err)
	require.False(t, receipt.Checkpointed, "an exact export is not an acknowledged checkpoint")
}

func TestControlCallbackCannotChangeReceiptIdentity(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	a.callback = func(context.Context) {
		a.lastEvent.Control.ID = "changed"
		a.lastEvent.Control.Sequence = 90
		a.lastEvent.Control.Data[0] = 99
	}
	_, s := v2UnitSession(t, a)
	require.NoError(t, initControlUnit(s))
	m := agent.ControlMessage{ID: "original", Sequence: 1, Name: "start", Data: []byte{1}}
	got, err := s.processControl(m)
	require.NoError(t, err)
	same, err := s.processControl(m)
	require.NoError(t, err)
	require.Equal(t, got, same)
	require.Equal(t, 1, a.handles)
	require.Equal(t, controlDigest(m), s.state.Agent.V2.Controls[0].Digest)
}

// These focused receipt tests model the actor after its Started transaction.
func initControlUnit(s *session) error {
	if err := s.initAgent(false, ResumeOptions{}); err != nil {
		return err
	}
	s.agent.latest.V2.Started = true
	s.state.Agent.V2.Started = true
	return nil
}

func TestControlWaitsForStarted(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	message := agent.ControlMessage{ID: "before-start", Sequence: 1, Name: "start"}
	_, err := s.processControl(message)
	require.Error(t, err, "Control before Started must be refused")
	require.True(t, controlRefusal(err), "not-ready must not end the call")
	require.Zero(t, a.handles)
	require.Zero(t, s.agent.latest.V2.ControlHigh)
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started}))
	receipt, err := s.processControl(message)
	require.NoError(t, err)
	require.Equal(t, uint64(1), receipt.Sequence)
	require.Equal(t, 2, a.handles)
}

type controlRejectAgent struct {
	v2UnitAgent
	failure string
}

func (a *controlRejectAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	effects, err := a.v2UnitAgent.Handle(ctx, e)
	if e.Kind != agent.Control {
		return effects, err
	}
	switch a.failure {
	case "deadline":
		<-ctx.Done()
		return agent.Effects{}, ctx.Err()
	case "error":
		return agent.Effects{}, errors.New("provider busy")
	}
	return effects, err
}
func TestControlHandlerRefusalRestoresAndRetries(t *testing.T) {
	for _, failure := range []string{"deadline", "error"} {
		t.Run(failure, func(t *testing.T) {
			a := &controlRejectAgent{v2UnitAgent: v2UnitAgent{state: agent.State{Version: 1, Bytes: []byte("accepted")}}, failure: failure}
			_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 250 * time.Millisecond })
			require.NoError(t, initControlUnit(s))
			m := agent.ControlMessage{ID: "retry", Sequence: 1, Name: "ptt.start"}
			before := cloneAgentState(s.state.Agent)
			_, err := s.processControl(m)
			require.Error(t, err)
			require.True(t, controlRefusal(err), "Control Handle failure must not end the call: %v", err)
			require.Equal(t, before, s.state.Agent)
			require.Equal(t, before.State.Bytes, a.state.Bytes)
			require.Equal(t, 1, a.restores)
			a.failure = ""
			receipt, err := s.processControl(m)
			require.NoError(t, err)
			require.Equal(t, uint64(1), receipt.Sequence)
			require.Equal(t, 2, a.handles)
		})
	}
}

func TestControlFullReceiptsLeaveTimerAndTaskCapacity(t *testing.T) {
	for _, withTask := range []bool{false, true} {
		t.Run(fmt.Sprint(withTask), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a)
			require.NoError(t, initControlUnit(s))
			// No store acknowledgement: fill the same receipt state as a stalled store.
			accepted := 0
			for i := uint64(1); i <= 40; i++ {
				id := fmt.Sprintf("%03d", i) + strings.Repeat("x", 125)
				_, err := s.processControl(agent.ControlMessage{ID: id, Sequence: i, Name: "press"})
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
				s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}
				a.effects.StartTasks = []agent.TaskSpec{{Step: "wait", Kind: "wait/v1", Class: agent.ReadOnly}}
			}
			require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "timer-task"}), "full receipts cannot starve non-Control metadata")
			require.Len(t, s.state.Agent.V2.Timers, 1)
			if withTask {
				require.Len(t, s.agent.v2.tasks, 1)
			}
			t.Logf("unacknowledged Controls capped at %d", accepted)
		})
	}
}

func TestControlReceiptEncodingIsFixedAndRestorable(t *testing.T) {
	for _, id := range []string{"short", strings.Repeat("x", 128)} {
		a := &v2UnitAgent{state: agent.State{Version: 1}}
		_, s := v2UnitSession(t, a)
		require.NoError(t, initControlUnit(s))
		m := agent.ControlMessage{ID: id, Sequence: 1, Name: "press"}
		original, err := s.processControl(m)
		require.NoError(t, err)
		wire, err := json.Marshal(s.agent.latest.V2.Controls[0])
		require.NoError(t, err)
		require.Len(t, wire, 102)
		require.NotContains(t, string(wire), id)
		var record controlRecord
		require.NoError(t, json.Unmarshal(wire, &record))
		s.agent.latest.V2.Controls[0] = record
		receipt, err := s.processControl(m)
		require.NoError(t, err)
		require.Equal(t, original, receipt)
		old, err := json.Marshal(map[string]any{"i": id, "s": m.Sequence, "d": controlDigest(m)})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(old, &record))
		s.agent.latest.V2.Controls[0] = record
		receipt, err = s.processControl(m)
		require.NoError(t, err)
		require.Equal(t, original, receipt, "initial slice-8a receipt reads migrate to digests")
	}
}
