package controlplane

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

// MaxAge=550ms selects scaled margins, not a hard cutoff. At default rates,
// a fresh sent track accepts ceil(6250*a)+65 + ceil(5000*(a+b))+64 <= 32766,
// where b is the remaining adoption budget. With b=1.5s this is a<=2.2344s.
func TestDefaultTakeoverSnapshotAgeBound(t *testing.T) {
	envelope := (mediaworker.CheckpointEnvelope{}).Defaults()
	require.Equal(t, 1500*time.Millisecond, takeoverBudget)
	bound := 2234400 * time.Microsecond
	for _, delta := range []time.Duration{-time.Microsecond, time.Microsecond} {
		age := bound + delta
		margin, _, outside, err := envelope.CheckpointMargins(age, mediaworker.CheckpointState{}, defaultSequenceMargin, defaultSRTCPIndexMargin)
		require.NoError(t, err)
		require.True(t, outside, "the 550 ms envelope scales rather than refusing this age")
		reserve := envelope.CallerSequenceReserve(mediaworker.CheckpointState{}, age+takeoverBudget)
		attempts, err := mediaworker.SequenceResumeAttemptsWithReserve(takeoverSnapshot(t, "age-bound", 0), margin, reserve)
		if delta < 0 {
			require.NoError(t, err)
			require.GreaterOrEqual(t, attempts, 1)
		} else {
			require.ErrorIs(t, err, mediaworker.ErrSequenceBudgetExhausted)
		}
	}
}
