package mediaworker

import (
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/stretchr/testify/require"
)

func TestAgentEventUsesStoreClockAtDispatch(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	base := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, clock := range []time.Time{base, base.Add(-time.Hour)} {
		s.mu.Lock()
		s.storeClock, s.storeClockAt = clock, time.Now()
		s.mu.Unlock()
		require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "same-event"}))
		require.WithinDuration(t, clock, a.lastEvent.At, time.Second)
	}
}

func TestAgentEventClockFallsBackUntilStoreObservation(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1}}
	_, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.mu.Lock()
	s.storeClockAt = time.Time{}
	s.mu.Unlock()
	before := time.Now()
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "same-event"}))
	require.False(t, a.lastEvent.At.Before(before))
	require.False(t, a.lastEvent.At.After(time.Now()))
	fallback := a.lastEvent.At
	store := before.Add(-time.Hour)
	s.mu.Lock()
	s.storeClock, s.storeClockAt = store, time.Now()
	s.mu.Unlock()
	require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: "same-event"}))
	require.WithinDuration(t, store, a.lastEvent.At, time.Second)
	require.True(t, a.lastEvent.At.Before(fallback))
}
