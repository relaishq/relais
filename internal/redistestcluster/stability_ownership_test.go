package redistestcluster

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestClusterWaitsForSlotOwnerWhenShardHasTwoPrimaries(t *testing.T) {
	addrs := Addresses(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin := redis.NewClusterClient(&redis.ClusterOptions{Addrs: addrs, MaxRedirects: -1, MaxRetries: -1})
	defer func() { _ = admin.Close() }()
	require.NoError(t, WaitForStable(ctx, admin))
	loadSlots := func(ctx context.Context) ([]redis.ClusterSlot, error) {
		cmd := redis.NewClusterSlotsCmd(ctx, "cluster", "slots")
		err := admin.Process(ctx, cmd)
		return cmd.Val(), err
	}
	slots, err := loadSlots(ctx)
	require.NoError(t, err)
	previous := slots[0].Nodes[0].ID
	key := clusterSlotKeys[slots[0].Start]
	// Keep the pre-promotion cache, then promote the real replica. The next
	// index read sent to the previous primary will now receive MOVED.
	require.NoError(t, Failover(ctx, admin, key))
	var corrected atomic.Bool
	opts := &redis.ClusterOptions{Addrs: addrs, MaxRedirects: -1, MaxRetries: -1}
	opts.ClusterSlots = func(ctx context.Context) ([]redis.ClusterSlot, error) {
		if corrected.Load() {
			return loadSlots(ctx)
		}
		return slots, nil
	}
	opts.NewClient = func(opts *redis.Options) *redis.Client {
		// A custom slot loader has no origin address; go-redis substitutes an
		// empty host for loopback endpoints. Keep the fixture's real address.
		if strings.HasPrefix(opts.Addr, ":") {
			copy := *opts
			copy.Addr = "127.0.0.1" + opts.Addr
			opts = &copy
		}
		node := redis.NewClient(opts)
		node.AddHook(twoPrimariesHook{previous: previous})
		return node
	}
	client := redis.NewClusterClient(opts)
	defer func() { _ = client.Close() }()
	blocked, stop := context.WithTimeout(ctx, time.Second)
	err = WaitForStable(blocked, client)
	stop()
	if err == nil {
		// The former barrier accepted the stale cache; a real command then MOVED.
		t.Logf("command after false readiness: %v", client.ZRangeWithScores(ctx, key, 0, -1).Err())
	}
	require.ErrorIs(t, err, context.DeadlineExceeded, "two master roles must not make a stale cache ready")
	corrected.Store(true)
	require.NoError(t, WaitForStable(ctx, client))
	require.NoError(t, client.ZRangeWithScores(ctx, key, 0, -1).Err())
}

// Valkey can report both the old and promoted nodes as online masters in one
// shard. SHARDS order does not identify which node actually owns its slots.
type twoPrimariesHook struct{ previous string }

func (h twoPrimariesHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h twoPrimariesHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h twoPrimariesHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if cmd, ok := cmd.(*redis.ClusterShardsCmd); ok {
			shards := cmd.Val()
			for i := range shards {
				for j := range shards[i].Nodes {
					if shards[i].Nodes[j].ID == h.previous {
						shards[i].Nodes[j].Role, shards[i].Nodes[j].Health = "master", "online"
						shards[i].Nodes[0], shards[i].Nodes[j] = shards[i].Nodes[j], shards[i].Nodes[0]
					}
				}
			}
			cmd.SetVal(shards)
		}
		return nil
	}
}
