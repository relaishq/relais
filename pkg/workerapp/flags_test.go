package workerapp

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitCheckpointsFollowsAllWorkerBooleanFlags(t *testing.T) {
	flags, _ := newWorkerFlags(Options{})
	flags.VisitAll(func(f *flag.Flag) {
		if value, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && value.IsBoolFlag() {
			t.Run(f.Name, func(t *testing.T) {
				require.True(t, SplitCheckpoints([]string{"-" + f.Name, "-agent-split-checkpoints"}))
				require.False(t, SplitCheckpoints([]string{"-" + f.Name, "-agent-split-checkpoints=false"}))
			})
		}
	})
}
