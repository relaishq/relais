package relay

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Cleanup of one lost session must not stall media of an unrelated live call
// while the ownership store is slow.
func TestTenureCleanupDoesNotBlockOtherCallsMedia(t *testing.T) {
	owners := newGatedOwners()
	sys := startTestRelay(t, Config{Owners: owners})
	a := sys.worker(t, sessionA)
	b := sys.worker(t, sessionB)
	_ = b
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	ctx := context.Background()
	lost, err := owners.Memory.Get(ctx, sessionB)
	require.NoError(t, err)

	owners.block() // Redis slow/stalled for the relay's ownership read
	done := make(chan error, 1)
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() { done <- sys.relay.ForgetSessionTenure(cleanupCtx, lost) }()
	require.Eventually(t, func() bool { return owners.calls(sessionB) > 0 }, time.Second, time.Millisecond)

	start := time.Now()
	caller.send(t, media, sys.relay.PublicAddr())
	datagram, ok := receive(t, a.conn, 800*time.Millisecond)
	t.Logf("unrelated call media forwarded=%t after %s (len %d)", ok, time.Since(start), len(datagram))
	owners.unblock()
	<-done
	require.True(t, ok, "unrelated live call's media blocked behind ForgetSessionTenure's store read")
}
