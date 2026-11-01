//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris || illumos

package metrics

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/rediswiretest"
	"github.com/stretchr/testify/require"
)

func TestRedisClientDiscardsClosedIdleConnectionWithoutRetry(t *testing.T) {
	for _, instrumented := range []bool{false, true} {
		name := "native"
		if instrumented {
			name = "instrumented"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			closeIdle := make(chan struct{})
			release := sync.OnceFunc(func() { close(closeIdle) })
			defer release()
			done := make(chan error, 1)
			go func() {
				for i := range 2 {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					reader := bufio.NewReader(conn)
					_, err = rediswiretest.Read(reader) // HELLO
					if err == nil {
						_, err = io.WriteString(conn, "-ERR unknown command HELLO\r\n")
					}
					if err == nil {
						_, err = rediswiretest.Read(reader) // PING
					}
					if err == nil {
						_, err = io.WriteString(conn, "+PONG\r\n")
					}
					if err == nil && i == 0 {
						<-closeIdle
					}
					_ = conn.Close()
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			connections := make(chan net.Conn, 2)
			options := &redis.Options{
				Addr: listener.Addr().String(), Protocol: 2, DisableIdentity: true,
				MaxRetries: -1, DialerRetries: 1, PoolSize: 1, PipelinePoolSize: -1,
				ContextTimeoutEnabled: true,
				Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err == nil {
						connections <- conn
					}
					return conn, err
				},
			}
			var client *redis.Client
			if instrumented {
				client = RedisClient(options, "idle-test")
			} else {
				client = redis.NewClient(options)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, client.Ping(ctx).Err())
			raw := <-connections
			release()
			// Observe the peer's FIN without consuming application data. The
			// socket stays in the idle pool; no timing sleep decides readiness.
			require.NoError(t, raw.SetReadDeadline(time.Now().Add(3*time.Second)))
			var b [1]byte
			n, err := raw.Read(b[:])
			require.Zero(t, n)
			require.ErrorIs(t, err, io.EOF)
			require.NoError(t, client.Ping(ctx).Err(), "discard the closed socket before sending, with command retries disabled")
			require.NoError(t, <-done)
			require.Len(t, connections, 1, "the second command must use a fresh connection")
		})
	}
}

func TestTrafficConnKeepsTLSOpaque(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	raw, err := net.Dial("tcp4", listener.Addr().String())
	require.NoError(t, err)
	defer raw.Close()
	peer, err := listener.Accept()
	require.NoError(t, err)
	defer peer.Close()
	_, ok := TrafficConn(raw, "test").(syscall.Conn)
	require.True(t, ok, "plain TCP retains its socket probe")
	encrypted := tls.Client(raw, &tls.Config{ServerName: "local.test", MinVersion: tls.VersionTLS12})
	_, ok = TrafficConn(encrypted, "test").(syscall.Conn)
	require.False(t, ok, "encrypted records must not be probed as plaintext")
}
