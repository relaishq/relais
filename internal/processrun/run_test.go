package processrun

import (
	"bufio"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/stretchr/testify/require"
)

func TestProcessConfigurationRequiresDedicatedStore(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	for _, addr := range []string{"", "localhost:6379", "127.0.0.1:6379", "[::1]:6379", "not-an-address", "127.0.0.1:06379", "127.0.0.1:redis", "127.0.0.1:0"} {
		require.Error(t, ValidateProductionRedis(addr), addr)
	}
	require.NoError(t, ValidateProductionRedis("127.0.0.1:16379"))
	t.Setenv("RELAIS_SESSIONSTORE_KEY", "")
	_, err := (StoreConfig{Address: "127.0.0.1:1"}).Open(context.Background())
	require.ErrorContains(t, err, "32 bytes")
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	_, err = (StoreConfig{Address: "localhost:6379"}).Open(context.Background())
	require.ErrorContains(t, err, "reserved")
	for _, addr := range []string{"0.0.0.0:0", "localhost:0", "192.168.1.1:0", ":0"} {
		_, err := ListenPrivate(addr)
		require.Error(t, err)
	}
	listener, err := ListenPrivate("127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}

func TestStoreConfigAuthenticatedConnections(t *testing.T) {
	password := os.Getenv("RELAIS_TEST_REDIS_PASSWORD")
	if password == "" {
		t.Skip("set RELAIS_TEST_REDIS_PASSWORD with a dedicated password-protected Redis")
	}
	addr := redistestenv.Address(t)
	require.NoError(t, redisendpoint.ValidateDev(addr))
	t.Setenv("RELAIS_REDIS_ADDR", addr)
	t.Setenv("RELAIS_REDIS_PASSWORD", password)
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	var config StoreConfig
	config.Flags(flag.NewFlagSet("authenticated", flag.ContinueOnError))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := config.Open(ctx)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	frames, err := config.OpenFrames(ctx)
	require.NoError(t, err)
	defer func() { _ = frames.Close() }()
	owners, err := config.OpenOwners(ctx)
	require.NoError(t, err)
	defer func() { _ = owners.Close() }()
	config.Password = ""
	_, err = config.OpenOwners(ctx)
	require.ErrorContains(t, err, "NOAUTH")
}

func TestSecondSignalExitsDuringDrain(t *testing.T) {
	if os.Getenv("RELAIS_TEST_SIGNAL_CHILD") == "1" {
		ctx, stop := Context()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("draining")
		select {}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondSignalExitsDuringDrain$")
	cmd.Env = append(os.Environ(), "RELAIS_TEST_SIGNAL_CHILD=1")
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	lines := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	expect := func(line string) {
		t.Helper()
		select {
		case got := <-lines:
			require.Equal(t, line, got)
		case <-time.After(3 * time.Second):
			t.Fatalf("did not receive %s", line)
		}
	}
	expect("ready")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	expect("draining")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-done:
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		status, ok := exit.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		require.Equal(t, syscall.SIGTERM, status.Signal())
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("second SIGTERM was ignored while draining")
	}
}

func TestRelayConnectionFlagsExcludeOutageBudget(t *testing.T) {
	var c StoreConfig
	relay := flag.NewFlagSet("relay", flag.ContinueOnError)
	c.ConnectionFlags(relay)
	require.Nil(t, relay.Lookup("store-outage-budget"))
	require.NotNil(t, relay.Lookup("redis"))
	worker := flag.NewFlagSet("worker", flag.ContinueOnError)
	c.Flags(worker)
	require.NotNil(t, worker.Lookup("store-outage-budget"))
}

func TestRedisCredentialsAndStartupTLSValidation(t *testing.T) {
	t.Setenv("RELAIS_REDIS_USERNAME", "private-username-sentinel")
	t.Setenv("RELAIS_REDIS_PASSWORD", "private-password-sentinel")
	t.Setenv("RELAIS_REDIS_TLS", "")
	t.Setenv("RELAIS_REDIS_TLS_CA_FILE", "")
	t.Setenv("RELAIS_REDIS_TLS_CERT_FILE", "")
	t.Setenv("RELAIS_REDIS_TLS_KEY_FILE", "")
	t.Setenv("RELAIS_REDIS_TLS_SERVER_NAME", "")
	var config StoreConfig
	flags := flag.NewFlagSet("credentials", flag.ContinueOnError)
	config.ConnectionFlags(flags)
	config.Address = "cache.example.invalid:16379"
	cfg, err := config.redisConfig()
	require.NoError(t, err)
	require.Equal(t, "private-username-sentinel", cfg.Username)
	require.Equal(t, "private-password-sentinel", cfg.Password)
	require.Nil(t, cfg.TLSConfig)
	for _, name := range []string{"redis-password", "redis-username", "redis-tls-key-file"} {
		require.Nil(t, flags.Lookup(name))
	}
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	t.Setenv("RELAIS_REDIS_TLS_CA_FILE", "private-file-path-sentinel")
	config.Address = "cache.example.invalid:6379"
	_, err = config.Open(context.Background())
	require.ErrorContains(t, err, "RELAIS_REDIS_TLS=1")
	_, err = config.OpenFrames(context.Background())
	require.ErrorContains(t, err, "RELAIS_REDIS_TLS=1")
	_, err = config.OpenOwners(context.Background())
	require.ErrorContains(t, err, "RELAIS_REDIS_TLS=1")
	require.NotContains(t, err.Error(), "private-")
}

func TestRedisClusterConfiguration(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ADDR", "cache-a.example.invalid:16379, cache-b.example.invalid:16380")
	t.Setenv("RELAIS_REDIS_CLUSTER", "1")
	t.Setenv("RELAIS_REDIS_USERNAME", "acl-user")
	t.Setenv("RELAIS_REDIS_PASSWORD", "secret")
	t.Setenv("RELAIS_REDIS_TLS", "1")
	var c StoreConfig
	fs := flag.NewFlagSet("cluster", flag.ContinueOnError)
	c.ConnectionFlags(fs)
	cfg, err := c.redisConfig()
	require.NoError(t, err)
	require.True(t, cfg.Cluster)
	require.Empty(t, cfg.Addr)
	require.Equal(t, []string{"cache-a.example.invalid:16379", "cache-b.example.invalid:16380"}, cfg.Addrs)
	require.Equal(t, "acl-user", cfg.Username)
	require.Equal(t, "secret", cfg.Password)
	require.NotNil(t, cfg.TLSConfig)
	options, err := cfg.ConnectionOptions()
	require.NoError(t, err)
	cluster := options.Cluster()
	require.False(t, cluster.ReadOnly)
	require.False(t, cluster.RouteByLatency)
	require.False(t, cluster.RouteRandomly)
	require.NoError(t, fs.Parse([]string{"-redis-cluster=false", "-redis=cache.example.invalid:16379"}))
	cfg, err = c.redisConfig()
	require.NoError(t, err)
	require.False(t, cfg.Cluster)
	require.Empty(t, cfg.Addrs)
	require.Equal(t, "cache.example.invalid:16379", cfg.Addr)
	for _, addr := range []string{"", "127.0.0.1:6379", "cache.example.invalid:16379,", "cache.example.invalid:16379,127.0.0.1:6379"} {
		c.Cluster, c.Address = true, addr
		_, err = c.redisConfig()
		require.Error(t, err)
	}
}

func TestRedisClusterEnvironmentSwitch(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ADDR", "cache.example.invalid:16380")
	for _, value := range []string{"true", "false", "yes", "typo"} {
		t.Setenv("RELAIS_REDIS_CLUSTER", value)
		var config StoreConfig
		config.ConnectionFlags(flag.NewFlagSet("cluster", flag.ContinueOnError))
		_, err := config.redisConfig()
		require.ErrorContains(t, err, "RELAIS_REDIS_CLUSTER must be 0 or 1")
	}
	for _, value := range []string{"", "0", "1"} {
		t.Setenv("RELAIS_REDIS_CLUSTER", value)
		var config StoreConfig
		config.ConnectionFlags(flag.NewFlagSet("cluster", flag.ContinueOnError))
		cfg, err := config.redisConfig()
		require.NoError(t, err)
		require.Equal(t, value == "1", cfg.Cluster)
	}
}

func TestSessionStoreKeyRingEnvironmentValidation(t *testing.T) {
	t.Setenv("RELAIS_SESSIONSTORE_KEY", hex.EncodeToString(make([]byte, 32)))
	t.Setenv("RELAIS_SESSIONSTORE_KEY_ID", "256")
	_, err := (StoreConfig{Address: "127.0.0.1:16379"}).Open(context.Background())
	require.ErrorContains(t, err, "RELAIS_SESSIONSTORE_KEY_ID")
	t.Setenv("RELAIS_SESSIONSTORE_KEY_ID", "1")
	t.Setenv("RELAIS_SESSIONSTORE_OLD_KEYS", `{"0":"secret-do-not-echo"}`)
	_, err = (StoreConfig{Address: "127.0.0.1:16379"}).Open(context.Background())
	require.ErrorContains(t, err, "RELAIS_SESSIONSTORE_OLD_KEYS")
	require.NotContains(t, err.Error(), "secret-do-not-echo")
}
