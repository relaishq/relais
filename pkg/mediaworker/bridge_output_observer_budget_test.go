package mediaworker

import (
	"context"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func maximumBridgeObservers(t *testing.T, rig acceptedEventRig) {
	t.Helper()
	s := rig.s
	s.worker.cfg.Agent.CallbackTimeout = time.Hour
	rig.app.state.Bytes = make([]byte, s.agent.v2.registration.Bounds.MaxStateBytes)
	rig.app.state.History = make([]byte, 8192)
	handler := func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		return agent.Effects{StartTasks: []agent.TaskSpec{
			{Step: "s", Kind: "bridge.state/v1", Turn: 1, Class: agent.ReadOnly, Request: make([]byte, s.agent.v2.bridge.limits.ObserverRequestBytes/2)},
			{Step: "a", Kind: "bridge.audio/v1", Turn: 1, Class: agent.ReadOnly, Request: make([]byte, s.agent.v2.bridge.limits.ObserverRequestBytes/2)},
		}}, nil
	}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	require.Len(t, s.agent.v2.tasks, 2)
}

func TestBridgeOutputMaximumObserversKeepCredit(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	maximumBridgeObservers(t, rig)
	credit, err := rig.s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Positive(t, credit.PacketSlots)
	require.GreaterOrEqual(t, credit.PayloadBytes, 160)
	require.NoError(t, bridgeExportReservationFits(rig.s))
	require.Equal(t, "admitted", rig.s.admitBridgePacket(bridgeRequest(scope, 1, time.Now())).Outcome)
}

func TestBridgeOutputObserverRequestsBounded(t *testing.T) {
	rig, scope := newBridgeRig(t, false)
	s := rig.s
	handler := func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }
	s.agent.v2.registration.Tasks = map[string]agent.TaskHandler{"bridge.state/v1": handler, "bridge.audio/v1": handler}
	rig.app.handle = func(context.Context, agent.Event) (agent.Effects, error) {
		return agent.Effects{StartTasks: []agent.TaskSpec{
			{Step: "s", Kind: "bridge.state/v1", Turn: 1, Class: agent.ReadOnly, Request: make([]byte, s.agent.v2.bridge.limits.ObserverRequestBytes)},
			{Step: "a", Kind: "bridge.audio/v1", Turn: 1, Class: agent.ReadOnly, Request: make([]byte, s.agent.v2.bridge.limits.ObserverRequestBytes)},
		}}, nil
	}
	_, err := (acceptedEvents{s}).submit(agent.Event{Kind: agent.Started}, eventLive)
	require.NoError(t, err)
	require.Len(t, s.agent.v2.tasks, 1)
	require.Len(t, s.state.Agent.V2.Capacity, 1)
	require.Equal(t, agent.Capacity, s.state.Agent.V2.Capacity[0].result().Outcome)
	credit, err := s.worker.BridgeOutputCredit(scope)
	require.NoError(t, err)
	require.Positive(t, credit.PacketSlots)
	require.False(t, s.agent.failed.Load())
}
