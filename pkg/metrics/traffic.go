package metrics

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"github.com/relaishq/relais/internal/movediag"
)

var storeCommands = promauto.NewCounterVec(prometheus.CounterOpts{Name: "relais_store_commands_total", Help: "Redis client command attempts, including pipelines and retries; excludes commands inside Lua."}, []string{"store"})
var transportBytes = promauto.NewCounterVec(prometheus.CounterOpts{Name: "relais_transport_bytes_total", Help: "TCP bytes successfully read or written, including protocol overhead and monitoring; excludes IP headers."}, []string{"transport", "client", "direction"})

// TrafficConn measures actual transport bytes, including encrypted snapshots.
func TrafficConn(conn net.Conn, transport string) net.Conn {
	return trafficConnForClient(conn, transport, "")
}

var storeWireParseFailures = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "relais_store_wire_parse_failures_total", Help: "Connections with uncountable Redis commands; custom approval must block."}, []string{"client"})

var storeWireCommands = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "relais_store_wire_commands_total", Help: "RESP command starts written to Redis, including library retries and connection setup."}, []string{"client"})

type trafficConn struct {
	wireCommands      prometheus.Counter
	wireParseFailures prometheus.Counter
	writeMu           sync.Mutex
	parser            redisCommandCounter
	net.Conn
	readBytes, writeBytes prometheus.Counter
}

func trafficConnForClient(conn net.Conn, transport, client string) net.Conn {
	var commands, parseFailures prometheus.Counter
	if transport == "redis" {
		commands = storeWireCommands.WithLabelValues(client)
		parseFailures = storeWireParseFailures.WithLabelValues(client)
	}
	return &trafficConn{wireParseFailures: parseFailures, wireCommands: commands, Conn: conn, readBytes: transportBytes.WithLabelValues(transport, client, "in"), writeBytes: transportBytes.WithLabelValues(transport, client, "out")}
}

func (c *trafficConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.readBytes.Add(float64(n))
	return n, err
}
func (c *trafficConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.Conn.Write(p)
	c.writeBytes.Add(float64(n))
	if c.wireCommands != nil {
		wasInvalid := c.parser.invalid
		c.wireCommands.Add(float64(c.parser.feed(p[:n])))
		if !wasInvalid && c.parser.invalid {
			c.wireParseFailures.Inc()
		}
	}
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

type redisDialFunc = redis.DialHook

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

// RedisClient initializes options once, then instruments the initialized dialer.
func RedisClient(options *redis.Options, client string) *redis.Client {
	original := *options
	return InstrumentRedisClient(redis.NewClient(&original), client)
}

// InstrumentRedisClient preserves the client's initialized TLS, retry and dial
// settings. Cluster callers first construct the node through the policy guard.
func InstrumentRedisClient(instance *redis.Client, client string) *redis.Client {
	instance.AddHook(redisTrafficHook{client: client})
	if movediag.Enabled() {
		instance.AddHook(movediag.RedisHook{})
	}
	return instance
}

type redisTrafficHook struct{ client string }

func (h redisTrafficHook) DialHook(next redis.DialHook) redis.DialHook {
	return RedisDialer(next, h.client)
}
func (redisTrafficHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (redisTrafficHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func StoreHook(store string) redis.Hook {
	return &storeHook{commands: storeCommands.WithLabelValues(store)}
}

type storeHook struct{ commands prometheus.Counter }

func (*storeHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *storeHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { h.commands.Inc(); return next(ctx, cmd) }
}
func (h *storeHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.commands.Add(float64(len(cmds)))
		return next(ctx, cmds)
	}
}

func TrafficCollectors() []prometheus.Collector {
	for _, store := range []string{"session", "owners", "frame"} {
		storeCommands.WithLabelValues(store)
		storeWireCommands.WithLabelValues(store)
		storeWireParseFailures.WithLabelValues(store)
	}
	for _, client := range []string{"session", "owners", "frame"} {
		for _, direction := range []string{"in", "out"} {
			transportBytes.WithLabelValues("redis", client, direction)
		}
	}
	for _, direction := range []string{"in", "out"} {
		transportBytes.WithLabelValues("http", "", direction)
	}

	return []prometheus.Collector{storeWireParseFailures, storeWireCommands, storeCommands, transportBytes}
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
