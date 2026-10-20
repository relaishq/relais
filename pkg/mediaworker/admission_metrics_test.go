package mediaworker

import (
	"context"
	"testing"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestAdmissionTimingUsesProductionPaths(t *testing.T) {
	legacySession := &session{worker: &Worker{}}
	require.ErrorIs(t, legacySession.validateAgentState(legacy.State{}), legacy.ErrVersion)
	require.EqualValues(t, 1, legacySession.worker.metrics.admissionCalls.Load())
	require.Positive(t, legacySession.worker.metrics.admissionWallNS.Load())
	s := componentWriterSession(t, sessionstore.NewMemory(), true)
	s.worker.splitLimits = SnapshotLimits{}.splitDefaults()
	s.agent = &agentHost{latest: cloneAgentState(s.state.Agent), v2: &v2Host{registration: agenthost.Registration{Bounds: effectiveAgentBounds(agent.Limits{}, s.snapshotLimits(), true)}}}
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	before := s.worker.metrics.admissionCalls.Load()
	_, err := s.admissionSnapshot(cloneAgentState(s.state.Agent))
	require.NoError(t, err)
	require.Equal(t, before+1, s.worker.metrics.admissionCalls.Load())
	require.Positive(t, s.worker.metrics.admissionWallNS.Load())
	require.NoError(t, s.persistSnapshotBudget(context.Background(), true, true))
	require.EqualValues(t, 1, s.worker.metrics.forcedAttempts.Load())
	require.Positive(t, s.worker.metrics.forcedWallNS.Load())
	require.Positive(t, s.worker.metrics.forcedMaxNS.Load())
}
