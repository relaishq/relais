package main

import (
	"flag"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/relais/internal/nettopology"
	"github.com/relais/internal/processrun"
	"github.com/stretchr/testify/require"
)

func capacityPlan(t *testing.T) *nettopology.Topology {
	t.Helper()
	plan, err := nettopology.NewPlan(0, 5413, "655912ff37", 3)
	require.NoError(t, err)
	return &nettopology.Topology{Plan: plan}
}

// Exercise the flags used by execute and parsed by the caller process. The
// caller has no private address: media must use its public-side plan address.
func TestCapacityCallerNamespaceAndBindFlags(t *testing.T) {
	topology := capacityPlan(t)
	fs := flag.NewFlagSet("caller", flag.ContinueOnError)
	namespace := fs.String("caller-namespace", "", "")
	bind := fs.String("caller-bind", "", "")
	require.NoError(t, fs.Parse(callerNetworkArgs(topology)))
	require.Equal(t, topology.Plan.Roles["caller"].Namespace, *namespace)
	addr, err := netip.ParseAddrPort(*bind)
	require.NoError(t, err)
	require.Equal(t, topology.Plan.Roles["caller"].Public, addr.Addr())
	require.True(t, addr.Addr().Is4() && !addr.Addr().IsUnspecified())
	require.Zero(t, addr.Port())
	external, err := childTopology("http://control/calls", netip.MustParseAddrPort(roleBind(topology, "relay", true)), *namespace, *bind)
	require.NoError(t, err)
	require.NotNil(t, external.CallerSocket)
	require.Empty(t, callerNetworkArgs(nil))
	external, err = childTopology("http://control/calls", netip.MustParseAddrPort("127.0.0.1:20000"), "", "")
	require.NoError(t, err)
	require.Nil(t, external.CallerSocket)
	for _, invalid := range [][2]string{{*namespace, ""}, {*namespace, "0.0.0.0:0"}, {*namespace, "[::1]:0"}, {"", *bind}} {
		_, err = childTopology("http://control/calls", external.RelayAddr, invalid[0], invalid[1])
		require.ErrorContains(t, err, "specific IPv4 UDP bind address")
	}
}

func TestCapacityPrivateRoleBindings(t *testing.T) {
	topology := capacityPlan(t)
	for _, role := range []string{"relay", "control", "store", "worker-0", "worker-1", "worker-2"} {
		addr := netip.MustParseAddrPort(roleBind(topology, role, false))
		require.Equal(t, topology.Plan.Roles[role].Private, addr.Addr())
		require.NoError(t, processrun.ValidatePrivateListener(addr.String(), topology.Plan.PrivateNets()))
		require.False(t, topology.Plan.CallerNet.Contains(addr.Addr()))
	}
	relay := netip.MustParseAddrPort(roleBind(topology, "relay", true))
	require.Equal(t, topology.Plan.Roles["relay"].Public, relay.Addr())
	require.True(t, topology.Plan.CallerNet.Contains(relay.Addr()))
	require.Error(t, processrun.ValidatePrivateListener(relay.String(), topology.Plan.PrivateNets()))
	control, err := controlAddress(topology)
	require.NoError(t, err)
	require.Equal(t, netip.AddrPortFrom(topology.Plan.Roles["control"].Private, 40001).String(), control)
	store, err := redisAddress(topology)
	require.NoError(t, err)
	require.Equal(t, netip.AddrPortFrom(topology.Plan.Roles["store"].Private, 16379).String(), store)
	require.NoError(t, processrun.ValidateRedis(store))
}

func TestCapacityRedisConfigKeepsPasswordOutOfArguments(t *testing.T) {
	topology := capacityPlan(t)
	password := strings.Repeat("a", 64)
	args, cleanup, err := redisCommand(topology, "redis-server", "16379", t.TempDir(), password)
	require.NoError(t, err)
	defer cleanup()
	require.NotContains(t, strings.Join(args, " "), password)
	require.Contains(t, args, redisHost(topology))
	info, err := os.Stat(args[1])
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	contents, err := os.ReadFile(args[1])
	require.NoError(t, err)
	require.True(t, string(contents) == "requirepass "+password+"\n", "password must match the service environment")
	cleanup()
	_, err = os.Stat(args[1])
	require.True(t, os.IsNotExist(err))
}
