package mediaworker

import (
	"context"
	"fmt"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestTaskCapacityWideCallback(t *testing.T) {
	for _, width := range []int{20, 33, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			// Admission must refuse by width, not by the 100 ms callback deadline,
			// which a 100-task callback can reach under -race on a CI runner.
			_, s := v2UnitSession(t, a, func(c *AgentConfig) { c.CallbackTimeout = 5 * time.Second })
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"x/v1": func(context.Context, *agent.TaskRun) error { return nil }}
			for i := range width {
				a.effects.StartTasks = append(a.effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "x/v1", Class: agent.ReadOnly})
			}
			err := s.handleV2Event(agent.Event{ID: "started/x", Kind: agent.Started})
			if width == 100 {
				require.ErrorIs(t, err, agent.ErrCapacity)
				require.NotErrorIs(t, err, errTaskAdmissionBackpressure)
				return
			}
			require.NoError(t, err)
			require.True(t, s.agent.latest.V2.Started)
			if width == 33 {
				require.Empty(t, s.agent.v2.tasks)
			}
			require.Equal(t, width, len(s.agent.v2.tasks)+len(s.agent.latest.V2.Capacity))
			require.NoError(t, s.taskBudget(s.agent.v2.tasks, 0))
			for _, task := range s.agent.v2.tasks {
				s.terminalTask(task, agent.OK, "")
				require.NoError(t, s.taskBudget(s.agent.v2.tasks, 0), "terminal publication must fit beside refusals")
			}
			a.effects = agent.Effects{}
			for range width {
				_, err := s.deliverTaskResult()
				require.NoError(t, err)
			}
			require.Empty(t, s.agent.latest.V2.Capacity)
			for _, task := range s.agent.v2.tasks {
				require.Equal(t, task.intent.Published, task.intent.Cursor)
			}
		})
	}
}
func TestTaskCapacityDuplicateCause(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"x/v1": func(context.Context, *agent.TaskRun) error { return nil }}
	for i := range 7 {
		a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "x/v1", Class: agent.ReadOnly}}}
		require.NoError(t, s.handleV2Event(agent.Event{ID: agent.EventID(fmt.Sprint("fill/", i)), Kind: agent.Started}))
	}
	a.effects = agent.Effects{StartTasks: []agent.TaskSpec{{Step: "s", Kind: "x/v1", Class: agent.ReadOnly}, {Step: "t", Kind: "x/v1", Class: agent.ReadOnly}}}
	cause := agent.Event{ID: "audio/1/99", Kind: agent.Started}
	require.NoError(t, s.handleV2Event(cause))
	before := len(s.agent.latest.V2.Capacity)
	require.NoError(t, s.handleV2Event(cause))
	require.Equal(t, before, len(s.agent.latest.V2.Capacity))
	require.NoError(t, s.validateTaskMoves(s.agent.latest.V2.Tasks, nil))
}
func TestTaskCapacityDuplicateRestore(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			_, s := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			s.agent.latest.V2.Capacity = []taskRefusal{{ID: "pending", Turn: 4}, {ID: "pending", Turn: 4}}
			var move *movePayload
			if exact {
				move = &movePayload{}
			}
			require.NoError(t, s.restoreTasks(move))
			require.Len(t, s.agent.latest.V2.Capacity, 1)
			require.Equal(t, agent.Turn(4), s.agent.latest.V2.Capacity[0].Turn)
		})
	}
}
func TestTaskCapacityPruneUsesReceiveHorizon(t *testing.T) {
	_, s := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	intent := taskIntent{ID: "receipt", Class: agent.ReadOnly, Phase: taskTerminal, Final: true, Terminal: agent.OK, Published: 1, Cursor: 1, InputCause: &taskCause{SSRC: 42, Index: 10}}
	s.agent.v2.tasks = []*localTask{{intent: intent}}
	s.publishTaskIntents()
	s.publishAgent()
	s.mu.Lock()
	s.durableAgent = cloneAgentState(s.state.Agent)
	s.state.Agent.Progress.SSRC = 42
	s.state.Agent.Progress.Index = 10
	s.state.SRTP.Inbound = map[uint32]uint64{42: 74}
	s.mu.Unlock()
	s.pruneTasks()
	require.Empty(t, s.agent.v2.tasks, "received packets must release the receipt while application input is held")
}
