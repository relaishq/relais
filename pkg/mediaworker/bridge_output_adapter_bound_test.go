package mediaworker

import (
	"context"
	"testing"
	"testing/synctest"

	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

// A declared adapter bound is a contract, not pressure that playback can drain.
func TestBridgeOutputOversizeAdapterSaveIsTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig, _ := newBridgeRig(t, false)
		s := rig.s
		rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
			rig.app.state.HasMoveOnly = true
			rig.app.state.MoveOnlyLength = s.agent.v2.bridge.limits.AdapterMoveOnlyBytes + 1
			return agent.Effects{}, nil
		}
		s.agent.v2.instance = &bridgeEnvelopeAgent{acceptedEventAgent: rig.app}
		_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
		require.ErrorIs(t, err, agent.ErrCapacity)
		require.ErrorIs(t, err, legacy.ErrSave)
		require.NotErrorIs(t, err, errTaskAdmissionBackpressure)
		require.False(t, s.agent.latest.V2.Started)
		rig.app.events = nil
		startBridgeActor(t, s)
		synctest.Wait()
		require.True(t, s.agent.failed.Load(), "invalid Save must close instead of retrying forever")
		require.ErrorIs(t, s.ctx.Err(), context.Canceled)
		require.Len(t, rig.app.events, 1, "one failed callback, no retry loop")
	})
}
