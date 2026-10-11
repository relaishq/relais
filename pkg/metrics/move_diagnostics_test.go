package metrics

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"

	"github.com/go-redis/redis/v8"
	"github.com/relais/internal/movediag"
	"github.com/stretchr/testify/require"
)

func TestMoveDiagnosticsThroughRealRedisClient(t *testing.T) {
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("dedicated Redis required")
		}
		t.Skip("dedicated Redis not configured")
	}
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.NotEqual(t, "6379", port)
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	client := RedisClient(&redis.Options{Addr: addr, MaxRetries: -1}, "move-test")
	defer client.Close()
	require.NoError(t, client.Ping(context.Background()).Err())
	ctx := movediag.Phase(movediag.Begin(context.Background()), "resume")
	require.Equal(t, redis.Nil, client.Get(ctx, "move-diagnostic-missing-key").Err())
	_, err = client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Ping(ctx); p.Echo(ctx, "secret-payload"); return nil })
	require.NoError(t, err)
	var events []movediag.Event
	for _, event := range movediag.Default.Snapshot().Events {
		if event.TraceID == movediag.ID(ctx) {
			events = append(events, event)
		}
	}
	require.Len(t, events, 3)
	require.Equal(t, []string{"GET", "PING", "ECHO"}, []string{events[0].Operation, events[1].Operation, events[2].Operation})
	for _, event := range events {
		require.Equal(t, "resume", event.Phase)
		require.False(t, event.Failed)
		require.True(t, event.End.After(event.Start))
	}
	data, err := json.Marshal(events)
	require.NoError(t, err)
	require.NotContains(t, string(data), "secret-payload")
	require.NotContains(t, string(data), "missing-key")
}
