package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/internal/rediswiretest"

	"github.com/redis/go-redis/v9"
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

// The wire counter includes two PING writes and both connection handshakes.
// Logical store work and move diagnostics record one PING across the lost reply.
func TestRedisWireCommandsIncludeLibraryRetry(t *testing.T) {
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
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
			reader := bufio.NewReader(conn)
			request, err := rediswiretest.Read(reader)
			if err == nil {
				_, err = io.WriteString(conn, "-ERR unknown command HELLO\r\n")
			}
			if err == nil {
				request, err = rediswiretest.Read(reader)
			}
			if err == nil {
				var args []string
				args, err = rediswiretest.Command(request)
				if err == nil && !strings.EqualFold(args[0], "auth") {
					err = fmt.Errorf("expected AUTH fallback")
				}
				if err == nil {
					_, err = io.WriteString(conn, "+OK\r\n")
				}
				if err == nil {
					request, err = rediswiretest.Read(reader)
				}
			}
			if err == nil && i == 1 {
				_, err = conn.Write([]byte("+PONG\r\n"))
			}
			_ = request

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
	client := RedisClient(&redis.Options{ContextTimeoutEnabled: true, PipelinePoolSize: -1, Password: "owned-password", Addr: listener.Addr().String(), Protocol: 2, DisableIdentity: true, DialerRetries: 1, MaxRetries: 1, MinRetryBackoff: time.Millisecond, MaxRetryBackoff: time.Millisecond}, "retry-test")
	defer client.Close()
	client.AddHook(StoreHook("retry-test"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = movediag.Begin(ctx)
	require.NoError(t, client.Ping(ctx).Err())
	require.NoError(t, <-done)
	require.Equal(t, 6.0, counterValue(wire)-beforeWire, "two HELLO, AUTH and PING wire attempts")
	require.Equal(t, 1.0, counterValue(logical)-beforeLogical, "one logical PING; handshake and wire retries are separate")
	var operations []string
	for _, event := range movediag.Default.Snapshot().Events {
		if event.TraceID == movediag.ID(ctx) {
			operations = append(operations, event.Operation)
		}
	}
	require.Equal(t, []string{"PING"}, operations)
}

func TestRedisDatabaseSelectionExcludesConnectionHandshake(t *testing.T) {
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		for _, step := range []struct{ name, argument, reply string }{
			{"hello", "2", "-ERR unknown command HELLO\r\n"},
			{"select", "1", "+OK\r\n"},
			{"ping", "", "+PONG\r\n"},
		} {
			raw, err := rediswiretest.Read(reader)
			if err != nil {
				done <- err
				return
			}
			args, err := rediswiretest.Command(raw)
			if err != nil {
				done <- err
				return
			}
			if len(args) == 0 || !strings.EqualFold(args[0], step.name) || (step.argument != "" && (len(args) < 2 || args[1] != step.argument)) {
				done <- fmt.Errorf("expected %s %s, got %v", step.name, step.argument, args)
				return
			}
			if _, err := io.WriteString(conn, step.reply); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	logical, wire := storeCommands.WithLabelValues("database-test"), storeWireCommands.WithLabelValues("database-test")
	beforeLogical, beforeWire := counterValue(logical), counterValue(wire)
	client := RedisClient(&redis.Options{Addr: listener.Addr().String(), DB: 1, MaxRetries: -1, DialerRetries: 1, PipelinePoolSize: -1, ContextTimeoutEnabled: true}, "database-test")
	defer client.Close()
	client.AddHook(StoreHook("database-test"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = movediag.Begin(ctx)
	require.NoError(t, client.Ping(ctx).Err())
	require.NoError(t, <-done)
	require.Equal(t, 3.0, counterValue(wire)-beforeWire, "HELLO, SELECT and PING were all sent")
	require.Equal(t, 1.0, counterValue(logical)-beforeLogical, "only PING is logical store work")
	var operations []string
	for _, event := range movediag.Default.Snapshot().Events {
		if event.TraceID == movediag.ID(ctx) {
			operations = append(operations, event.Operation)
		}
	}
	require.Equal(t, []string{"PING"}, operations)
}
