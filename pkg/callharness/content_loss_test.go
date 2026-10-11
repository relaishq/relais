package callharness

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentLossBurstUsesSendOrderAndSuccessfulWrites(t *testing.T) {
	for _, scattered := range []bool{false, true} {
		t.Run(fmt.Sprint(scattered), func(t *testing.T) {
			var units []contentUnit
			for i := 1; i <= 30; i++ {
				at := time.Duration(i) * time.Millisecond
				returned := at + time.Millisecond
				lost := i <= 15
				if scattered {
					lost = i%2 == 0
				}
				if lost {
					returned = 0
				}
				units = append(units, contentUnit{at: at, returnedAt: returned, written: true, identity: fmt.Sprint(i)})
			}
			// Recorder maps do not promise order. Reverse order before counting.
			for i, j := 0, len(units)-1; i < j; i, j = i+1, j-1 {
				units[i], units[j] = units[j], units[i]
			}
			units = append(units, contentUnit{at: 1500 * time.Microsecond, returnedAt: 2 * time.Millisecond, written: false}, contentUnit{at: 40 * time.Millisecond, written: true})
			counts := countContentLoss(units, time.Millisecond, 31*time.Millisecond, 50*time.Millisecond)
			require.True(t, counts.Trusted)
			require.True(t, counts.RunTrusted)
			require.Equal(t, 30, counts.Sent)
			require.Equal(t, 15, counts.Lost)
			want := 15
			if scattered {
				want = 1
			}
			require.Equal(t, want, counts.LongestLostRun)
		})
	}
}

func TestContentLossBurstUsesObservationHorizon(t *testing.T) {
	units := []contentUnit{{at: time.Millisecond, written: true, returnedAt: 100 * time.Millisecond}, {at: 2 * time.Millisecond, written: true, returnedAt: 3 * time.Millisecond}, {at: 3 * time.Millisecond, written: true}}
	counts := countContentLoss(units, 0, 4*time.Millisecond, 50*time.Millisecond)
	require.Equal(t, 2, counts.Lost)
	require.Equal(t, 1, counts.LongestLostRun)
	counts = countContentLoss(units, 0, 4*time.Millisecond, 150*time.Millisecond)
	require.Equal(t, 1, counts.Lost)
	require.Equal(t, 1, counts.LongestLostRun)
	require.False(t, countContentLoss(nil, 0, time.Second, time.Second).RunTrusted)
}
