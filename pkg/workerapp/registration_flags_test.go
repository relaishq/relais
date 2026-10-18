package workerapp

import (
	"context"
	"strconv"
	"testing"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestAgentNames(t *testing.T) {
	options := Options{DefaultAgent: "missing", Agents: []Registration{
		{Name: "zeta/v1", Factory: func(agent.FactoryArgs) agent.Agent { return nil }},
		{Name: "alpha/v1", Factory: func(agent.FactoryArgs) agent.Agent { return nil }},
	}}
	err := Run(context.Background(), nil, options)
	require.ErrorContains(t, err, "missing")
	require.ErrorContains(t, err, "alpha/v1, demo, echo, zeta/v1")
}
func TestFlagBudgetValidation(t *testing.T) {
	for _, tc := range []struct {
		flag    string
		value   int
		minimum string
	}{
		{"agent-snapshot-bytes-per-second", 30720, "78848"},
		{"agent-snapshot-burst-bytes", 7168, "14336"},
		{"agent-max-snapshot-bytes", int(^uint(0) >> 1), ""},
	} {
		err := Run(context.Background(), []string{"-" + tc.flag, strconv.Itoa(tc.value)}, Options{})
		require.ErrorIs(t, err, agent.ErrCapacity)
		require.ErrorContains(t, err, tc.flag)
		if tc.minimum != "" {
			require.ErrorContains(t, err, tc.minimum)
		}
	}
	// A registration's declared zero defaults must stay identical across operators.
	l := mediaworker.SnapshotLimits{MaxBytes: 7168, BytesPerSecond: 81920, BurstBytes: 28672, MoveBytesPerSession: 128 << 10, MoveBytesTotal: 64 << 20}
	_, err := registeredAgents(Options{Agents: []Registration{{Name: "custom", Factory: func(agent.FactoryArgs) agent.Agent { return nil }}}}, "custom", l)
	require.NoError(t, err)
}
