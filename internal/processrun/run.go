// Package processrun shares process-only configuration and HTTP lifecycle.
package processrun

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/framecache"
	"github.com/relaishq/relais/pkg/metrics"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
)

// StoreConfig requires an explicit Redis endpoint or Cluster seed list.
// Snapshot users also need a key.
// No Redis constructor infers a default endpoint.
type StoreConfig struct {
	Address, Prefix, Username, Password string
	Cluster                             bool
	clusterEnvErr                       error
	OutageBudget                        time.Duration
}

func Env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func (c *StoreConfig) Flags(fs *flag.FlagSet) {
	fs.DurationVar(&c.OutageBudget, "store-outage-budget", sessionstore.DefaultStoreOutageBudget, "live-call store outage retention budget; match on worker and control")
	c.ConnectionFlags(fs)
}

// ConnectionFlags registers Redis connection settings without worker retention policy.
func (c *StoreConfig) ConnectionFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Address, "redis", Env("RELAIS_REDIS_ADDR", ""), "explicit Redis host:port; comma-separated seeds with -redis-cluster (local 6379 requires RELAIS_REDIS_ALLOW_LOCAL_6379=1)")
	enabled, err := redisendpoint.EnvSwitch("RELAIS_REDIS_CLUSTER")
	c.clusterEnvErr = err
	fs.BoolVar(&c.Cluster, "redis-cluster", enabled, "use Redis Cluster with the -redis seed addresses (primary reads only)")
	fs.StringVar(&c.Prefix, "redis-prefix", Env("RELAIS_REDIS_PREFIX", "relais:process:"), "shared session-store namespace")
	// Keep credentials out of command-line flags and readiness output.
	c.Password = os.Getenv("RELAIS_REDIS_PASSWORD")
	c.Username = os.Getenv("RELAIS_REDIS_USERNAME")
}

// ValidateProductionRedis applies the production endpoint policy.
// Development and test code must use redisendpoint.ValidateDev instead.
func ValidateProductionRedis(addr string) error { return redisendpoint.Validate(addr) }

func (c StoreConfig) Open(ctx context.Context) (*sessionstore.Redis, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	key, opts, err := SessionStoreKeys(os.Getenv)
	if err != nil {
		return nil, err
	}
	cfg, err := c.redisConfig()
	if err != nil {
		return nil, err
	}
	return sessionstore.NewRedis(ctx, cfg, key, opts)
}

// OpenFrames shares the snapshot master key, using a separate HKDF domain.
func (c StoreConfig) OpenFrames(ctx context.Context) (*framecache.Redis, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	key, err := storeKey()
	if err != nil {
		return nil, err
	}
	cfg, err := c.redisConfig()
	if err != nil {
		return nil, err
	}
	return framecache.NewRedis(ctx, cfg, key, framecache.Limits{})
}

// SessionStoreKeys shares active/decrypt key parsing with process observers.
func SessionStoreKeys(lookup func(string) string) ([]byte, sessionstore.RedisOptions, error) {
	key, err := sessionStoreKey(lookup)
	if err != nil {
		return nil, sessionstore.RedisOptions{}, err
	}
	opts, err := sessionstore.RedisKeyOptions(lookup)
	return key, opts, err
}
func storeKey() ([]byte, error) { return sessionStoreKey(os.Getenv) }
func sessionStoreKey(lookup func(string) string) ([]byte, error) {
	encoded := lookup("RELAIS_SESSIONSTORE_KEY")
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		key, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("RELAIS_SESSIONSTORE_KEY must encode 32 bytes as hex or base64")
	}
	return key, nil
}
func Context() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	stop := func() { signal.Stop(signals); cancel() }
	go func() {
		select {
		case <-signals:
		case <-ctx.Done():
		}
		// Restore default signal handling before making the drain visible.
		stop()
	}()
	return ctx, stop
}

// ListenPrivate refuses accidental public exposure of unauthenticated APIs.
func ListenPrivate(addr string) (net.Listener, error) {
	if err := ValidatePrivateListener(addr, os.Getenv("RELAIS_PRIVATE_NETS")); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return metrics.TrafficListener(l), nil
}

// Ready is one machine-readable startup line, containing no store key.
func Ready(fields map[string]any) {
	fields["pid"] = os.Getpid()
	_ = json.NewEncoder(os.Stdout).Encode(fields)
}
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Protocols: protocols, HTTP2: &http.HTTP2Config{SendPingTimeout: 500 * time.Millisecond, PingTimeout: time.Second}, Handler: movediag.Handler(handler), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		<-done
		return nil
	}
}

// OpenOwners grants the relay only address lookup, with no snapshot key.
func (c StoreConfig) OpenOwners(ctx context.Context) (*sessionstore.RedisOwners, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	cfg, err := c.redisConfig()
	if err != nil {
		return nil, err
	}
	return sessionstore.NewRedisOwners(ctx, cfg)
}

// redisConfig is shared by every production Redis connection, including the
// keyless relay owner client. Credentials have no command-line representation.
func (c StoreConfig) redisConfig() (storage.RedisConfig, error) {
	if err := c.validate(); err != nil {
		return storage.RedisConfig{}, err
	}
	config, err := redisendpoint.TLSFromEnv()
	if err != nil {
		return storage.RedisConfig{}, err
	}
	cfg := storage.RedisConfig{Addr: c.Address, Prefix: c.Prefix, Username: c.Username, Password: c.Password, TLSConfig: config, Cluster: c.Cluster}
	if c.Cluster {
		cfg.Addr = ""
		cfg.Addrs = c.addresses()
	}
	return cfg, nil
}

func (c StoreConfig) addresses() []string {
	if !c.Cluster {
		return []string{c.Address}
	}
	addrs := strings.Split(c.Address, ",")
	for i := range addrs {
		addrs[i] = strings.TrimSpace(addrs[i])
	}
	return addrs
}
func (c StoreConfig) validate() error {
	if c.clusterEnvErr != nil {
		return c.clusterEnvErr
	}
	for _, addr := range c.addresses() {
		if err := ValidateProductionRedis(addr); err != nil {
			return err
		}
	}
	return nil
}
