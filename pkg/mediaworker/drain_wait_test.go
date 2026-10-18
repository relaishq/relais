package mediaworker

import (
	"context"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestDrainWaitReadOnlyKindPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		class  agent.TaskClass
		listed bool
		wait   bool
	}{
		{"listed-read-only", agent.ReadOnly, true, false},
		{"unlisted-read-only", agent.ReadOnly, false, true},
		{"listed-side-effecting", agent.SideEffecting, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			h := s.agent.v2
			h.registration.Tasks = map[string]agent.TaskHandler{"task/v1": func(ctx context.Context, _ *agent.TaskRun) error {
				<-ctx.Done()
				return ctx.Err()
			}}
			if tc.listed {
				h.registration.SkipDrainWait = []string{"task/v1"}
			}
			intent := taskIntent{ID: "running", Kind: "task/v1", Class: tc.class, Resume: agent.Retry, Phase: taskNever}
			if tc.class == agent.SideEffecting {
				// Provide the durable launch grant through the normal host seam.
				intent.Phase = taskStarted
				s.durableAgent = cloneAgentState(s.agent.latest)
				s.durableAgent.V2.Tasks = []taskIntent{intent}
			}
			h.tasks = []*localTask{{intent: intent, terminalBytes: taskTerminalBytes(intent)}}
			s.launchTasks()
			require.True(t, h.tasks[0].launched)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			err := w.QuiesceTasks(ctx)
			if tc.wait {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.NoError(t, err)
			}
			// Both counted and skipped handlers retain the launch fence.
			h.tasks = append(h.tasks, &localTask{intent: taskIntent{ID: "queued", Kind: "task/v1", Class: agent.ReadOnly, Phase: taskNever}})
			s.launchTasks()
			require.False(t, h.tasks[1].launched)
			require.Equal(t, taskNever, h.tasks[1].intent.Phase)
			s.stopTaskAttempts()
		})
	}
}
