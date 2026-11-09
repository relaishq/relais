package clocks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	v, err = l.At(5100 * time.Millisecond)
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
	for _, age := range []time.Duration{4800 * time.Millisecond, 5 * time.Second, 5100 * time.Millisecond, 10100 * time.Millisecond} {
		t.Run(age.String(), func(t *testing.T) {
			now, calls := time.Unix(0, 0), 0
			first := liveObservation(age)
			_, err := RefreshLease(context.Background(), first, func() time.Time { return now }, func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }, func(context.Context) (LeaseObservation, error) {
				calls++
				fresh := first
				if age < 10*time.Second {
					fresh = liveObservation(0)
					fresh.ReferenceUnixNS, fresh.MeasuredUnixNS = "1792886525000000000", "1792886525000000000"
				}
				return fresh, nil
			})
			if age < 4900*time.Millisecond {
				require.NoError(t, err)
				require.Zero(t, calls)
			} else if age < 10*time.Second {
				if err != nil || calls == 0 {
					t.Fatal("MUTATION: admission-lease-refresh", age, calls, err)
				}
			} else {
				require.EqualError(t, err, "clock-lease-expired")
				require.Equal(t, 10*time.Second, now.Sub(time.Unix(0, 0)))
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

func TestLeaseFivePollValidity(t *testing.T) {
	o := liveObservation(100 * time.Millisecond)
	l, err := (Lease{}).Observe(o, 0, 300*time.Millisecond)
	require.NoError(t, err)
	for _, age := range []time.Duration{2100 * time.Millisecond, 4899 * time.Millisecond, 4900 * time.Millisecond} {
		v, err := l.At(age)
		require.NoError(t, err)
		r, err := RecordLease(o, int64(age))
		require.NoError(t, err)
		if v.Expired != (age >= 4900*time.Millisecond) || r.Expired != v.Expired {
			t.Fatal("MUTATION: lease-five-polls", age, v, r)
		}
	}
}

func TestLeaseRefreshTenPollWindow(t *testing.T) {
	now := time.Unix(0, 0)
	start := now
	first := liveObservation(5100 * time.Millisecond)
	calls := 0
	_, err := RefreshLease(context.Background(), first, func() time.Time { return now }, func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }, func(context.Context) (LeaseObservation, error) {
		calls++
		if now.Sub(start) < 4500*time.Millisecond {
			return first, nil
		}
		fresh := liveObservation(0)
		fresh.ReferenceUnixNS = "1792886525000000000"
		fresh.MeasuredUnixNS = fresh.ReferenceUnixNS
		return fresh, nil
	})
	if err != nil || calls == 0 || now.Sub(start) < 4*time.Second || now.Sub(start) > 10*time.Second {
		t.Fatal("MUTATION: lease-ten-poll-refresh", err, calls, now.Sub(start))
	}
}

func TestLeaseUnavailableReasonPreserved(t *testing.T) {
	for _, reason := range []string{"unavailable", "source-changed", "reference-changing", "tracking-invalid", "poll-invalid", "dispersion-invalid", "read-failed"} {
		data := []byte(`{"errorBoundNs":1,"rateErrorPpm":0,"measuredUnixNs":"1792886400000000000","lease":{"available":false,"reason":"` + reason + `"}}`)
		s, err := Parse(data)
		require.NoError(t, err)
		pub := PublicSample(s)
		if s.Lease == nil || s.Lease.Reason != reason || pub.Lease == nil || pub.Lease.Reason != reason || pub.RecordedLease != nil {
			t.Fatal("MUTATION: lease-unavailable-reason", reason, s.Lease, pub.Lease)
		}
		_, err = RecordLease(*pub.Lease, 1000000000)
		require.Error(t, err)
	}
}

func TestLeaseUnavailableReasonSanitized(t *testing.T) {
	for _, reason := range []string{"private-label", "read-failed"} {
		s := PublicSample(Sample{Lease: &LeaseObservation{Reason: reason, ResidualPPM: "private-label"}})
		raw, err := json.Marshal(s)
		require.NoError(t, err)
		if strings.Contains(string(raw), "private-label") {
			t.Fatal("MUTATION: lease-reason-sanitized", string(raw))
		}
	}
}
