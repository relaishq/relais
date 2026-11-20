package storage

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"github.com/relaishq/relais/internal/redisendpoint"
)

// RedisEndpointPolicyError refuses a node before its transport opens a socket.
type RedisEndpointPolicyError struct{ Err error }

func (e *RedisEndpointPolicyError) Error() string {
	return "redis cluster endpoint policy: " + e.Err.Error()
}
func (e *RedisEndpointPolicyError) Unwrap() error { return e.Err }

// RedisClusterOptions uses native discovery and primary-only routing. Every
// address the client dials passes the seed endpoint policy. Native discovery
// replaces a remote source's loopback announcement with that source's host,
// so it cannot reach local Redis. A refused node fails closed for its slots.
func RedisClusterOptions(options *redis.UniversalOptions) *redis.ClusterOptions {
	return redisClusterOptions(options, redisendpoint.Validate)
}

func redisClusterOptions(options *redis.UniversalOptions, validate func(string) error) *redis.ClusterOptions {
	cluster := options.Cluster()
	cluster.Protocol, cluster.DisableIdentity = 2, true
	cluster.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
	// Preserve v8's refresh cadence instead of v9's 60-second default.
	cluster.ClusterStateReloadInterval = 10 * time.Second
	cluster.ReadOnly, cluster.RouteByLatency, cluster.RouteRandomly = false, false, false
	cluster.DisableRoutingPolicies = true
	cluster.NewClient = func(opts *redis.Options) *redis.Client {
		original := *opts
		original.Protocol = 2
		original.DisableIdentity = true
		original.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
		constructionErr := validate(original.Addr)
		dial := original.Dialer
		if dial == nil {
			dial = redis.NewDialer(&original)
		}
		original.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if constructionErr != nil {
				return nil, &RedisEndpointPolicyError{Err: constructionErr}
			}
			// Check the actual destination too, including future handoff paths.
			if err := validate(addr); err != nil {
				return nil, &RedisEndpointPolicyError{Err: err}
			}
			return dial(ctx, network, addr)
		}
		client := redis.NewClient(&original)
		client.AddHook(redisClusterSlotsHook{})
		return client
	}
	return cluster
}

// RefreshRedisClusterState reloads the client's slot map every
// ClusterStateReloadInterval. go-redis reloads only when a command finds the
// map stale, and that command still uses the stale map, so an idle client's
// first write after a primary failover goes to the dead primary. A failed dial
// also requests a reload, so a caller's bounded retry can reach the new
// primary. Call stop before closing the client; stop waits for the refresh
// goroutine to exit and is safe to call more than once.
func RefreshRedisClusterState(client *redis.ClusterClient) (stop func()) {
	client.AddHook(redisDialReloadHook{client: client})
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(client.Options().ClusterStateReloadInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				client.ReloadState(context.Background())
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); <-exited }) }
}

// IsRedisDialError reports a failed connection attempt. The driver opens a
// connection before it writes any command bytes, so the command did not run.
// TLS handshake failures and errors on an established connection use other
// operations and do not match.
func IsRedisDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

type redisDialReloadHook struct{ client *redis.ClusterClient }

func (redisDialReloadHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h redisDialReloadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if IsRedisDialError(err) {
			h.client.ReloadState(ctx)
		}
		return err
	}
}
func (h redisDialReloadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		// A pipeline returns its first error; a dial error to another node can
		// sit behind an ordinary command error.
		if IsRedisDialError(err) || slices.ContainsFunc(cmds, func(cmd redis.Cmder) bool { return IsRedisDialError(cmd.Err()) }) {
			h.client.ReloadState(ctx)
		}
		return err
	}
}

// An empty reply is a failed discovery source, not a replacement routing map.
// Native loading then tries the remaining sources and retains the prior map
// when none returns usable slots.
type redisClusterSlotsHook struct{}

func (redisClusterSlotsHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (redisClusterSlotsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if slots, ok := cmd.(*redis.ClusterSlotsCmd); ok && len(slots.Val()) == 0 {
			return errors.New("redis cluster discovery: empty CLUSTER SLOTS reply")
		}
		return nil
	}
}
func (redisClusterSlotsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
