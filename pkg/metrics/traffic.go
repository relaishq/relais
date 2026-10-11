package metrics

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var storeCommands = promauto.NewCounterVec(prometheus.CounterOpts{Name: "relais_store_commands_total", Help: "Redis client command attempts, including pipelines and retries; excludes commands inside Lua."}, []string{"store"})
var transportBytes = promauto.NewCounterVec(prometheus.CounterOpts{Name: "relais_transport_bytes_total", Help: "TCP bytes successfully read or written, including protocol overhead and monitoring; excludes IP headers."}, []string{"transport", "direction"})

// TrafficConn measures actual transport bytes, including encrypted snapshots.
func TrafficConn(conn net.Conn, transport string) net.Conn {
	return &trafficConn{Conn: conn, transport: transport}
}

type trafficConn struct {
	net.Conn
	transport string
}

func (c *trafficConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	transportBytes.WithLabelValues(c.transport, "in").Add(float64(n))
	return n, err
}
func (c *trafficConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	transportBytes.WithLabelValues(c.transport, "out").Add(float64(n))
	return n, err
}

type trafficListener struct{ net.Listener }

func (l trafficListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return TrafficConn(c, "http"), nil
}
func TrafficListener(l net.Listener) net.Listener { return trafficListener{l} }

// RedisDialer supplies a wrapper without changing Redis retry policy.
func RedisDialer(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 5 * time.Minute}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return TrafficConn(c, "redis"), nil
}

type StoreHook string

func (h StoreHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	storeCommands.WithLabelValues(string(h)).Inc()
	return ctx, nil
}
func (StoreHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (h StoreHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	storeCommands.WithLabelValues(string(h)).Add(float64(len(cmds)))
	return ctx, nil
}
func (StoreHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TrafficCollectors() []prometheus.Collector {
	for _, store := range []string{"session", "frame"} {
		storeCommands.WithLabelValues(store)
	}
	for _, transport := range []string{"redis", "http"} {
		for _, direction := range []string{"in", "out"} {
			transportBytes.WithLabelValues(transport, direction)
		}
	}
	return []prometheus.Collector{storeCommands, transportBytes}
}

var httpTransportOnce sync.Once
var httpTransport http.RoundTripper

// HTTPTransport counts outgoing private HTTP connections while preserving the
// standard transport's timeouts, pooling, proxy and retry behavior.
func HTTPTransport() http.RoundTripper {
	httpTransportOnce.Do(func() {
		original, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			httpTransport = http.DefaultTransport
			return
		}
		transport := original.Clone()
		dial := transport.DialContext
		if dial == nil {
			dial = (&net.Dialer{}).DialContext
		}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return TrafficConn(c, "http"), nil
		}
		httpTransport = transport
	})
	return httpTransport
}
