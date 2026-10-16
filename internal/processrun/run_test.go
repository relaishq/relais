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
	"github.com/stretchr/testify/require"
)

func TestProcessConfigurationRequiresDedicatedStore(t *testing.T) {
	t.Setenv("RELAIS_REDIS_ALLOW_LOCAL_6379", "")
	for _, addr := range []string{"", "localhost:6379", "127.0.0.1:6379", "[::1]:6379", "not-an-address", "127.0.0.1:06379", "127.0.0.1:redis", "127.0.0.1:0"} {
		require.Error(t, ValidateRedis(addr), addr)
	}
	require.NoError(t, ValidateRedis("127.0.0.1:16379"))
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
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
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
