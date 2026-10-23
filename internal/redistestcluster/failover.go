package redistestcluster

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redisendpoint"
)

// Failover coordinates a no-write-loss promotion of the replica for key's slot.
// It flushes that replica's script cache first to exercise NOSCRIPT after promotion.
// The supplied client is for test administration, never a process's own client.
// This is protocol/reconnect evidence, not asynchronous Redis durability evidence.
func Failover(ctx context.Context, client *redis.ClusterClient, key string) error {
	primary, err := client.MasterForKey(ctx, key)
	if err != nil {
		return err
	}
	id, err := primary.Do(ctx, "CLUSTER", "MYID").Text()
	if err != nil {
		return err
	}
	rows, err := primary.Do(ctx, "CLUSTER", "REPLICAS", id).StringSlice()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("slot primary has no replica")
	}
	fields := strings.Fields(rows[0])
	if len(fields) < 3 {
		return fmt.Errorf("invalid replica metadata")
	}
	addr, _, _ := strings.Cut(fields[1], "@")
	addr, _, _ = strings.Cut(addr, ",")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return err
	}
	if err := redisendpoint.ValidateDev(addr); err != nil {
		return err
	}
	opts := *primary.Options()
	opts.Addr, opts.MaxRetries = addr, -1
	replica := redis.NewClient(&opts)
	defer func() { _ = replica.Close() }()
	// WAIT is only setup coordination, not a production durability setting.
	if n, err := primary.Wait(ctx, 1, 3*time.Second).Result(); err != nil {
		return err
	} else if n < 1 {
		return fmt.Errorf("replica did not catch up")
	}
	if err := replica.ScriptFlush(ctx).Err(); err != nil {
		return err
	}
	if err := replica.Do(ctx, "CLUSTER", "FAILOVER").Err(); err != nil {
		return err
	}
	for {
		info, err := replica.Info(ctx, "replication").Result()
		if err == nil && strings.Contains(info, "role:master") {
			return WaitForStable(ctx, client)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
