package clocks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func liveObservation(age time.Duration) LeaseObservation {
	o := leaseFixture()
	o.IntervalNS, o.HostElapsedNS = 20000000, 50000000
	o.ReferenceAgeNS = int64(age)
	o.MeasuredUnixNS = fmt.Sprint(int64(1792886524000000000) + int64(age))
	return o
}

func TestLeaseTransportAndReread(t *testing.T) {
	o := liveObservation(0)
	o.ErrorBoundNS = 59950
	l, err := (Lease{}).Observe(o, 0, 800*time.Millisecond)
	require.NoError(t, err)
	v, err := l.At(800 * time.Millisecond)
	require.NoError(t, err)
	if v.BoundNS <= 60000 {
		t.Fatal("MUTATION: lease-transport", v)
	}
	l, err = l.Observe(o, time.Second, 1100*time.Millisecond)
	require.NoError(t, err)
	v, err = l.At(2100 * time.Millisecond)
	require.NoError(t, err)
	if !v.Expired {
		t.Fatal("MUTATION: lease-reread-renewal", v)
	}
	o.ReferenceUnixNS, o.MeasuredUnixNS = "1792886525000000000", "1792886525000000000"
	l, err = l.Observe(o, 2*time.Second, 2100*time.Millisecond)
	require.NoError(t, err)
	v, err = l.At(2200 * time.Millisecond)
	require.NoError(t, err)
	require.False(t, v.Expired)
}

func TestLeaseRefreshReferenceAges(t *testing.T) {
	for _, age := range []time.Duration{1900 * time.Millisecond, 2 * time.Second, 2100 * time.Millisecond, 4100 * time.Millisecond} {
		t.Run(age.String(), func(t *testing.T) {
			now, calls := time.Unix(0, 0), 0
			first := liveObservation(age)
			_, err := RefreshLease(context.Background(), first, func() time.Time { return now }, func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }, func(context.Context) (LeaseObservation, error) {
				calls++
				fresh := first
				if age < 4*time.Second {
					fresh = liveObservation(0)
					fresh.ReferenceUnixNS, fresh.MeasuredUnixNS = "1792886525000000000", "1792886525000000000"
				}
				return fresh, nil
			})
			if age < 2*time.Second {
				require.NoError(t, err)
				require.Zero(t, calls)
			} else if age < 4*time.Second {
				if err != nil || calls == 0 {
					t.Fatal("MUTATION: admission-lease-refresh", age, calls, err)
				}
			} else {
				require.EqualError(t, err, "clock-lease-expired")
				require.Equal(t, 4*time.Second, now.Sub(time.Unix(0, 0)))
			}
		})
	}
}

func TestMalformedLeaseDoesNotRefresh(t *testing.T) {
	o := liveObservation(2 * time.Second)
	o.ErrorBoundNS = -1
	now, reads := time.Unix(0, 0), 0
	_, err := RefreshLease(context.Background(), o, func() time.Time { return now }, func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }, func(context.Context) (LeaseObservation, error) { reads++; return o, nil })
	if err == nil || err.Error() != "clock-lease-missing" || reads != 0 || now != time.Unix(0, 0) {
		t.Fatal("MUTATION: malformed-lease-refresh", err, reads)
	}
}
