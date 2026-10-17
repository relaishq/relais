package workerapp

import (
	"context"
	"testing"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestRegistrationStartupAdmission(t *testing.T) {
	factory := func(agent.FactoryArgs) agent.Agent { return nil }
	limits := mediaworker.SnapshotLimits{MaxBytes: 7168, BytesPerSecond: 81920, BurstBytes: 28672, MoveBytesPerSession: 1 << 20, MoveBytesTotal: 64 << 20}
	for _, name := range []string{"echo", "demo", ""} {
		_, err := registeredAgents(Options{Agents: []Registration{{Name: name, Factory: factory}}}, "echo", limits)
		require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	}
	reg := Registration{Name: "external/v1", Factory: factory}
	_, err := registeredAgents(Options{Agents: []Registration{reg, reg}}, reg.Name, limits)
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	reg.Limits.MaxStateBytes = 1025
	_, err = registeredAgents(Options{Agents: []Registration{reg}}, reg.Name, limits)
	require.NoError(t, err)
	reg.Limits.MaxStateBytes = 8192
	// Exercise Run: refusal must occur before opening listeners/stores, and even
	// when this registration is not the default selected for new calls.
	err = Run(context.Background(), nil, Options{Agents: []Registration{reg}})
	require.ErrorIs(t, err, agent.ErrStateTooLarge)
	reg.Limits.MaxStateBytes = 1024
	reg.Limits.MaxPendingBytes, reg.Limits.MaxMailboxBytes, reg.Limits.MaxActiveTasks = 1<<20, 16<<20, 100
	cfg, err := registeredAgents(Options{Agents: []Registration{reg}}, reg.Name, limits)
	require.NoError(t, err)
	require.Len(t, cfg.Registrations, 1)
}
