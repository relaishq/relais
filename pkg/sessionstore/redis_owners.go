package sessionstore

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/relais/pkg/storage"
)

// RedisOwners exposes live leases, route metadata and connection lifecycle.
// It holds no snapshot encryption keys and has no snapshot read/write API.
// Lookups share Redis's fenced lease/expiry semantics, including expiry pruning.
type RedisOwners struct{ leases *Redis }

func NewRedisOwners(ctx context.Context, cfg storage.RedisConfig) (*RedisOwners, error) {
	if strings.TrimSpace(cfg.Addr) == "" && len(cfg.Addrs) == 0 {
		return nil, errors.New("sessionstore: Redis address required")
	}
	for _, addr := range cfg.Addrs {
		if strings.TrimSpace(addr) == "" {
			return nil, errors.New("sessionstore: empty Redis cluster address")
		}
	}
	if strings.ContainsAny(cfg.Prefix, "{}") {
		return nil, errors.New("sessionstore: prefix cannot contain braces")
	}
	client := connectRedis(cfg)
	leases := &Redis{client: client, prefix: cfg.Prefix, retention: 5 * time.Minute}
	if err := leases.retryRead(ctx, "session_ping", func() error { return client.Ping(ctx).Err() }); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &RedisOwners{leases: leases}, nil
}
func (r *RedisOwners) Owner(ctx context.Context, id string) (netip.AddrPort, error) {
	return r.leases.Owner(ctx, id)
}
func (r *RedisOwners) Close() error { return r.leases.Close() }
