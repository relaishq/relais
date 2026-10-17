package harness

import (
	"context"
	"flag"
	"fmt"
	"github.com/relaishq/relais/internal/processrun"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNamespaceRedisRequiresPrivateCredential(t *testing.T) {
	dir := t.TempDir()
	args, password, cleanup, err := redisLaunch("10.189.80.4", "16379", dir, true)
	require.NoError(t, err)
	defer cleanup()
	require.True(t, len(password) == 64, "namespace Redis must require a per-run credential")
	require.False(t, strings.Contains(strings.Join(args, " "), password), "password must not occur in argv")
	require.Equal(t, filepath.Join(dir, "redis-auth.conf"), args[0])
	info, err := os.Stat(args[0])
	require.NoError(t, err)
	require.EqualValues(t, 0600, info.Mode().Perm())
	b, err := os.ReadFile(args[0])
	require.NoError(t, err)
	require.True(t, strings.Contains(string(b), "requirepass "+password+"\n"), "private config must require the generated password")
	require.True(t, strings.Contains(string(b), "protected-mode yes\n"), "protected mode must remain enabled")
	cleanup()
	_, err = os.Stat(args[0])
	require.True(t, os.IsNotExist(err))
}
func TestLoopbackRedisCommandUnchanged(t *testing.T) {
	args, password, cleanup, err := redisLaunch("127.0.0.1", "16379", t.TempDir(), false)
	require.NoError(t, err)
	defer cleanup()
	require.Empty(t, password)
	require.Equal(t, "--bind", args[0])
}

func TestPrivateRedisProtectedModeAndProcessCredential(t *testing.T) {
	if _, err := exec.LookPath("redis-server"); err != nil {
		t.Skip("redis-server unavailable")
	}
	host := ""
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, address := range addrs {
		prefix, e := netip.ParsePrefix(address.String())
		if e == nil && prefix.Addr().Is4() && prefix.Addr().IsPrivate() && !prefix.Addr().IsLoopback() {
			host = prefix.Addr().String()
			break
		}
	}
	if host == "" {
		t.Skip("no private non-loopback address; construction regression still runs")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	require.NotContains(t, []int{6379, 9101, 9201}, port)
	dir := t.TempDir()
	args, password, cleanup, err := redisLaunch(host, fmt.Sprint(port), dir, true)
	require.NoError(t, err)
	defer cleanup()
	top := &Topology{Dir: dir, Root: dir, redisPassword: password}
	_, err = top.start("redis", "redis-server", args, os.Environ())
	require.NoError(t, err)
	defer top.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address := net.JoinHostPort(host, fmt.Sprint(port))
	require.NoError(t, top.waitReady(ctx, "redis", address, time.Second))
	require.Error(t, pingRedis(ctx, address, ""), "unauthenticated private client must fail")
	require.NoError(t, pingRedis(ctx, address, password))
	t.Setenv("RELAIS_REDIS_ADDR", address)
	t.Setenv("RELAIS_REDIS_PASSWORD", password)
	var cfg processrun.StoreConfig
	cfg.ConnectionFlags(flag.NewFlagSet("auth-regression", flag.ContinueOnError))
	require.True(t, cfg.Password == password, "real process config must consume the credential")
	owners, err := cfg.OpenOwners(ctx)
	require.NoError(t, err)
	defer owners.Close()
}
