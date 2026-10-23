package metrics

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestTrafficConnCountsActualPartialWritesAndReads(t *testing.T) {
	beforeIn := counterValue(transportBytes.WithLabelValues("test", "", "in"))
	beforeOut := counterValue(transportBytes.WithLabelValues("test", "", "out"))
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	wrapped := TrafficConn(client, "test")
	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf [3]byte
		_, _ = io.ReadFull(server, buf[:])
		_, _ = server.Write([]byte("reply"))
	}()
	n, err := wrapped.Write([]byte("abc"))
	require.NoError(t, err)
	require.Equal(t, 3, n)
	var buf [5]byte
	_, err = io.ReadFull(wrapped, buf[:])
	require.NoError(t, err)
	<-done
	require.Equal(t, beforeIn+5, counterValue(transportBytes.WithLabelValues("test", "", "in")))
	require.Equal(t, beforeOut+3, counterValue(transportBytes.WithLabelValues("test", "", "out")))
}
func TestStoreHookIncludesPipelineCommands(t *testing.T) {
	before := counterValue(storeCommands.WithLabelValues("test"))
	hook := StoreHook("test")
	ctx := context.Background()
	err := hook.ProcessHook(func(context.Context, redis.Cmder) error { return nil })(ctx, redis.NewCmd(ctx, "get", "key"))
	require.NoError(t, err)
	err = hook.ProcessPipelineHook(func(context.Context, []redis.Cmder) error { return nil })(ctx, []redis.Cmder{redis.NewCmd(ctx, "set", "a", "b"), redis.NewCmd(ctx, "get", "a")})
	require.NoError(t, err)
	require.Equal(t, before+3, counterValue(storeCommands.WithLabelValues("test")))
	recorder := httptest.NewRecorder()
	ProcessHandler(func() []Sample { return nil }, prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "relais_test_zero", Help: "test"}, func() float64 { return 0 })).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	require.True(t, strings.Contains(recorder.Body.String(), "relais_store_commands_total"))
}

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	_ = c.Write(&m)
	return m.GetCounter().GetValue()
}

func TestStoreHookExcludesConnectionHandshake(t *testing.T) {
	ctx := context.Background()
	commands := []redis.Cmder{redis.NewCmd(ctx, "hello", 2), redis.NewCmd(ctx, "auth", "owned-password"), redis.NewCmd(ctx, "select", 1), redis.NewCmd(ctx, "client", "setinfo", "LIB-NAME", "go-redis"), redis.NewCmd(ctx, "client", "id"), redis.NewCmd(ctx, "ping")}
	counter := storeCommands.WithLabelValues("handshake-test")
	before := counterValue(counter)
	hook := StoreHook("handshake-test")
	var singles, pipelines int
	for _, command := range commands {
		require.NoError(t, hook.ProcessHook(func(context.Context, redis.Cmder) error { singles++; return nil })(ctx, command))
	}
	require.Equal(t, 2.0, counterValue(counter)-before)
	require.NoError(t, hook.ProcessPipelineHook(func(context.Context, []redis.Cmder) error { pipelines++; return nil })(ctx, commands))
	require.Equal(t, 4.0, counterValue(counter)-before)
	require.Equal(t, len(commands), singles)
	require.Equal(t, 1, pipelines)
}
