package sessionstore

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryOwners(t *testing.T) {
	ctx := context.Background()
	workerA := netip.MustParseAddrPort("127.0.0.1:4001")
	workerB := netip.MustParseAddrPort("127.0.0.1:4002")
	store := NewMemory()

	_, err := store.Owner(ctx, "s1")
	require.ErrorIs(t, err, ErrNotFound, "unknown session")

	require.NoError(t, store.Claim(ctx, "s1", workerA))
	owner, err := store.Owner(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, workerA, owner)

	// A new claim replaces the owner, and the old owner can no longer
	// release the session.
	require.NoError(t, store.Claim(ctx, "s1", workerB))
	require.NoError(t, store.Release(ctx, "s1", workerA))
	owner, err = store.Owner(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, workerB, owner, "release by a former owner is ignored")

	require.NoError(t, store.Release(ctx, "s1", workerB))
	_, err = store.Owner(ctx, "s1")
	require.ErrorIs(t, err, ErrNotFound, "released session")

	require.NoError(t, store.Release(ctx, "never-claimed", workerA), "releasing an unknown session")
	require.Error(t, store.Claim(ctx, "", workerA), "claim without a session ID")
	require.Error(t, store.Claim(ctx, "s2", netip.AddrPort{}), "claim without a worker")
}
