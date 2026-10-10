package main

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/relais/internal/nettopology"
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
	require.Error(t, validateTopologyOptions("netns", "region", ""))
}
func TestDriverDoesNotInheritPrivateAllowList(t *testing.T) {
	t.Setenv("RELAIS_PRIVATE_NETS", "10.0.0.0/8")
	require.Empty(t, envValue(processEnv("key", "127.0.0.1:16379", "prefix"), "RELAIS_PRIVATE_NETS"))
	require.NotEmpty(t, os.Getenv("RELAIS_PRIVATE_NETS"))
}
