package nettopology

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayLossPlanKeepsSpareReachable(t *testing.T) {
	p := testPlan(t)
	p.addSpareRelay()
	spare := p.Roles["relay-spare"]
	require.Len(t, p.Links, 9)
	require.True(t, p.Datacentre.Contains(spare.Private))
	require.True(t, p.CallerNet.Contains(spare.Public))
	for name, role := range p.Roles {
		if name != "relay-spare" {
			require.NotEqual(t, role.Namespace, spare.Namespace)
			require.NotEqual(t, role.Private, spare.Private)
			require.NotEqual(t, role.Public, spare.Public)
		}
	}
	_, _, stale := StaleOwner(spare.Namespace, 501, func(int) bool { return false })
	require.True(t, stale, "interrupted setup must recognize the spare for cleanup")
	commands := p.SetupCommands()
	require.Contains(t, commands, Command{"ip", "netns", "add", spare.Namespace})
	require.Contains(t, commands, Command{"ip", "netns", "exec", spare.Namespace, "sysctl", "-q", "-w", "net.ipv4.ip_forward=0"})
	media := netip.AddrPortFrom(p.Roles["relay"].Public, 32000)
	other := netip.AddrPortFrom(spare.Public, 32001)
	rules, err := p.MediaCommands(media, other)
	require.NoError(t, err)
	require.Len(t, rules, 6, "flush once, then allow both media paths")
	require.Contains(t, strings.Join(rules[2], " "), "-d "+media.Addr().String()+" --dport 32000")
	require.Contains(t, strings.Join(rules[4], " "), "-d "+other.Addr().String()+" --dport 32001")
	for _, bad := range []netip.AddrPort{
		netip.AddrPortFrom(spare.Private, 32001),
		netip.AddrPortFrom(p.Roles["caller"].Public, 32001),
		netip.AddrPortFrom(spare.Public, 6379),
	} {
		_, err := p.MediaCommands(media, bad)
		require.Error(t, err)
	}
	_, err = p.MediaCommands()
	require.Error(t, err)
}

func TestRelayBlackholeCoversBothDirectionsOnlyInLostNamespace(t *testing.T) {
	p := testPlan(t)
	p.addSpareRelay()
	for _, block := range []bool{true, false} {
		var commands []Command
		topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) {
			commands = append(commands, c)
			return "", nil
		}}
		require.NoError(t, topology.BlackholeRelay(t.Context(), block))
		require.Len(t, commands, 2)
		for i, chain := range []string{"INPUT", "OUTPUT"} {
			expected := Command{"ip", "netns", "exec", p.Roles["relay"].Namespace, "iptables", "-w", "2"}
			if block {
				expected = append(expected, "-I", chain, "1")
			} else {
				expected = append(expected, "-D", chain)
			}
			expected = append(expected, "-j", "DROP")
			require.Equal(t, expected, commands[i], "no protocol/address filter: includes public media, private legs, HTTP and established Redis")
		}
	}
	failure := errors.New("iptables failed")
	topology := &Topology{Plan: p, runner: func(context.Context, Command) (string, error) { return "", failure }}
	require.ErrorIs(t, topology.BlackholeRelay(t.Context(), true), failure)
	topology.closed = true
	require.ErrorContains(t, topology.BlackholeRelay(t.Context(), true), "closed")
}
