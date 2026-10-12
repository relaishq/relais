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
		require.False(t, s.observe(lease, false, nil, now.Add(elapsed), DefaultTTL))
	}
	require.True(t, s.observe(lease, false, nil, now.Add(DefaultTTL), DefaultTTL))
}
func TestStalenessFailedPollResetsObservation(t *testing.T) {
	var s staleness
	now := time.Now()
	lease := sessionstore.RelayLease{Holder: process("a"), Epoch: 1, ExpiresAt: now.Add(DefaultTTL)}
	require.False(t, s.observe(lease, false, nil, now, DefaultTTL))
	require.False(t, s.observe(lease, false, nil, now.Add(500*time.Millisecond), DefaultTTL))
	require.False(t, s.observe(sessionstore.RelayLease{}, false, sessionstore.ErrTransient, now.Add(600*time.Millisecond), DefaultTTL))
	// A long outage does not count as reachable staleness.
	returned := now.Add(30 * time.Second)
	require.False(t, s.observe(lease, false, nil, returned, DefaultTTL))
	require.False(t, s.observe(lease, false, nil, returned.Add(500*time.Millisecond), DefaultTTL))
	require.True(t, s.observe(lease, false, nil, returned.Add(DefaultTTL), DefaultTTL))
}
func TestStalenessRenewalOrSuccessorResetsObservation(t *testing.T) {
	for _, kind := range []string{"renewal", "successor", "forwarder"} {
		t.Run(kind, func(t *testing.T) {
			var s staleness
			now := time.Now()
			lease := sessionstore.RelayLease{Holder: process("a"), Epoch: 1, ExpiresAt: now.Add(DefaultTTL)}
			require.False(t, s.observe(lease, false, nil, now, DefaultTTL))
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
			require.False(t, s.observe(changed, false, nil, renewedAt, DefaultTTL))
			require.False(t, s.observe(changed, false, nil, now.Add(DefaultTTL), DefaultTTL))
			require.True(t, s.observe(changed, false, nil, renewedAt.Add(DefaultTTL), DefaultTTL))
		})
	}
}
