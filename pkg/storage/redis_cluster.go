package storage

import (
	"context"
	"net"

	"github.com/redis/go-redis/v9"
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
	cluster.ReadOnly, cluster.RouteByLatency, cluster.RouteRandomly = false, false, false
	cluster.DisableRoutingPolicies = true
	cluster.NewClient = func(opts *redis.Options) *redis.Client {
		original := *opts
		if err := validate(original.Addr); err != nil {
			refusal := &RedisEndpointPolicyError{Err: err}
			original.Dialer = func(context.Context, string, string) (net.Conn, error) { return nil, refusal }
		}
		return redis.NewClient(&original)
	}
	return cluster
}
