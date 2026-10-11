package metrics

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func TestTrafficConnCountsActualPartialWritesAndReads(t *testing.T) {
	beforeIn := counterValue(transportBytes.WithLabelValues("test", "in"))
	beforeOut := counterValue(transportBytes.WithLabelValues("test", "out"))
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
	require.Equal(t, beforeIn+5, counterValue(transportBytes.WithLabelValues("test", "in")))
	require.Equal(t, beforeOut+3, counterValue(transportBytes.WithLabelValues("test", "out")))
}
func TestStoreHookIncludesPipelineCommands(t *testing.T) {
	before := counterValue(storeCommands.WithLabelValues("test"))
	hook := StoreHook("test")
	ctx, err := hook.BeforeProcess(context.Background(), redis.NewCmd(context.Background(), "get", "key"))
	require.NoError(t, err)
	_, err = hook.BeforeProcessPipeline(ctx, []redis.Cmder{redis.NewCmd(ctx, "set", "a", "b"), redis.NewCmd(ctx, "get", "a")})
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
