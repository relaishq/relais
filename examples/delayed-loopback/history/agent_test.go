package history

import (
	"context"
	"testing"

	legacy "github.com/relaishq/relais/pkg/agent"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestHistoryLayoutsAndRollback(t *testing.T) {
	ctx := context.Background()
	for _, split := range []bool{false, true} {
		args := agent.FactoryArgs{MaxStateBytes: 8192}
		if split {
			args.MaxStateBytes = 3072
			args.MaxHistoryBytes = 8192
		}
		a := Registration(split, false).Factory(args).(*Agent)
		before, err := a.Save(ctx)
		require.NoError(t, err)
		if split {
			require.Len(t, before.Bytes, 2871)
			require.Len(t, before.History, 4205)
		} else {
			require.Len(t, before.Bytes, 7087)
			require.Empty(t, before.History)
		}
		originalHistory := append([]byte(nil), a.history...)
		for i := uint64(0); i < InputsPerTurn; i++ {
			effects, err := a.Handle(ctx, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{Progress: legacy.Progress{SSRC: 7, Index: i}, Payload: []byte{0xf8, 0xff, 0xfe}}})
			require.NoError(t, err)
			if i+1 < InputsPerTurn {
				require.Equal(t, originalHistory, a.history)
			} else {
				require.NotEqual(t, originalHistory, a.history)
				require.Len(t, effects.StartTasks, 1)
				require.Equal(t, agent.SideEffecting, effects.StartTasks[0].Class)
			}
		}

		changed, err := a.Save(ctx)
		require.NoError(t, err)
		require.NotEqual(t, before, changed)
		require.NoError(t, a.Restore(ctx, before))
		restored, err := a.Save(ctx)
		require.NoError(t, err)
		require.Equal(t, before, restored)
		bad := before
		bad.Version = 2
		require.ErrorIs(t, a.Restore(ctx, bad), agent.ErrVersion)
	}
}

func TestChurnRefusalRetainsAcceptedHistory(t *testing.T) {
	ctx := context.Background()
	a := Registration(true, true).Factory(agent.FactoryArgs{MaxHistoryBytes: 8192}).(*Agent)
	a.state.Inputs = 399
	before, err := a.Save(ctx)
	require.NoError(t, err)
	_, err = a.Handle(ctx, agent.Event{Kind: agent.AudioInput, Input: &legacy.Input{Progress: legacy.Progress{SSRC: 7, Index: 400}, Payload: []byte{0xf8, 0xff, 0xfe}}})
	require.NoError(t, err)
	require.NotEqual(t, before.History, a.history)
	require.NoError(t, a.Restore(ctx, before))
	_, err = a.Handle(ctx, agent.Event{Kind: agent.TaskMessage, Result: &agent.TaskResult{Outcome: agent.Capacity}})
	require.NoError(t, err)
	after, err := a.Save(ctx)
	require.NoError(t, err)
	require.Equal(t, before.History, after.History)
	require.EqualValues(t, 1, a.state.Refused)
}
