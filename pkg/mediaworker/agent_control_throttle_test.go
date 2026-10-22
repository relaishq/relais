package mediaworker

import (
	"context"
	"fmt"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func controlThrottleSession(t *testing.T) (*session, *v2UnitAgent) {
	t.Helper()
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
	require.NoError(t, initControlUnit(s))
	for i := uint64(1); i <= 2; i++ {
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Control, Control: &agent.ControlMessage{ID: fmt.Sprint(i), Sequence: i, Name: "x"}}, eventLive)
		require.NoError(t, err)
	}
	return s, a
}

// Receipt pressure on a fan-out must not
// prevent caller audio, or let unrelated successful callbacks reset its delay.
func TestControlAudioBackpressureDoesNotThrottle(t *testing.T) {
	for _, source := range []string{"mailbox", "playback"} {
		t.Run(source, func(t *testing.T) {
			s, a := controlThrottleSession(t)
			r := agent.TaskResult{TaskID: "audio", Sequence: 1, Turn: 1, Outcome: agent.OK, Chunk: agent.Chunk{Audio: &agent.EncodedAudio{Format: "audio/opus", Packets: [][]byte{{0xf8, 0xff, 0xfe}}}}}
			intent := taskIntent{ID: r.TaskID, Turn: 1, Class: agent.ReadOnly, Phase: taskStarted, Published: 1}
			task := &localTask{intent: intent, results: []agent.TaskResult{r}, mailboxBytes: taskResultBytes(r), terminalBytes: taskTerminalBytes(intent)}
			s.agent.v2.tasks = []*localTask{task}
			require.NoError(t, s.registerAudioLocked(r))
			s.publishTaskIntents(s.agent.latest.V2)
			s.publishAgent()
			if source == "mailbox" {
				s.agent.v2.registration.Bounds.MaxMailboxBytes = task.mailboxBytes + task.terminalBytes
			} else {
				s.agent.v2.registration.Bounds.MaxPendingBytes = 700
			}
			a.effects = agent.Effects{Audio: []agent.Audio{{Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence}}, {Turn: r.Turn, Source: agent.AudioRef{TaskID: r.TaskID, Sequence: r.Sequence}}}}
			event := agent.Event{Kind: agent.TaskMessage, ID: "task/audio/1", Result: &r}
			_, eventErr6 := (acceptedEvents{s}).submit(event, eventLive)
			require.ErrorIs(t, eventErr6, errTaskAdmissionBackpressure)
			handles := a.handles
			a.effects = agent.Effects{}
			_, eventErr7 := (acceptedEvents{s}).submit(event, eventLive)
			require.NoError(t, eventErr7, "audio pressure must not arm receipt throttle")
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
	_, eventErr8 := (acceptedEvents{s}).submit(first, eventLive)
	require.ErrorIs(t, eventErr8, errTaskAdmissionBackpressure)
	handles := a.handles
	_, eventErr9 := (acceptedEvents{s}).submit(second, eventLive)
	require.ErrorIs(t, eventErr9, errTaskAdmissionBackpressure)
	require.Equal(t, handles+1, a.handles, "a different event has its own first attempt")
	for _, event := range []agent.Event{first, second} {
		_, eventErr10 := (acceptedEvents{s}).submit(event, eventLive)
		require.ErrorIs(t, eventErr10, errTaskAdmissionBackpressure)
	}
	require.Equal(t, handles+1, a.handles, "both blocked events retain independent delays")
}
