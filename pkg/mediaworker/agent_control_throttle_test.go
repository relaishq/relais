package mediaworker

import (
	"context"
	"fmt"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func controlThrottleSession(t *testing.T) (*session, *v2UnitAgent) {
	t.Helper()
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
	require.NoError(t, initControlUnit(s))
	for i := uint64(1); i <= 2; i++ {
		_, err := s.processControl(agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"})
		require.NoError(t, err)
	}
	return s, a
}

// Receipt pressure on a fan-out must not
// prevent caller audio, or let unrelated successful callbacks reset its delay.
func TestControlReceiptThrottleAllowsOtherEvents(t *testing.T) {
	s, a := controlThrottleSession(t)
	s.agent.v2.registration.Bounds.MaxPendingBytes = 900
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	fan := agent.Effects{}
	for i := 0; i < 10; i++ {
		fan.StartTasks = append(fan.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "wait/v1", Class: agent.ReadOnly})
	}
	blocked := agent.Event{Kind: agent.Started, ID: "fan"}
	a.effects = fan
	require.ErrorIs(t, s.handleV2Event(blocked), errTaskAdmissionBackpressure)
	handles := a.handles
	require.ErrorIs(t, s.handleV2Event(blocked), errTaskAdmissionBackpressure)
	require.Equal(t, handles, a.handles)
	a.effects = agent.Effects{}
	input := legacy.Input{}
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.AudioInput, ID: "audio/1/1", Input: &input}))
	require.Equal(t, handles+1, a.handles)
	require.EqualValues(t, 1, s.agent.latest.Progress.Consumed)
	a.effects = fan
	require.ErrorIs(t, s.handleV2Event(blocked), errTaskAdmissionBackpressure)
	require.Equal(t, handles+1, a.handles, "unrelated success keeps the blocked event throttled")
	require.NoError(t, s.checkpoint.restore(checkpointRestore{pair: &s.state.Agent, controlAck: ptrControlAck(2)}))
	require.NoError(t, s.handleV2Event(blocked), "acknowledgement permits immediate retry")
	require.Equal(t, handles+2, a.handles)
}

func TestControlAudioBackpressureDoesNotThrottle(t *testing.T) {
	for _, source := range []string{"mailbox", "playback"} {
		t.Run(source, func(t *testing.T) {
			s, a := controlThrottleSession(t)
			r := agent.TaskResult{TaskID: "audio", Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}
			intent := taskIntent{ID: r.TaskID, Turn: 1, Class: agent.ReadOnly, Phase: taskStarted, Published: 1}
			task := &localTask{intent: intent, results: []agent.TaskResult{r}, mailboxBytes: taskResultBytes(r), terminalBytes: taskTerminalBytes(intent)}
			s.agent.v2.tasks = []*localTask{task}
			require.NoError(t, s.registerAudioLocked(r))
			s.publishTaskIntents()
			s.publishAgent()
			if source == "mailbox" {
				s.agent.v2.registration.Bounds.MaxMailboxBytes = task.mailboxBytes + task.terminalBytes
			} else {
				s.agent.v2.registration.Bounds.MaxPendingBytes = 700
			}
			a.effects = agent.Effects{Audio: []agent.Audio{{Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence}}, {Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence}}}}
			event := agent.Event{Kind: agent.TaskMessage, ID: "task/audio/1", Result: &r}
			require.ErrorIs(t, s.handleV2Event(event), errTaskAdmissionBackpressure)
			handles := a.handles
			a.effects = agent.Effects{}
			require.NoError(t, s.handleV2Event(event), "audio pressure must not arm receipt throttle")
			require.Equal(t, handles+1, a.handles)
		})
	}
}

func TestControlReceiptThrottleKeepsIndependentDelays(t *testing.T) {
	s, a := controlThrottleSession(t)
	s.agent.v2.registration.Bounds.MaxPendingBytes = 900
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"wait/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	for i := 0; i < 10; i++ {
		a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "wait/v1", Class: agent.ReadOnly})
	}
	first := agent.Event{Kind: agent.Started, ID: "fan/first"}
	second := agent.Event{Kind: agent.Started, ID: "fan/second"}
	require.ErrorIs(t, s.handleV2Event(first), errTaskAdmissionBackpressure)
	handles := a.handles
	require.ErrorIs(t, s.handleV2Event(second), errTaskAdmissionBackpressure)
	require.Equal(t, handles+1, a.handles, "a different event has its own first attempt")
	for _, event := range []agent.Event{first, second} {
		require.ErrorIs(t, s.handleV2Event(event), errTaskAdmissionBackpressure)
	}
	require.Equal(t, handles+1, a.handles, "both blocked events retain independent delays")
}
