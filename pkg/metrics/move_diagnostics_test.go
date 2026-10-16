package metrics

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/go-redis/redis/v8"
	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/internal/redisendpoint"
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
	require.NoError(t, redisendpoint.ValidateDev(addr))
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	client := RedisClient(&redis.Options{Addr: addr, MaxRetries: -1}, "move-test")
	defer client.Close()
	require.NoError(t, client.Ping(context.Background()).Err())
	ctx := movediag.Phase(movediag.Begin(context.Background()), "resume")
	// A client constructed with diagnostics off must have no trace hook,
	// even if a traced coordination context later reaches a snapshot command.
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "")
	plain := RedisClient(&redis.Options{Addr: addr, MaxRetries: -1}, "move-default-test")
	defer plain.Close()
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	require.NoError(t, plain.Ping(ctx).Err())
	for _, event := range movediag.Default.Snapshot().Events {
		require.NotEqual(t, movediag.ID(ctx), event.TraceID)
	}
	require.Equal(t, redis.Nil, client.Get(ctx, "move-diagnostic-missing-key").Err())
	_, err := client.Pipelined(ctx, func(p redis.Pipeliner) error { p.Ping(ctx); p.Echo(ctx, "secret-payload"); return nil })
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
