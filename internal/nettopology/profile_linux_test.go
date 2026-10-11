//go:build linux

package nettopology

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLinuxProfilesApplyMeasureAndCleanUp(t *testing.T) {
	requireLinuxNamespaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	rootBefore, err := runCommand(ctx, Command{"tc", "qdisc", "show"})
	require.NoError(t, err)
	topology, err := Open(ctx, t.Logf)
	require.NoError(t, err)
	defer topology.Close()
	rootWithTopology, err := runCommand(ctx, Command{"tc", "qdisc", "show"})
	require.NoError(t, err)
	// Bind an echo socket in the relay namespace; caller packets traverse the
	// real owned public veth pair. No root interface receives a qdisc.
	var server *net.UDPConn
	require.NoError(t, withNamespace(topology.Plan.Roles["relay"].Namespace, func() error {
		var err error
		server, err = net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(topology.Plan.Roles["relay"].Public, 0)))
		return err
	}))
	defer server.Close()
	endpoint := server.LocalAddr().(*net.UDPAddr).AddrPort()
	require.NoError(t, topology.AllowMedia(ctx, endpoint))
	echoed := make(chan struct{})
	go func() {
		defer close(echoed)
		var b [100]byte
		for {
			n, addr, err := server.ReadFromUDPAddrPort(b[:])
			if err != nil {
				return
			}
			_, _ = server.WriteToUDPAddrPort(b[:n], addr)
		}
	}()
	caller, err := topology.CallerSocket()
	require.NoError(t, err)
	defer caller.Close()
	for _, name := range []string{"region", "far-caller", "lossy-dc", "lan"} {
		settings, err := topology.ApplyProfile(ctx, name)
		require.NoError(t, err)
		for _, s := range settings {
			if s.Delay > 0 {
				require.Contains(t, s.Qdisc, "netem")
			} else {
				require.NotContains(t, s.Qdisc, "netem")
			}
		}
		// Loss is stochastic; use eight attempts and test the mean of actual
		// replies. Bounds detect doubled RTT or a shaping path that was bypassed.
		var samples []time.Duration
		for i := 0; i < 8; i++ {
			require.NoError(t, caller.SetDeadline(time.Now().Add(350*time.Millisecond)))
			start := time.Now()
			_, err := caller.WriteTo([]byte{byte(i)}, net.UDPAddrFromAddrPort(endpoint))
			require.NoError(t, err)
			var b [100]byte
			_, _, err = caller.ReadFrom(b[:])
			if err == nil {
				samples = append(samples, time.Since(start))
			}
		}
		require.GreaterOrEqual(t, len(samples), 4)
		// Fixed region delay is 10ms in each direction. Far caller has ±10ms
		// in each direction; allow Linux timer granularity without hiding 2x RTT.
		var sum time.Duration
		for _, sample := range samples {
			sum += sample
		}
		mean := sum / time.Duration(len(samples))
		if name == "region" {
			require.Greater(t, mean, 17*time.Millisecond)
			require.Less(t, mean, 45*time.Millisecond)
		}
		if name == "far-caller" {
			require.Greater(t, mean, 110*time.Millisecond)
			require.Less(t, mean, 230*time.Millisecond)
		}
		t.Logf("profile=%s measured_rtt=%s replies=%d/8", name, mean, len(samples))
	}
	rootAfter, err := runCommand(ctx, Command{"tc", "qdisc", "show"})
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(rootWithTopology), strings.TrimSpace(rootAfter))
	require.NoError(t, caller.Close())
	require.NoError(t, server.Close())
	<-echoed
	require.NoError(t, topology.Close())
	assertRemoved(t, topology.Plan)
	rootCleaned, err := runCommand(ctx, Command{"tc", "qdisc", "show"})
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(rootBefore), strings.TrimSpace(rootCleaned))
}

func TestLinuxProfileBarrierLossIsScopedAndReversible(t *testing.T) {
	requireLinuxNamespaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	topology, err := Open(ctx, t.Logf)
	require.NoError(t, err)
	t.Cleanup(func() { _ = topology.Close() })
	openSocket := func(role string) *net.UDPConn {
		var conn *net.UDPConn
		require.NoError(t, withNamespace(topology.Plan.Roles[role].Namespace, func() error {
			var err error
			conn, err = net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(topology.Plan.Roles[role].Private, 0)))
			return err
		}))
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	source := openSocket("relay")
	destinations := []*net.UDPConn{openSocket("worker-0"), openSocket("worker-1")}
	remove, err := topology.BarrierLoss(ctx)
	require.NoError(t, err)
	defer func() { _ = remove() }()
	for _, destination := range destinations {
		addr := destination.LocalAddr().(*net.UDPAddr).AddrPort()
		// The real barrier wire prefix is RLS\x01. Transmission must succeed
		// just as on a lossy link; only the receiver observes the missing marker.
		_, err = source.WriteToUDPAddrPort([]byte("RLS\x01\x0012345678"), addr)
		require.NoError(t, err)
		require.NoError(t, destination.SetReadDeadline(time.Now().Add(25*time.Millisecond)))
		var b [100]byte
		_, _, err = destination.ReadFromUDPAddrPort(b[:])
		var timeout net.Error
		require.ErrorAs(t, err, &timeout)
		require.True(t, timeout.Timeout())
		_, err = source.WriteToUDPAddrPort([]byte("media"), addr)
		require.NoError(t, err)
		require.NoError(t, destination.SetReadDeadline(time.Now().Add(time.Second)))
		n, _, err := destination.ReadFromUDPAddrPort(b[:])
		require.NoError(t, err)
		require.Equal(t, "media", string(b[:n]))
	}
	require.NoError(t, remove())
	for _, destination := range destinations {
		addr := destination.LocalAddr().(*net.UDPAddr).AddrPort()
		_, err = source.WriteToUDPAddrPort([]byte("RLS\x01\x0012345678"), addr)
		require.NoError(t, err)
		require.NoError(t, destination.SetReadDeadline(time.Now().Add(time.Second)))
		var b [100]byte
		n, _, err := destination.ReadFromUDPAddrPort(b[:])
		require.NoError(t, err)
		require.Equal(t, "RLS\x01\x0012345678", string(b[:n]))
	}
}
