package storage

import (
	"context"
	"errors"
	"net"
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
