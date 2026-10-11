package nettopology

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testPlan(t *testing.T) Plan {
	t.Helper()
	p, err := NewPlan(501, 12345, "aabbccddee", 2)
	require.NoError(t, err)
	return p
}
func TestAddressPlanAndCommands(t *testing.T) {
	p := testPlan(t)
	require.Len(t, p.Roles, 6)
	require.Len(t, p.Links, 7)
	require.NotEmpty(t, p.FabricNamespace)
	require.LessOrEqual(t, len(p.ManagementHost), 15)
	require.LessOrEqual(t, len(p.ManagementPeer), 15)
	require.False(t, p.Datacentre.Overlaps(p.CallerNet))
	namespaces := map[string]bool{}
	addresses := map[netip.Addr]bool{p.Host: true}
	for name, role := range p.Roles {
		require.False(t, namespaces[role.Namespace], name)
		namespaces[role.Namespace] = true
		for _, ip := range []netip.Addr{role.Private, role.Public} {
			if !ip.IsValid() {
				continue
			}
			require.True(t, ip.IsPrivate())
			require.False(t, addresses[ip], name)
			addresses[ip] = true
		}
	}
	require.False(t, p.Roles["caller"].Private.IsValid())
	for _, link := range p.Links {
		require.LessOrEqual(t, len(link.HostLink), 15)
		require.LessOrEqual(t, len(link.PeerLink), 15)
		require.True(t, link.Address.Addr().IsPrivate())
	}
	commands := p.SetupCommands()
	for _, role := range []string{"caller", "relay"} {
		require.Contains(t, commands, Command{"ip", "netns", "exec", p.Roles[role].Namespace, "sysctl", "-q", "-w", "net.ipv6.conf.all.disable_ipv6=1", "net.ipv6.conf.default.disable_ipv6=1"})
	}
	require.Equal(t, Command{"ip", "netns", "add", p.FabricNamespace}, commands[0], "namespace marker must precede host links for stale cleanup")
	for _, command := range commands {
		text := strings.Join(command, " ")
		require.NotContains(t, text, "route add default")
		require.NotContains(t, text, "tc ")
		if strings.Contains(text, "iptables") || strings.Contains(text, "sysctl") {
			require.Equal(t, []string{"ip", "netns", "exec"}, []string(command[:3]))
		}
	}
	media := netip.AddrPortFrom(p.Roles["relay"].Public, 32000)
	rules, err := p.MediaCommands(media)
	require.NoError(t, err)
	require.Len(t, rules, 4)
	require.Contains(t, strings.Join(rules[2], " "), "-p udp -d "+media.Addr().String()+" --dport 32000 -j ACCEPT")
	for _, bad := range []netip.AddrPort{netip.AddrPortFrom(media.Addr(), 0), netip.AddrPortFrom(media.Addr(), 6379), netip.AddrPortFrom(p.Roles["relay"].Private, 32000)} {
		_, err := p.MediaCommands(bad)
		require.Error(t, err)
	}
	p2, err := NewPlan(501, 12345, "aabbccddef", 2)
	require.NoError(t, err)
	require.NotEqual(t, p.ID, p2.ID)
	require.NotEqual(t, p.DCBridge, p2.DCBridge)
}
func TestStaleCleanupOwnership(t *testing.T) {
	p := testPlan(t)
	id, prefix, stale := StaleOwner(p.Roles["relay"].Namespace, 501, func(int) bool { return false })
	require.True(t, stale)
	require.Equal(t, p.ID, id)
	require.Equal(t, p.LinkPrefix, prefix)
	for _, name := range []string{"unrelated", p.ID, p.ID + "-arbitrary", p.Roles["relay"].Namespace + "-extra"} {
		_, _, stale := StaleOwner(name, 501, func(int) bool { return false })
		require.False(t, stale, name)
	}
	_, _, stale = StaleOwner(p.Roles["relay"].Namespace, 0, func(int) bool { return false })
	require.False(t, stale)
	_, _, stale = StaleOwner(p.Roles["relay"].Namespace, 501, func(int) bool { return true })
	require.False(t, stale, "never clean a concurrent run or reused living PID")
	_, err := NewPlan(0, 1, "../bad", 2)
	require.Error(t, err)
	_, err = NewPlan(0, 1, "aabbccddee", 201)
	require.Error(t, err)
}
func TestRouteOverlap(t *testing.T) {
	p := testPlan(t)
	require.False(t, overlapsRoutes(p, "default via 172.17.0.1 dev eth0\n172.17.0.0/16 dev eth0"))
	for _, route := range []string{p.Datacentre.String() + " dev bridge", p.CallerNet.String() + " dev bridge", "10.0.0.0/8 dev tun0", "local " + p.Host.String() + " dev bridge table local"} {
		require.True(t, overlapsRoutes(p, route), route)
	}
}
func TestCleanupUsesFreshContextAndOnlyOwnedLink(t *testing.T) {
	var commands []Command
	failure := errors.New("injected deletion failure")
	topology := &Topology{runner: func(ctx context.Context, command Command) (string, error) {
		require.NoError(t, ctx.Err())
		commands = append(commands, command)
		if command[2] == "delete" {
			return "", failure
		}
		return "", nil
	}}
	require.ErrorIs(t, topology.removeLink(testPlan(t).DCBridge), failure)
	require.Equal(t, []Command{{"ip", "link", "show", "dev", testPlan(t).DCBridge}, {"ip", "link", "delete", testPlan(t).DCBridge}}, commands)
	topology.runner = func(context.Context, Command) (string, error) {
		return "Device does not exist", errors.New("link absent")
	}
	require.NoError(t, topology.removeLink(testPlan(t).DCBridge))
}

func TestOwnedLinkNames(t *testing.T) {
	p := testPlan(t)
	for _, name := range []string{p.DCBridge, p.CallerBridge, p.ManagementHost, p.ManagementPeer, p.Links[0].HostLink, p.Links[0].PeerLink} {
		require.True(t, OwnedLink(name, p.LinkPrefix), name)
	}
	for _, name := range []string{"docker0", p.LinkPrefix, p.LinkPrefix + "unrelated", p.ManagementHost + "extra", "rn0011223344d"} {
		require.False(t, OwnedLink(name, p.LinkPrefix), name)
	}
	require.False(t, OwnedLink("docker0", ""))
}
