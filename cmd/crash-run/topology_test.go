package main

import (
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/nettopology"
	"github.com/stretchr/testify/require"
)

func TestTopologyBindingsAndOptions(t *testing.T) {
	p, err := nettopology.NewPlan(0, 1, "aabbccddee", 2)
	require.NoError(t, err)
	topology := &nettopology.Topology{Plan: p}
	require.Equal(t, "127.0.0.1:0", roleBind(nil, "relay", true))
	for _, role := range []string{"relay", "worker-0", "worker-1", "control", "store"} {
		addr, err := netip.ParseAddrPort(roleBind(topology, role, false))
		require.NoError(t, err)
		require.Equal(t, p.Roles[role].Private, addr.Addr())
	}
	public, err := netip.ParseAddrPort(roleBind(topology, "relay", true))
	require.NoError(t, err)
	require.Equal(t, p.Roles["relay"].Public, public.Addr())
	redis, err := redisAddress(topology)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(redis, ":16379"))
	require.NoError(t, validateTopologyOptions("loopback", "lan", "127.0.0.1:16379"))
	require.NoError(t, validateTopologyOptions("netns", "lan", ""))
	require.Error(t, validateTopologyOptions("netns", "lan", "127.0.0.1:16379"))
	require.Error(t, validateTopologyOptions("wrong", "lan", ""))
	require.NoError(t, validateTopologyOptions("netns", "region", ""))
	require.Error(t, validateTopologyOptions("loopback", "region", ""))
}
func TestDriverDoesNotInheritPrivateAllowList(t *testing.T) {
	t.Setenv("RELAIS_PRIVATE_NETS", "10.0.0.0/8")
	t.Setenv("RELAIS_REDIS_PASSWORD", "inherited-credential")
	require.Empty(t, envValue(processEnv("key", "127.0.0.1:16379", "prefix"), "RELAIS_PRIVATE_NETS"))
	require.Empty(t, envValue(processEnv("key", "127.0.0.1:16379", "prefix"), "RELAIS_REDIS_PASSWORD"))
	require.NotEmpty(t, os.Getenv("RELAIS_PRIVATE_NETS"))
}

func TestProcessEnvDropsInheritedRedisSettings(t *testing.T) {
	for _, name := range []string{
		"RELAIS_REDIS_ADDR", "RELAIS_REDIS_PREFIX", "RELAIS_REDIS_PASSWORD",
		"RELAIS_REDIS_USERNAME", "RELAIS_REDIS_TLS", "RELAIS_REDIS_TLS_CA_FILE",
		"RELAIS_REDIS_TLS_SERVER_NAME", "RELAIS_REDIS_TLS_CERT_FILE", "RELAIS_REDIS_TLS_KEY_FILE",
		"RELAIS_REDIS_ALLOW_LOCAL_6379", "RELAIS_REDIS_FUTURE_SETTING",
	} {
		t.Setenv(name, "inherited-value")
	}
	t.Setenv("RELAIS_TEST_CHILD_MARKER", "preserved")
	env := processEnv("owned-key", "127.0.0.1:16379", "owned-prefix:")
	redisSettings := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "RELAIS_REDIS_") {
			require.NotContains(t, redisSettings, name, "owned settings must appear once")
			redisSettings[name] = value
		}
	}
	require.Equal(t, map[string]string{"RELAIS_REDIS_ADDR": "127.0.0.1:16379", "RELAIS_REDIS_PREFIX": "owned-prefix:"}, redisSettings)
	require.Contains(t, env, "RELAIS_TEST_CHILD_MARKER=preserved")
}

// #413: control failed to bind because its pre-picked port equalled the relay
// HTTP port the relay had already bound. Every fixed endpoint must bind while
// the earlier ones are held. The deterministic check is in clusterprocess.
func TestPairTopologyEndpointsBindTogether(t *testing.T) {
	topology, err := newPairTopology(nil, t.TempDir(), nil, "", 0, 0)
	require.NoError(t, err)
	ports := map[string]bool{}
	bind := func(network, addr string) {
		_, port, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		require.False(t, ports[port], "port %s picked twice", port)
		ports[port] = true
		var socket io.Closer
		if network == "udp" {
			socket, err = net.ListenPacket(network, addr)
		} else {
			socket, err = net.Listen(network, addr)
		}
		require.NoError(t, err, "%s %s", network, addr)
		t.Cleanup(func() { _ = socket.Close() })
	}
	for _, addr := range []string{topology.relayMedia, topology.relayLeg, topology.workerMedia[0], topology.workerMedia[1]} {
		bind("udp", addr)
	}
	for _, addr := range []string{topology.relayHTTP, topology.controlAddr, topology.signalAddr, topology.workerHTTP[0], topology.workerHTTP[1]} {
		bind("tcp", addr)
	}
}
