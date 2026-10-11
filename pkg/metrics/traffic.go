package metrics

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/relais/internal/movediag"
)

var storeCommands = promauto.NewCounterVec(prometheus.CounterOpts{Name: "relais_store_commands_total", Help: "Redis client command attempts, including pipelines and retries; excludes commands inside Lua."}, []string{"store"})
var transportBytes = promauto.NewCounterVec(prometheus.CounterOpts{Name: "relais_transport_bytes_total", Help: "TCP bytes successfully read or written, including protocol overhead and monitoring; excludes IP headers."}, []string{"transport", "client", "direction"})

// TrafficConn measures actual transport bytes, including encrypted snapshots.
func TrafficConn(conn net.Conn, transport string) net.Conn {
	return trafficConnForClient(conn, transport, "")
}

type trafficConn struct {
	net.Conn
	readBytes, writeBytes prometheus.Counter
}

func trafficConnForClient(conn net.Conn, transport, client string) net.Conn {
	return &trafficConn{Conn: conn, readBytes: transportBytes.WithLabelValues(transport, client, "in"), writeBytes: transportBytes.WithLabelValues(transport, client, "out")}
}

func (c *trafficConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.readBytes.Add(float64(n))
	return n, err
}
func (c *trafficConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.writeBytes.Add(float64(n))
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

type redisDialFunc func(context.Context, string, string) (net.Conn, error)

// RedisDialer wraps the configured go-redis dialer, including its timeout,
// custom transport and TLS handshake behavior.
func RedisDialer(dial redisDialFunc, client string) redisDialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return trafficConnForClient(conn, "redis", client), nil
	}
}

// RedisClient uses go-redis v8's own option initialization to obtain its
// private default dialer. The unconnected prototype is immediately closed.
// Build the real client from fresh input options: re-initializing normalized
// options would turn MaxRetries=-1 back into automatic retries.
func RedisClient(options *redis.Options, client string) *redis.Client {
	original := *options
	dial := original.Dialer
	if dial == nil {
		prototypeOptions := original
		prototypeOptions.MinIdleConns = 0 // The prototype must never connect.
		prototype := redis.NewClient(&prototypeOptions)
		dial = prototype.Options().Dialer
		_ = prototype.Close()
	}
	original.Dialer = RedisDialer(dial, client)
	clientInstance := redis.NewClient(&original)
	clientInstance.AddHook(movediag.RedisHook{})
	return clientInstance
}

func StoreHook(store string) redis.Hook {
	return &storeHook{commands: storeCommands.WithLabelValues(store)}
}

type storeHook struct{ commands prometheus.Counter }

func (h *storeHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	h.commands.Inc()
	return ctx, nil
}
func (*storeHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (h *storeHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	h.commands.Add(float64(len(cmds)))
	return ctx, nil
}
func (*storeHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TrafficCollectors() []prometheus.Collector {
	for _, store := range []string{"session", "owners", "frame"} {
		storeCommands.WithLabelValues(store)
	}
	for _, client := range []string{"session", "owners", "frame"} {
		for _, direction := range []string{"in", "out"} {
			transportBytes.WithLabelValues("redis", client, direction)
		}
	}
	for _, direction := range []string{"in", "out"} {
		transportBytes.WithLabelValues("http", "", direction)
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
