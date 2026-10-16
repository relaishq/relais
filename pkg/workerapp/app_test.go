package workerapp

import (
	"context"
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunReturnsFlagErrorsWithoutExiting(t *testing.T) {
	// Repeated calls must not register flags on the process-global FlagSet.
	for range 2 {
		require.ErrorIs(t, Run(context.Background(), []string{"-h"}, Options{}), flag.ErrHelp)
		require.ErrorContains(t, Run(context.Background(), []string{"-unknown-worker-flag"}, Options{}), "flag provided but not defined")
		require.NoError(t, Run(context.Background(), []string{"-version"}, Options{}))
	}
}
