package movediag

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRedisHookExcludesConnectionHandshake(t *testing.T) {
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	ctx := Begin(context.Background())
	commands := []redis.Cmder{redis.NewCmd(ctx, "hello", 2), redis.NewCmd(ctx, "auth", "owned-password"), redis.NewCmd(ctx, "client", "setinfo", "LIB-NAME", "go-redis"), redis.NewCmd(ctx, "client", "id"), redis.NewCmd(ctx, "ping")}
	hook := RedisHook{}
	var singles, pipelines int
	for _, command := range commands {
		require.NoError(t, hook.ProcessHook(func(context.Context, redis.Cmder) error { singles++; return nil })(ctx, command))
	}
	require.NoError(t, hook.ProcessPipelineHook(func(context.Context, []redis.Cmder) error { pipelines++; return nil })(ctx, commands))
	var operations []string
	for _, event := range Default.Snapshot().Events {
		if event.TraceID == ID(ctx) {
			operations = append(operations, event.Operation)
		}
	}
	require.Equal(t, []string{"CLIENT", "PING", "CLIENT", "PING"}, operations)
	require.Equal(t, len(commands), singles)
	require.Equal(t, 1, pipelines)
}
