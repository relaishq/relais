package sessionstore

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

func TestCheckpointClockFencingAndIdentity(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		a, b := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
		lease, err := store.Claim(ctx, "checkpoint", a, time.Minute)
		require.NoError(t, err)
		before, err := store.Clock(ctx, lease.SessionID)
		require.NoError(t, err)
		original := []byte("original")
		require.NoError(t, store.PutState(ctx, lease, original))
		first, err := store.Checkpoint(ctx, lease.SessionID, original)
		require.NoError(t, err)
		require.False(t, first.StoredAt.Before(before))
		require.False(t, first.Now.Before(first.StoredAt))
		time.Sleep(15 * time.Millisecond)
		next, err := store.Transfer(ctx, lease, b, time.Minute)
		require.NoError(t, err)
		require.ErrorIs(t, store.PutState(ctx, lease, []byte("stale")), ErrLeaseLost)
		second, err := store.Checkpoint(ctx, lease.SessionID, original)
		require.NoError(t, err)
		require.True(t, first.StoredAt.Equal(second.StoredAt), "transfer and failed put never refresh a checkpoint")
		require.GreaterOrEqual(t, second.Age, 10*time.Millisecond)
		require.NoError(t, store.PutState(ctx, next, []byte("newer")))
		_, err = store.Checkpoint(ctx, lease.SessionID, original)
		require.ErrorIs(t, err, ErrStateSuperseded, "newer checkpoint cannot date older bytes")
		require.NoError(t, store.Release(ctx, next))
		_, err = store.Checkpoint(ctx, lease.SessionID, []byte("newer"))
		require.ErrorIs(t, err, ErrNotFound)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = store.Clock(canceled, lease.SessionID)
		require.ErrorIs(t, err, context.Canceled)
		_, err = store.Checkpoint(canceled, lease.SessionID, original)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestCheckpointDigestHidesPlaintextFingerprint(t *testing.T) {
	r := testRedis(t)
	ctx := context.Background()
	data := []byte("predictable snapshot")
	lease, err := r.Claim(ctx, "hmac", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.PutState(ctx, lease, data))
	digest, err := r.client.HGet(ctx, r.keys(lease.SessionID)[0], "state_digest").Result()
	require.NoError(t, err)
	expected, err := r.stateDigest(lease.SessionID, r.activeKey, data)
	require.NoError(t, err)
	require.Equal(t, expected, digest)
	require.NotContains(t, digest, fmt.Sprintf("%x", sha256.Sum256(data)))
	differentSession, err := r.stateDigest("another-session", r.activeKey, data)
	require.NoError(t, err)
	require.NotEqual(t, differentSession, digest)
	other := &Redis{keysByID: map[byte][]byte{r.activeKey: []byte("different master key")}}
	differentKey, err := other.stateDigest(lease.SessionID, r.activeKey, data)
	require.NoError(t, err)
	require.NotEqual(t, differentKey, digest)
}

type clockRoutingClient struct {
	redis.UniversalClient
	keys []string
}

func (c *clockRoutingClient) Time(context.Context) *redis.TimeCmd {
	return redis.NewTimeCmdResult(time.Unix(123, 0), nil)
}

func (c *clockRoutingClient) Eval(_ context.Context, _ string, keys []string, _ ...interface{}) *redis.Cmd {
	c.keys = keys
	return redis.NewCmdResult([]interface{}{int64(123), int64(456789)}, nil)
}

func TestCheckpointRedisClockRoutesToSessionSlot(t *testing.T) {
	client := &clockRoutingClient{}
	store := &Redis{client: client, prefix: "clock:"}
	_, err := store.Clock(context.Background(), "clock-session")
	require.NoError(t, err)
	require.Equal(t, store.keys("clock-session"), client.keys, "TIME must route to the node storing this session")
}

func TestCheckpointClockRegressionIsUnsafe(t *testing.T) {
	forStores(t, func(t *testing.T, factory func(*testing.T) Store) {
		store := factory(t)
		ctx := context.Background()
		lease, err := store.Claim(ctx, "clock", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
		require.NoError(t, err)
		data := []byte("checkpoint")
		require.NoError(t, store.PutState(ctx, lease, data))
		future := time.Now().Add(time.Hour)
		switch s := store.(type) {
		case *Memory:
			s.mu.Lock()
			checkpoint := s.checkpoints[lease.SessionID]
			checkpoint.StoredAt = future
			s.checkpoints[lease.SessionID] = checkpoint
			s.mu.Unlock()
		case *Redis:
			require.NoError(t, s.client.HSet(ctx, s.keys(lease.SessionID)[0], "checkpoint_at", strconv.FormatInt(future.UnixMilli(), 10)).Err())
		}
		_, err = store.Checkpoint(ctx, lease.SessionID, data)
		require.ErrorIs(t, err, ErrUnsafeCheckpointClock)
	})
}

func TestMemoryCheckpointExpiryForgetsRoutes(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	lease, err := store.Claim(ctx, "checkpoint-expiry", netip.MustParseAddrPort("127.0.0.1:1"), time.Minute)
	require.NoError(t, err)
	data := []byte("checkpoint")
	require.NoError(t, store.PutState(ctx, lease, data))
	route := routeFor(lease, netip.MustParseAddrPort("127.0.0.1:2"), time.Minute)
	require.NoError(t, store.PutRoute(ctx, route, false))
	store.mu.Lock()
	lease.ExpiresAt = time.Now().Add(-time.Second)
	store.leases[lease.SessionID] = lease
	store.mu.Unlock()
	_, err = store.Checkpoint(ctx, lease.SessionID, data)
	require.ErrorIs(t, err, ErrNotFound)
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Empty(t, store.routes, "checkpoint expiry must remove the caller address index")
	require.Empty(t, store.routeSessions, "checkpoint expiry must remove the session route index")
	require.Empty(t, store.leases)
	require.Empty(t, store.states)
	require.Empty(t, store.checkpoints)
}

func TestCheckpointDigestUsesDistinctDerivedKey(t *testing.T) {
	master := []byte("01234567890123456789012345678901")
	store := &Redis{keysByID: map[byte][]byte{7: master}}
	key, err := hkdf.Key(sha256.New, master, nil, "relais/sessionstore/checkpoint-digest/v1/session", 32)
	require.NoError(t, err)
	encryptionKey, err := hkdf.Key(sha256.New, master, nil, "relais/sessionstore/v1/session", 32)
	require.NoError(t, err)
	require.NotEqual(t, encryptionKey, key)
	require.NotEqual(t, master, key)
	mac := hmac.New(sha256.New, key)
	mac.Write(stateAAD("session", "checkpoint-digest-v1", []byte{7}))
	mac.Write([]byte("state"))
	digest, err := store.stateDigest("session", 7, []byte("state"))
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("7:%x", mac.Sum(nil)), digest)
	_, err = store.stateDigest("session", 8, []byte("state"))
	require.Error(t, err)
}
