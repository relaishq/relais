package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitCheckpointsRegistrationFlag(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		enabled bool
	}{
		{nil, false},
		{[]string{"-name", "worker", "-agent-split-checkpoints"}, true},
		{[]string{"--agent-split-checkpoints=true"}, true},
		{[]string{"-agent-split-checkpoints", "-agent-split-checkpoints=false"}, false},
		{[]string{"-frame-cache-off", "-agent-split-checkpoints=1"}, true},
		{[]string{"--", "-agent-split-checkpoints"}, false},
		{[]string{"positional", "-agent-split-checkpoints"}, false},
		{[]string{"-name", "-agent-split-checkpoints"}, false},
	} {
		require.Equal(t, tc.enabled, splitCheckpoints(tc.args), "%v", tc.args)
	}
}
