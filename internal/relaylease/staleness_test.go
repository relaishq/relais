package relaylease

import (
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestStalenessRequiresFullTTLOfSuccessfulPolls(t *testing.T) {
	var s staleness
	now := time.Now()
	lease := sessionstore.RelayLease{Holder: process("a"), Epoch: 1, ExpiresAt: now.Add(DefaultTTL)}
	for elapsed := time.Duration(0); elapsed < DefaultTTL; elapsed += DefaultPoll {
		require.False(t, s.observe(lease, false, nil, now.Add(elapsed), DefaultTTL, DefaultRenew))
	}
	require.True(t, s.observe(lease, false, nil, now.Add(DefaultTTL), DefaultTTL, DefaultRenew))
}
func TestStalenessFailedPollResetsObservation(t *testing.T) {
	var s staleness
	now := time.Now()
	lease := sessionstore.RelayLease{Holder: process("a"), Epoch: 1, ExpiresAt: now.Add(DefaultTTL)}
	require.False(t, s.observe(lease, false, nil, now, DefaultTTL, DefaultRenew))
	require.False(t, s.observe(lease, false, nil, now.Add(500*time.Millisecond), DefaultTTL, DefaultRenew))
	require.False(t, s.observe(sessionstore.RelayLease{}, false, sessionstore.ErrTransient, now.Add(600*time.Millisecond), DefaultTTL, DefaultRenew))
	// A long outage does not count as reachable staleness.
	returned := now.Add(30 * time.Second)
	require.False(t, s.observe(lease, false, nil, returned, DefaultTTL, DefaultRenew))
	require.False(t, s.observe(lease, false, nil, returned.Add(500*time.Millisecond), DefaultTTL, DefaultRenew))
	require.True(t, s.observe(lease, false, nil, returned.Add(DefaultTTL), DefaultTTL, DefaultRenew))
}
func TestStalenessRenewalOrSuccessorResetsObservation(t *testing.T) {
	for _, kind := range []string{"renewal", "successor", "forwarder"} {
		t.Run(kind, func(t *testing.T) {
			var s staleness
			now := time.Now()
			lease := sessionstore.RelayLease{Holder: process("a"), Epoch: 1, ExpiresAt: now.Add(DefaultTTL)}
			require.False(t, s.observe(lease, false, nil, now, DefaultTTL, DefaultRenew))
			changed := lease
			switch kind {
			case "renewal":
				changed.ExpiresAt = lease.ExpiresAt.Add(200 * time.Millisecond)
			case "successor":
				changed.Holder = process("b")
				changed.Epoch++
			case "forwarder":
				changed.Forwarder = process("b")
			}
			renewedAt := now.Add(500 * time.Millisecond)
			require.False(t, s.observe(changed, false, nil, renewedAt, DefaultTTL, DefaultRenew))
			require.False(t, s.observe(changed, false, nil, now.Add(DefaultTTL), DefaultTTL, DefaultRenew))
			require.True(t, s.observe(changed, false, nil, renewedAt.Add(DefaultTTL), DefaultTTL, DefaultRenew))
		})
	}
}

func TestStalenessShortBlipPreservesOnlySuccessfulIntervals(t *testing.T) {
	var s staleness
	now := time.Now()
	lease := sessionstore.RelayLease{Holder: process("a"), Epoch: 1, ExpiresAt: now.Add(DefaultTTL)}
	require.False(t, s.observe(lease, false, nil, now, DefaultTTL, DefaultRenew))
	require.False(t, s.observe(lease, false, nil, now.Add(400*time.Millisecond), DefaultTTL, DefaultRenew))
	require.False(t, s.observe(lease, false, sessionstore.ErrTransient, now.Add(500*time.Millisecond), DefaultTTL, DefaultRenew))
	require.False(t, s.observe(lease, false, nil, now.Add(600*time.Millisecond), DefaultTTL, DefaultRenew))
	require.False(t, s.observe(lease, false, nil, now.Add(700*time.Millisecond), DefaultTTL, DefaultRenew))
	require.True(t, s.observe(lease, false, nil, now.Add(800*time.Millisecond), DefaultTTL, DefaultRenew))
}
func TestStalenessFailureThresholdUsesConfiguredRenew(t *testing.T) {
	for _, renew := range []time.Duration{150 * time.Millisecond, 300 * time.Millisecond} {
		var s staleness
		now := time.Now()
		lease := sessionstore.RelayLease{Holder: process("a"), Epoch: 1, ExpiresAt: now.Add(DefaultTTL)}
		require.False(t, s.observe(lease, false, nil, now, DefaultTTL, renew))
		require.False(t, s.observe(lease, false, nil, now.Add(500*time.Millisecond), DefaultTTL, renew))
		require.False(t, s.observe(lease, false, sessionstore.ErrTransient, now.Add(600*time.Millisecond), DefaultTTL, renew))
		require.False(t, s.observe(lease, false, nil, now.Add(800*time.Millisecond), DefaultTTL, renew))
		eligible := s.observe(lease, false, nil, now.Add(900*time.Millisecond), DefaultTTL, renew)
		require.Equal(t, renew > 200*time.Millisecond, eligible)
	}
}
