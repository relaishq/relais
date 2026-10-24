package redistestcluster

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redisendpoint"
)

// WaitForStable waits for every node to agree on all slot owners, then waits
// for each supplied test client's cached routes to match. It uses metadata
// only: no application script is retried or warmed before its assertion.
func WaitForStable(ctx context.Context, clients ...*redis.ClusterClient) error {
	if len(clients) == 0 {
		return fmt.Errorf("cluster stability requires a client")
	}
	var last error
	for {
		owners, err := agreedSlotOwners(ctx, clients[0])
		if err == nil {
			for _, client := range clients {
				client.ReloadState(ctx) // asynchronous; verify the cached map below
				for slot, key := range clusterSlotKeys {
					node, routeErr := client.MasterForKey(ctx, key)
					if routeErr != nil {
						err = routeErr
						break
					}
					if node.Options().Addr != owners[slot] {
						err = fmt.Errorf("slot %d cached at %s, want %s", slot, node.Options().Addr, owners[slot])
						break
					}
				}
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster slot agreement: %w (last observation: %v)", ctx.Err(), last)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func agreedSlotOwners(ctx context.Context, client *redis.ClusterClient) ([16384]string, error) {
	var agreed [16384]string
	// Valkey's SLOTS omits replicas with replication offset 0.
	// In particular, a newly promoted replica can be missing while every
	// sampled node still agrees on the previous epoch.
	// Expand membership from SHARDS before declaring agreement.
	var nodes []*redis.Client
	known := map[string]bool{}
	var mu sync.Mutex
	err := client.ForEachShard(ctx, func(_ context.Context, node *redis.Client) error {
		mu.Lock()
		defer mu.Unlock()
		known[node.Options().Addr] = true
		nodes = append(nodes, node)
		return nil
	})
	if err != nil {
		return agreed, err
	}
	var opened []*redis.Client
	defer func() {
		for _, node := range opened {
			_ = node.Close()
		}
	}()
	seen := false
	for i := 0; i < len(nodes); i++ {
		node := nodes[i]
		info, err := node.ClusterInfo(ctx).Result()
		if err != nil {
			return agreed, err
		}
		if !strings.Contains(info, "cluster_state:ok\r\n") {
			return agreed, fmt.Errorf("node %s has not reached cluster_state:ok", node.Options().Addr)
		}
		shards, err := node.ClusterShards(ctx).Result()
		if err != nil {
			return agreed, err
		}
		// SHARDS identifies members, but its node roles are not slot ownership:
		// Valkey can list both old and promoted nodes as online masters in one
		// shard during failover. SLOTS identifies the owner explicitly and is
		// also the metadata go-redis uses to load each client's routing cache.
		slotCommand := redis.NewClusterSlotsCmd(ctx, "cluster", "slots")
		err = node.Process(ctx, slotCommand)
		slots := slotCommand.Val()
		if err != nil {
			return agreed, err
		}
		var owners [16384]string
		for _, slot := range slots {
			if slot.Start < 0 || slot.End >= len(owners) || slot.Start > slot.End || len(slot.Nodes) == 0 {
				return agreed, fmt.Errorf("node %s returned invalid slots", node.Options().Addr)
			}
			for i := slot.Start; i <= slot.End; i++ {
				owners[i] = slot.Nodes[0].Addr
			}
		}
		for _, owner := range owners {
			if owner == "" {
				return agreed, fmt.Errorf("node %s has incomplete slots", node.Options().Addr)
			}
		}
		if seen && owners != agreed {
			return agreed, fmt.Errorf("node %s disagrees on slot owners", node.Options().Addr)
		}
		agreed, seen = owners, true
		for _, shard := range shards {
			for _, member := range shard.Nodes {
				if member.Health == "fail" {
					continue
				}
				addr, err := shardNodeAddress(member, node.Options().TLSConfig != nil)
				if err != nil {
					return agreed, err
				}
				if known[addr] {
					continue
				}
				if err := redisendpoint.ValidateDev(addr); err != nil {
					return agreed, err
				}
				opts := *node.Options()
				opts.Addr, opts.MaxRetries = addr, -1
				discovered := client.Options().NewClient(&opts)
				known[addr] = true
				nodes = append(nodes, discovered)
				opened = append(opened, discovered)
			}
		}
	}
	if !seen {
		return agreed, fmt.Errorf("cluster has no nodes")
	}
	return agreed, nil
}

func shardNodeAddress(node redis.Node, tls bool) (string, error) {
	host := node.Endpoint
	if host == "" || host == "?" {
		host = node.IP
	}
	port := node.Port
	if tls && node.TLSPort > 0 {
		port = node.TLSPort
	}
	if host == "" || port <= 0 {
		return "", fmt.Errorf("shard node has no reachable endpoint")
	}
	return net.JoinHostPort(host, strconv.FormatInt(port, 10)), nil
}

// One harmless routing key per slot lets metadata checks cover indexes and
// relay keys too, including slots for which no session has been created.
var clusterSlotKeys = func() [16384]string {
	var keys [16384]string
	remaining := len(keys)
	for i := 0; remaining > 0; i++ {
		key := fmt.Sprintf("slot-map-%d", i)
		slot := Slot(key)
		if keys[slot] == "" {
			keys[slot] = key
			remaining--
		}
	}
	return keys
}()
