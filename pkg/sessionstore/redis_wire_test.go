package sessionstore

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/redistestcluster"
	"github.com/relaishq/relais/internal/rediswiretest"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type scriptReplyFault struct {
	attempts atomic.Int64
	armed    atomic.Bool
}

func (f *scriptReplyFault) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := next(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &lostScriptReplyConn{Conn: conn, fault: f}, nil
	}
}
func (*scriptReplyFault) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (*scriptReplyFault) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type lostScriptReplyConn struct {
	net.Conn
	fault   *scriptReplyFault
	mu      sync.Mutex
	pending []byte
	drop    bool
}

func (c *lostScriptReplyConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, p[:n]...)
	for len(c.pending) > 0 {
		reader := bufio.NewReader(bytes.NewReader(c.pending))
		raw, parseErr := rediswiretest.Read(reader)
		if parseErr != nil {
			break
		}
		c.pending = c.pending[len(raw):]
		args, parseErr := rediswiretest.Command(raw)
		if parseErr != nil || len(args) < 4 || !c.fault.armed.Load() {
			continue
		}
		if args[0] != "evalsha" && args[0] != "eval" {
			continue
		}
		keys, parseErr := strconv.Atoi(args[2])
		if parseErr != nil || 3+keys >= len(args) {
			continue
		}
		if args[3+keys] == "renew" {
			c.fault.attempts.Add(1)
			c.drop = true
		}
	}
	return n, err
}
func (c *lostScriptReplyConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	drop := c.drop
	c.mu.Unlock()
	if drop && n > 0 {
		_ = c.Close()
		return 0, io.EOF
	} // Server executed before producing its reply.
	return n, err
}

func TestRedisClusterFencedScriptLostReplyNotResent(t *testing.T) {
	for _, mode := range []string{"standalone", "cluster"} {
		t.Run(mode, func(t *testing.T) {
			var r *Redis
			if mode == "standalone" {
				var err error
				r, err = NewRedis(context.Background(), storage.RedisConfig{Addr: redistestcluster.Standalone(t), Prefix: "wire:"}, make([]byte, 32))
				require.NoError(t, err)
				t.Cleanup(func() { r.maintenance.Wait(); require.NoError(t, r.Close()) })
			} else {
				r = componentCluster(t, redistestcluster.Addresses(t))
			}
			ctx := context.Background()
			lease, err := r.Claim(ctx, "wire-lost-renew", netip.MustParseAddrPort("127.0.0.1:23456"), time.Minute)
			require.NoError(t, err)
			r.maintenance.Wait()
			fault := &scriptReplyFault{}
			install := func(ctx context.Context, node *redis.Client) error {
				node.AddHook(fault)
				// Only the fixture's disposable servers are used. Force a fresh connection
				// through the hook without injecting an error outside the driver's retries.
				opts := *node.Options()
				opts.MaxRetries = -1
				admin := redis.NewClient(&opts)
				defer admin.Close()
				if err := admin.Do(ctx, "CLIENT", "KILL", "TYPE", "normal", "SKIPME", "yes").Err(); err != nil {
					return err
				}
				warm, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				for {
					if err := node.Ping(warm).Err(); err == nil {
						return nil
					}
					if warm.Err() != nil {
						return warm.Err()
					}
				}
			}
			switch client := r.client.(type) {
			case *redis.Client:
				require.NoError(t, install(ctx, client))
			case *redis.ClusterClient:
				require.NoError(t, client.ForEachShard(ctx, install))
			default:
				t.Fatal("unsupported test client")
			}
			_, err = r.Get(ctx, lease.SessionID) // Reconnect after CLIENT KILL before arming the wire fault.
			require.NoError(t, err)
			fault.armed.Store(true)
			_, err = r.Renew(ctx, lease, time.Minute)
			require.ErrorIs(t, err, ErrTransient)
			require.EqualValues(t, 1, fault.attempts.Load(), "possibly-executed fenced script must never be sent again")
			fault.armed.Store(false)
			current, err := r.Get(ctx, lease.SessionID)
			require.NoError(t, err)
			require.Equal(t, lease.Epoch, current.Epoch)
			require.Equal(t, lease.Worker, current.Worker)
			require.False(t, current.ExpiresAt.Before(lease.ExpiresAt), "the server committed the renewal")
		})
	}
}
