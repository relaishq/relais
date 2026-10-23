package callharness

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestHarnessCadenceBudgetsKeepExplicitLegacyCap(t *testing.T) {
	require.Equal(t, mediaworker.SnapshotLimits{}, harnessSnapshotLimits(0))
	require.Equal(t, mediaworker.SnapshotLimits{}, harnessSnapshotLimits(100*time.Millisecond))
	for _, interval := range []time.Duration{10 * time.Millisecond, 33 * time.Millisecond, 50 * time.Millisecond} {
		limits := harnessSnapshotLimits(interval)
		require.Equal(t, 7168, limits.MaxBytes)
		split := limits.SplitDefaults()
		require.Equal(t, 7168, split.MaxBytes, "accelerated tests explicitly retain their chosen hot cap")
		require.Equal(t, limits.BytesPerSecond, split.BytesPerSecond)
		require.NoError(t, split.ValidateForInterval(interval))
	}
}
