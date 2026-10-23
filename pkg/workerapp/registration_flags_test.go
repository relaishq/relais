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

func TestSplitWorkingDeclarationRefusedBeforeStartup(t *testing.T) {
	calls := 0
	options := Options{DefaultAgent: "oversize/v1", Agents: []Registration{{Name: "oversize/v1", Limits: agent.Limits{MaxStateBytes: 16384}, Factory: func(agent.FactoryArgs) agent.Agent { calls++; return nil }}}}
	err := Run(context.Background(), []string{"-agent-split-checkpoints"}, options)
	require.ErrorIs(t, err, agent.ErrStateTooLarge)
	require.Zero(t, calls)
}

func TestSnapshotFlagsResolveSessionProfilesAndExplicitOverrides(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, zero := range []bool{false, true} {
			var args []string
			if split {
				args = append(args, "-agent-split-checkpoints")
			}
			if zero {
				args = append(args, "-agent-max-snapshot-bytes=0", "-agent-snapshot-bytes-per-second=0", "-agent-snapshot-burst-bytes=0")
			}
			limits, err := SnapshotLimitsForArgs(args)
			require.NoError(t, err)
			expected := mediaworker.SnapshotLimits{MaxBytes: 7168, BytesPerSecond: 81920, BurstBytes: 28672, MoveBytesPerSession: 1 << 20, MoveBytesTotal: 64 << 20}
			if split {
				expected = expected.SplitDefaults()
				expected.MaxBytes, expected.BytesPerSecond, expected.BurstBytes = 6144, 70000, 40960
			}
			require.Equal(t, expected, limits)
			flags, raw := newWorkerFlags(Options{})
			require.NoError(t, flags.Parse(args))
			require.Zero(t, raw.limits.MaxBytes, "raw zero must survive for legacy adoption and split selection")
			require.NoError(t, limits.Validate())
			args = append(args, "-agent-max-snapshot-bytes=9000", "-agent-snapshot-bytes-per-second=120000", "-agent-snapshot-burst-bytes=60000")
			limits, err = SnapshotLimitsForArgs(args)
			require.NoError(t, err)
			require.Equal(t, 9000, limits.MaxBytes)
			require.Equal(t, 120000, limits.BytesPerSecond)
			require.Equal(t, 60000, limits.BurstBytes)
		}
	}
	_, err := SnapshotLimitsForArgs([]string{"-agent-max-snapshot-bytes=invalid"})
	require.Error(t, err)
	// Preflight must resolve zero without changing the actual per-session input.
	err = Run(context.Background(), nil, Options{})
	require.NotErrorIs(t, err, agent.ErrCapacity)
}
