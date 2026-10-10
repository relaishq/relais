package sessionstore

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func TestRedisOwnersNeedsNoSnapshotKey(t *testing.T) {
	full := testRedis(t)
	t.Setenv("RELAIS_SESSIONSTORE_KEY", "not-an-encryption-key")
	owners, err := NewRedisOwners(context.Background(), storage.RedisConfig{Addr: full.client.(*redis.Client).Options().Addr, Prefix: full.prefix})
	require.NoError(t, err)
	defer func() { require.NoError(t, owners.Close()) }()
	_, hasStateAPI := any(owners).(Store)
	require.False(t, hasStateAPI)
	require.Empty(t, owners.leases.keysByID, "owner reader has no decryption keys")
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:11111")
	b := netip.MustParseAddrPort("127.0.0.1:11112")
	lease, err := full.Claim(ctx, "owner-only", a, time.Minute)
	require.NoError(t, err)
	require.NoError(t, full.PutState(ctx, lease, []byte("encrypted state")))
	addr, err := owners.Owner(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, a, addr)
	lease, err = full.Transfer(ctx, lease, b, time.Minute)
	require.NoError(t, err)
	addr, err = owners.Owner(ctx, lease.SessionID)
	require.NoError(t, err)
	require.Equal(t, b, addr)
	_, err = full.Renew(ctx, lease, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	_, err = owners.Owner(ctx, lease.SessionID)
	require.ErrorIs(t, err, ErrNotFound)
}
