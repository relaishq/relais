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
	for i, clock := range []time.Time{base, base.Add(-time.Hour)} {
		s.mu.Lock()
		s.storeClock, s.storeClockAt = clock, time.Now()
		s.mu.Unlock()
		require.NoError(t, s.handleV2Event(agent.Event{Kind: agent.Started, ID: agent.EventID(string(rune('a' + i)))}))
		require.WithinDuration(t, clock, a.lastEvent.At, time.Second)
	}
}
