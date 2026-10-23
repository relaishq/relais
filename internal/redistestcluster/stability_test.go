package redistestcluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestClusterWaitsForEveryClientSlotMap(t *testing.T) {
	addrs := Addresses(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin := redis.NewClusterClient(&redis.ClusterOptions{Addrs: addrs, MaxRedirects: -1, MaxRetries: -1})
	defer func() { _ = admin.Close() }()
	require.NoError(t, WaitForStable(ctx, admin))
	// The cache hook below models go-redis v9.23's SLOTS-based loader.
	slotCommand := redis.NewClusterSlotsCmd(ctx, "cluster", "slots")
	require.NoError(t, admin.Process(ctx, slotCommand))
	slots := slotCommand.Val()
	// Keep every slot except the last correctly routed. This models an
	// untouched index slot remaining stale after one session read succeeds.
	stale := make([]redis.ClusterSlot, 0, len(slots)+1)
	for _, slot := range slots {
		if slot.End != 16383 {
			stale = append(stale, slot)
			continue
		}
		last := slot
		slot.End--
		stale = append(stale, slot)
		last.Start = last.End
		last.Nodes = []redis.ClusterNode{{ID: "stale", Addr: "127.0.0.1:1"}}
		stale = append(stale, last)
	}
	var corrected atomic.Bool
	client := redis.NewClusterClient(&redis.ClusterOptions{Addrs: addrs, MaxRedirects: -1, MaxRetries: -1})
	client.OnNewNode(func(node *redis.Client) {
		node.AddHook(slotMapHook{stale: stale, corrected: &corrected})
	})
	defer func() { _ = client.Close() }()
	node, err := client.MasterForKey(ctx, clusterSlotKeys[16383])
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:1", node.Options().Addr)
	blocked, stop := context.WithTimeout(ctx, time.Second)
	err = WaitForStable(blocked, admin, client)
	stop()
	require.ErrorIs(t, err, context.DeadlineExceeded, "one stale client slot must prevent readiness")
	corrected.Store(true)
	require.NoError(t, WaitForStable(ctx, admin, client))
}

// Override only metadata responses, keeping the client's real slot cache and
// asynchronous reload path. Application commands are never intercepted.
type slotMapHook struct {
	stale     []redis.ClusterSlot
	corrected *atomic.Bool
}

func (h slotMapHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h slotMapHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h slotMapHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if slots, ok := cmd.(*redis.ClusterSlotsCmd); ok && err == nil && !h.corrected.Load() {
			slots.SetVal(h.stale)
		}
		return err
	}
}
