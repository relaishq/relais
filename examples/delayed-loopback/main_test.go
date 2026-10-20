package main

import (
	"testing"

	"github.com/relaishq/relais/examples/delayed-loopback/history"
	"github.com/stretchr/testify/require"
)

func TestSplitCheckpointsSelectsHistoryLimits(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		enabled bool
	}{
		{nil, false},
		{[]string{"-name", "worker", "-agent-split-checkpoints"}, true},
		{[]string{"--agent-split-checkpoints=true"}, true},
		{[]string{"-agent-split-checkpoints", "-agent-split-checkpoints=false"}, false},
		{[]string{"-frame-cache-off", "-redis-cluster", "-srtp-diagnostics", "-agent-split-checkpoints=1"}, true},
		{[]string{"--", "-agent-split-checkpoints"}, false},
		{[]string{"positional", "-agent-split-checkpoints"}, false},
		{[]string{"-name", "-agent-split-checkpoints"}, false},
	} {
		enabled := splitCheckpoints(tc.args)
		require.Equal(t, tc.enabled, enabled, "%v", tc.args)
		for _, churn := range []bool{false, true} {
			reg := history.Registration(enabled, churn)
			if enabled {
				require.Zero(t, reg.Limits.MaxStateBytes)
				require.Zero(t, reg.Limits.MaxHistoryBytes)
			} else {
				require.Equal(t, 8192, reg.Limits.MaxStateBytes)
			}
		}
	}
}
