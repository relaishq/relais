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
	var mu sync.Mutex
	seen := false
	err := client.ForEachShard(ctx, func(ctx context.Context, node *redis.Client) error {
		info, err := node.ClusterInfo(ctx).Result()
		if err != nil {
			return err
		}
		if !strings.Contains(info, "cluster_state:ok\r\n") {
			return fmt.Errorf("node %s has not reached cluster_state:ok", node.Options().Addr)
		}
		shards, err := node.ClusterShards(ctx).Result()
		if err != nil {
			return err
		}
		var owners [16384]string
		for _, shard := range shards {
			primary, err := shardPrimaryAddress(shard, node.Options().TLSConfig != nil)
			if err != nil {
				return err
			}
			for _, slot := range shard.Slots {
				if slot.Start < 0 || slot.End >= int64(len(owners)) || slot.Start > slot.End {
					return fmt.Errorf("node %s returned invalid slots", node.Options().Addr)
				}
				for i := slot.Start; i <= slot.End; i++ {
					owners[i] = primary
				}
			}
		}
		for _, owner := range owners {
			if owner == "" {
				return fmt.Errorf("node %s has incomplete slots", node.Options().Addr)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if seen && owners != agreed {
			return fmt.Errorf("node %s disagrees on slot owners", node.Options().Addr)
		}
		agreed, seen = owners, true
		return nil
	})
	if err == nil && !seen {
		err = fmt.Errorf("cluster has no nodes")
	}
	return agreed, err
}

func shardPrimaryAddress(shard redis.ClusterShard, tls bool) (string, error) {
	for _, node := range shard.Nodes {
		if node.Role != "master" || node.Health != "online" {
			continue
		}
		host := node.Endpoint
		if host == "" || host == "?" {
			host = node.IP
		}
		port := node.Port
		if tls && node.TLSPort > 0 {
			port = node.TLSPort
		}
		if host == "" || port <= 0 {
			return "", fmt.Errorf("shard primary has no reachable endpoint")
		}
		return net.JoinHostPort(host, strconv.FormatInt(port, 10)), nil
	}
	return "", fmt.Errorf("shard has no online primary")
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
