package nettopology

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const tcpFixture = `Ip: Forwarding
Ip: 1
Tcp: RtoAlgorithm RetransSegs
Tcp: 1 7
TcpExt: TCPLossProbes TCPLossProbeRecovery TCPTimeouts TCPSynRetrans TCPFastRetrans TCPSpuriousRTOs
TcpExt: 3 2 1 4 5 6
`

func TestTCPCountersMissingMalformedAndUnrelatedSections(t *testing.T) {
	counters, missing, err := ParseTCPCounters(tcpFixture)
	require.NoError(t, err)
	require.Empty(t, missing)
	require.Equal(t, uint64(7), counters["TcpRetransSegs"])
	require.Equal(t, uint64(3), counters["TCPLossProbes"])
	counters, missing, err = ParseTCPCounters("Tcp: RetransSegs\nTcp: 0\n")
	require.NoError(t, err)
	require.Len(t, missing, 6)
	require.Equal(t, map[string]uint64{"TcpRetransSegs": 0}, counters)
	for _, raw := range []string{"Tcp: RetransSegs\n", "Tcp: RetransSegs\nTcp: -1\n", "Tcp: RetransSegs\nTcp: 1 2\n"} {
		_, _, err = ParseTCPCounters(raw)
		require.Error(t, err)
	}
}
func TestTCPReadsOwnedNamespacesAndDriverWithoutMutations(t *testing.T) {
	p, err := NewPlan(0, 123, "aabbccddee", 2)
	require.NoError(t, err)
	var commands []Command
	topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) {
		commands = append(commands, c)
		return tcpFixture, nil
	}}
	snapshots := topology.SnapshotTCP(context.Background())
	require.Len(t, snapshots, len(p.Roles)+2)
	for i, s := range snapshots {
		require.NoError(t, err)
		require.Empty(t, s.Error)
		require.Empty(t, s.Missing)
		require.Equal(t, uint64(7), s.Counters["TcpRetransSegs"])
		command := commands[i]
		require.Equal(t, []string{"cat", "/proc/net/snmp", "/proc/net/netstat"}, []string(command[len(command)-3:]))
		if s.Role == "driver" {
			require.Len(t, command, 3)
		} else {
			require.Equal(t, []string{"ip", "netns", "exec", s.Namespace}, []string(command[:4]))
		}
	}
	topology.runner = func(context.Context, Command) (string, error) { return "", errors.New("read failed") }
	for _, s := range topology.SnapshotTCP(context.Background()) {
		require.Contains(t, s.Error, "read failed")
		require.Empty(t, s.Counters)
	}
}
func TestPrivateRouteTuningScopeAndObservedSettings(t *testing.T) {
	p, err := NewPlan(0, 123, "aabbccddee", 2)
	require.NoError(t, err)
	var commands []Command
	topology := &Topology{Plan: p, runner: func(_ context.Context, c Command) (string, error) {
		commands = append(commands, c)
		return "rto_min lock 5ms quickack 1", nil
	}}
	settings, err := topology.TunePrivateRoutes(context.Background())
	require.NoError(t, err)
	require.Len(t, settings, 5)
	for _, c := range commands {
		require.Equal(t, "ip", c[0])
		require.Equal(t, "-n", c[1])
		require.True(t, strings.HasPrefix(c[2], p.ID+"-"))
		require.NotEqual(t, p.Roles["caller"].Namespace, c[2])
		require.NotEqual(t, p.FabricNamespace, c[2])
		require.Equal(t, p.Datacentre.String(), c[5])
	}
	topology.runner = func(context.Context, Command) (string, error) { return "rto_min lock 200ms", nil }
	_, err = topology.TunePrivateRoutes(context.Background())
	require.ErrorContains(t, err, "not observed")
}
