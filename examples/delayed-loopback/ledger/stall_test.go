package ledger

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestStallRegistrationDefersIntentsAndRetainsTerminals(t *testing.T) {
	a := &stallAgent{}
	ctx := context.Background()
	fx, err := a.Handle(ctx, agent.Event{Kind: agent.Started})
	require.NoError(t, err)
	require.Empty(t, fx.StartTasks)
	fx, err = a.Handle(ctx, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{}})
	require.NoError(t, err)
	require.Len(t, fx.StartTasks, 2)
	require.Equal(t, 15*time.Second, fx.StartTasks[0].Timeout)
	require.Equal(t, 2*time.Second, fx.StartTasks[1].Timeout)
	for _, spec := range fx.StartTasks {
		require.Equal(t, agent.SideEffecting, spec.Class)
		require.Equal(t, agent.Retry, spec.Resume)
	}
	fx, err = a.Handle(ctx, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{}})
	require.NoError(t, err)
	require.Empty(t, fx.StartTasks)
	for _, r := range []agent.TaskResult{{Turn: 2, Outcome: agent.Timeout, Final: true}, {Turn: 1, Outcome: agent.OK, Final: true}, {Turn: 99, Outcome: agent.Capacity, Final: true}} {
		_, err = a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &r})
		require.NoError(t, err)
	}
	require.Len(t, a.Outcomes, 2)
	state, err := a.Save(ctx)
	require.NoError(t, err)
	restored := &stallAgent{}
	require.NoError(t, restored.Restore(ctx, state))
	require.Equal(t, a, restored)
	require.ErrorIs(t, restored.Restore(ctx, agent.State{Version: 2, Bytes: json.RawMessage(`{}`)}), agent.ErrVersion)
}
