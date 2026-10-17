package metrics

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

func TestRedisWireCommandsBinaryAndFragmented(t *testing.T) {
	var counter redisCommandCounter
	command := []byte("*2\r\n$3\r\nset\r\n$6\r\n*1\r\n\x00x\r\n")
	var total uint64
	for _, b := range append(command, []byte("*1\r\n$4\r\nping\r\n")...) {
		total += counter.feed([]byte{b})
	}
	require.EqualValues(t, 2, total)
	require.False(t, counter.invalid)
}

// The production Redis dial wrapper sees two wire commands even though the
// logical process hook runs once when go-redis retries a lost reply.
func TestRedisWireCommandsIncludeLibraryRetry(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			request := make([]byte, len("*1\r\n$4\r\nping\r\n"))
			_, err = io.ReadFull(conn, request)
			if err == nil && i == 1 {
				_, err = conn.Write([]byte("+PONG\r\n"))
			}
			_ = conn.Close()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	wire := storeWireCommands.WithLabelValues("retry-test")
	logical := storeCommands.WithLabelValues("retry-test")
	beforeWire, beforeLogical := counterValue(wire), counterValue(logical)
	client := RedisClient(&redis.Options{Addr: listener.Addr().String(), MaxRetries: 1, MinRetryBackoff: time.Millisecond, MaxRetryBackoff: time.Millisecond}, "retry-test")
	defer client.Close()
	client.AddHook(StoreHook("retry-test"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())
	require.NoError(t, <-done)
	require.Equal(t, 2.0, counterValue(wire)-beforeWire)
	require.Equal(t, 1.0, counterValue(logical)-beforeLogical)
}
