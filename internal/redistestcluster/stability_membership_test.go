package redistestcluster

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestClusterWaitsForReplicaMissingFromSlotDiscovery(t *testing.T) {
	addrs := Addresses(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin := redis.NewClusterClient(&redis.ClusterOptions{Addrs: addrs, MaxRedirects: -1, MaxRetries: -1})
	defer func() { _ = admin.Close() }()
	require.NoError(t, WaitForStable(ctx, admin))
	shards, err := admin.ClusterShards(ctx).Result()
	require.NoError(t, err)
	missing := ""
	for _, shard := range shards {
		for _, node := range shard.Nodes {
			if node.Role == "replica" {
				missing = net.JoinHostPort(node.IP, strconv.FormatInt(node.Port, 10))
				break
			}
		}
		if missing != "" {
			break
		}
	}
	require.NotEmpty(t, missing)
	var corrected, observed atomic.Bool
	// Valkey omits zero-offset replicas from SLOTS, while SHARDS still lists
	// them. Model that discovery gap and a node that has not applied the epoch.
	opts := &redis.ClusterOptions{Addrs: addrs, MaxRedirects: -1, MaxRetries: -1}
	opts.NewClient = func(opts *redis.Options) *redis.Client {
		node := redis.NewClient(opts)
		node.AddHook(incompleteMembershipHook{address: opts.Addr, missing: missing, corrected: &corrected, observed: &observed})
		return node
	}
	client := redis.NewClusterClient(opts)
	defer func() { _ = client.Close() }()
	blocked, stop := context.WithTimeout(ctx, time.Second)
	err = WaitForStable(blocked, client)
	stop()
	require.ErrorIs(t, err, context.DeadlineExceeded, "a replica absent from SLOTS must still participate in slot agreement")
	require.True(t, observed.Load(), "readiness must sample the node hidden from SLOTS")
	corrected.Store(true)
	require.NoError(t, WaitForStable(ctx, client))
}

type incompleteMembershipHook struct {
	address, missing    string
	corrected, observed *atomic.Bool
}

func (h incompleteMembershipHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h incompleteMembershipHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h incompleteMembershipHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		switch cmd := cmd.(type) {
		case *redis.ClusterSlotsCmd:
			slots := cmd.Val()
			for i := range slots {
				nodes := make([]redis.ClusterNode, 0, len(slots[i].Nodes))
				for j, node := range slots[i].Nodes {
					if j == 0 || node.Addr != h.missing {
						nodes = append(nodes, node)
					}
				}
				slots[i].Nodes = nodes
			}
			cmd.SetVal(slots)
		case *redis.ClusterShardsCmd:
			if h.address == h.missing && !h.corrected.Load() {
				h.observed.Store(true)
				shards := cmd.Val()
				for i := range shards {
					for j := range shards[i].Nodes {
						if shards[i].Nodes[j].Role == "master" {
							// Keep membership and coverage intact, but disagree on this owner.
							shards[i].Nodes[j].Port++
							shards[i].Nodes[j].TLSPort++
						}
					}
				}
				cmd.SetVal(shards)
			}
		}
		return nil
	}
}
