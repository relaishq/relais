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
	"syscall"
	"time"

	"github.com/relais/internal/redisendpoint"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
)

// StoreConfig requires an explicit Redis address. Snapshot users also need a key.
// It does not inherit the older storage package's localhost:6379 default.
type StoreConfig struct{ Address, Prefix string }

func Env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func (c *StoreConfig) Flags(fs *flag.FlagSet) {
	fs.StringVar(&c.Address, "redis", Env("RELAIS_REDIS_ADDR", ""), "explicit Redis host:port (6379 is refused)")
	fs.StringVar(&c.Prefix, "redis-prefix", Env("RELAIS_REDIS_PREFIX", "relais:process:"), "shared session-store namespace")
}
func ValidateRedis(addr string) error { return redisendpoint.Validate(addr) }

func (c StoreConfig) Open(ctx context.Context) (*sessionstore.Redis, error) {
	if err := ValidateRedis(c.Address); err != nil {
		return nil, err
	}
	key, err := storeKey()
	if err != nil {
		return nil, err
	}
	return sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: c.Address, Prefix: c.Prefix}, key)
}

// OpenFrames shares the snapshot master key, using a separate HKDF domain.
func (c StoreConfig) OpenFrames(ctx context.Context) (*framecache.Redis, error) {
	if err := ValidateRedis(c.Address); err != nil {
		return nil, err
	}
	key, err := storeKey()
	if err != nil {
		return nil, err
	}
	return framecache.NewRedis(ctx, storage.RedisConfig{Addr: c.Address, Prefix: c.Prefix}, key, framecache.Limits{})
}
func storeKey() ([]byte, error) {
	encoded := os.Getenv("RELAIS_SESSIONSTORE_KEY")
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
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("private HTTP listener must use a literal loopback IP")
	}
	return net.Listen("tcp", addr)
}

// Ready is one machine-readable startup line, containing no store key.
func Ready(fields map[string]any) {
	fields["pid"] = os.Getpid()
	_ = json.NewEncoder(os.Stdout).Encode(fields)
}
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
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
	if err := ValidateRedis(c.Address); err != nil {
		return nil, err
	}
	return sessionstore.NewRedisOwners(ctx, storage.RedisConfig{Addr: c.Address, Prefix: c.Prefix})
}
