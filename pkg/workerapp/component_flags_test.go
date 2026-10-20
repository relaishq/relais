package workerapp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitCheckpointFlagIsExplicitAndOptional(t *testing.T) {
	for _, args := range [][]string{{"-version"}, {"-agent-split-checkpoints", "-version"}, {"-agent-split-checkpoints=false", "-version"}} {
		require.NoError(t, Run(context.Background(), args, Options{}))
	}
	require.ErrorContains(t, Run(context.Background(), []string{"-agent-split-checkpoints=invalid"}, Options{}), "invalid boolean value")
}
