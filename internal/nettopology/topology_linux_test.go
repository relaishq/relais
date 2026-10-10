//go:build linux

package nettopology

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func requireLinuxNamespaces(t *testing.T) {
	t.Helper()
	if os.Getenv("RELAIS_TEST_NETNS_REQUIRE") != "1" {
		t.Skip("set RELAIS_TEST_NETNS_REQUIRE=1 and run the built test binary as root")
	}
	require.Equal(t, 0, os.Geteuid(), "namespace tests require root/CAP_SYS_ADMIN and CAP_NET_ADMIN")
}
func TestLinuxIsolationAndCleanup(t *testing.T) {
	requireLinuxNamespaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	topology, err := Open(ctx, t.Logf)
	require.NoError(t, err)
	defer topology.Close()
	var private []string
	for _, role := range []string{"relay", "worker-0", "worker-1", "control", "store"} {
		var listener net.Listener
		err := withNamespace(topology.Plan.Roles[role].Namespace, func() error {
			var err error
			listener, err = net.Listen("tcp4", net.JoinHostPort(topology.Plan.Roles[role].Private.String(), "0"))
			return err
		})
		require.NoError(t, err)
		defer listener.Close()
		private = append(private, listener.Addr().String())
	}
	require.NoError(t, topology.VerifyPrivateAPIs(ctx, private))
	var publicTCP net.Listener
	require.NoError(t, withNamespace(topology.Plan.Roles["relay"].Namespace, func() error {
		var err error
		publicTCP, err = net.Listen("tcp4", net.JoinHostPort(topology.Plan.Roles["relay"].Public.String(), "0"))
		return err
	}))
	defer publicTCP.Close()
	require.NoError(t, withNamespace(topology.Plan.Roles["caller"].Namespace, func() error {
		conn, err := net.DialTimeout("tcp4", publicTCP.Addr().String(), 150*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return errors.New("caller reached relay public TCP listener")
		}
		return nil
	}))
	makeEcho := func() net.PacketConn {
		var conn net.PacketConn
		require.NoError(t, withNamespace(topology.Plan.Roles["relay"].Namespace, func() error {
			var err error
			conn, err = net.ListenPacket("udp4", net.JoinHostPort(topology.Plan.Roles["relay"].Public.String(), "0"))
			return err
		}))
		t.Cleanup(func() { _ = conn.Close() })
		go func() {
			buffer := make([]byte, 64)
			for {
				n, addr, err := conn.ReadFrom(buffer)
				if err != nil {
					return
				}
				_, _ = conn.WriteTo(buffer[:n], addr)
			}
		}()
		return conn
	}
	allowed, blocked := makeEcho(), makeEcho()
	endpoint, err := netip.ParseAddrPort(allowed.LocalAddr().String())
	require.NoError(t, err)
	require.NoError(t, topology.AllowMedia(ctx, endpoint))
	caller, err := topology.CallerSocket()
	require.NoError(t, err)
	defer caller.Close()
	require.Equal(t, topology.Plan.Roles["caller"].Public.String(), caller.LocalAddr().(*net.UDPAddr).IP.String())
	require.NoError(t, caller.SetDeadline(time.Now().Add(time.Second)))
	_, err = caller.WriteTo([]byte("media"), allowed.LocalAddr())
	require.NoError(t, err)
	buffer := make([]byte, 64)
	n, _, err := caller.ReadFrom(buffer)
	require.NoError(t, err)
	require.Equal(t, "media", string(buffer[:n]))
	require.NoError(t, caller.SetDeadline(time.Now().Add(150*time.Millisecond)))
	_, err = caller.WriteTo([]byte("forbidden"), blocked.LocalAddr())
	if err != nil {
		// OUTPUT filtering may reject the datagram before it leaves the caller.
		require.True(t, errors.Is(err, syscall.EPERM), "unexpected blocked-port send error: %v", err)
	} else {
		_, _, err = caller.ReadFrom(buffer)
		var netErr net.Error
		require.ErrorAs(t, err, &netErr, "only the selected media port may reply")
		require.True(t, netErr.Timeout(), "unexpected blocked-port receive error: %v", err)
	}
	// Real children must execute directly in the namespace with unchanged PID.
	manager := &clusterprocess.Manager{}
	require.NoError(t, manager.AddCleanup(func() { require.NoError(t, topology.Close()) }))
	child, err := manager.StartInNamespace(t.TempDir(), "owned", topology.Plan.Roles["store"].Namespace, os.Environ(), "/bin/sleep", "60")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		data, err := runCommand(ctx, Command{"ip", "netns", "pids", topology.Plan.Roles["store"].Namespace})
		return err == nil && strings.Contains(" "+strings.ReplaceAll(strings.TrimSpace(data), "\n", " ")+" ", fmt.Sprintf(" %d ", child.PID()))
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, caller.Close())
	manager.Stop(true)
	require.NoError(t, topology.Close())
	assertRemoved(t, topology.Plan)
}
func TestLinuxPartialSetupCleansOwnedResources(t *testing.T) {
	requireLinuxNamespaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Get a collision-checked plan, then recreate only part of that same plan.
	complete, err := Open(ctx, t.Logf)
	require.NoError(t, err)
	require.NoError(t, complete.Close())
	plan := complete.Plan
	injected := errors.New("injected failure after first veth creation")
	partial := &Topology{Plan: plan, logf: t.Logf, runner: func(ctx context.Context, command Command) (string, error) {
		output, err := runCommand(ctx, command)
		if err == nil && len(command) > 3 && command[0] == "ip" && command[1] == "link" && command[2] == "add" && command[3] == plan.Links[0].HostLink {
			return output, injected
		}
		return output, err
	}}
	require.ErrorIs(t, partial.execute(ctx, plan.SetupCommands()), injected)
	partial.runner = runCommand
	require.NoError(t, partial.Close())
	require.NoError(t, partial.Close())
	assertRemoved(t, plan)
}
func assertRemoved(t *testing.T, plan Plan) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	namespaces, err := runCommand(ctx, Command{"ip", "netns", "list"})
	require.NoError(t, err)
	require.NotContains(t, namespaces, plan.ID)
	for _, name := range []string{plan.ManagementHost, plan.ManagementPeer, plan.DCBridge, plan.CallerBridge, plan.Links[0].HostLink, plan.Links[0].PeerLink} {
		_, err := runCommand(ctx, Command{"ip", "link", "show", "dev", name})
		require.Error(t, err)
	}
}

func TestLinuxInterruptCleanup(t *testing.T) {
	requireLinuxNamespaces(t)
	if os.Getenv("RELAIS_TEST_NETNS_SIGNAL_CHILD") == "1" {
		manager := &clusterprocess.Manager{}
		ctx, stop := manager.Context()
		defer stop()
		topology, err := OpenManaged(ctx, nil, manager.AddCleanup)
		require.NoError(t, err)
		child, err := manager.StartInNamespace(os.Getenv("RELAIS_TEST_NETNS_SIGNAL_DIR"), "leaf", topology.Plan.Roles["store"].Namespace, os.Environ(), "/bin/sh", "-c", "trap '' INT TERM; echo ready; exec sleep 60")
		require.NoError(t, err)
		require.NoError(t, child.WaitLog(ctx, "ready"))
		data, _ := json.Marshal(topology.Plan)
		fmt.Println(string(data))
		<-ctx.Done()
		fmt.Println("cleanup")
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxInterruptCleanup$", "-test.timeout=1m")
	cmd.Env = append(os.Environ(), "RELAIS_TEST_NETNS_SIGNAL_CHILD=1", "RELAIS_TEST_NETNS_SIGNAL_DIR="+dir)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	scanner := bufio.NewScanner(output)
	require.True(t, scanner.Scan())
	var plan Plan
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &plan))
	// Keep a recovery path if the signal cleanup is regressed during testing.
	defer func() { topology := &Topology{Plan: plan, runner: runCommand}; _ = topology.Close() }()
	require.NoError(t, cmd.Process.Signal(os.Interrupt))
	require.True(t, scanner.Scan())
	require.Equal(t, "cleanup", scanner.Text())
	require.NoError(t, cmd.Process.Signal(os.Interrupt))
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("double interrupt did not finish namespace cleanup")
	}
	assertRemoved(t, plan)
}

func TestLinuxStalePartialLinksAreReclaimed(t *testing.T) {
	requireLinuxNamespaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	complete, err := Open(ctx, t.Logf)
	require.NoError(t, err)
	require.NoError(t, complete.Close())
	owner := exec.Command("/bin/true")
	require.NoError(t, owner.Run())
	require.Equal(t, unix.ESRCH, unix.Kill(owner.Process.Pid, 0), "stale fixture owner must be absent")
	plan, err := NewPlan(os.Getuid(), owner.Process.Pid, strings.TrimPrefix(complete.Plan.LinkPrefix, "rn"), 2)
	require.NoError(t, err)
	recovery := &Topology{Plan: plan, runner: runCommand}
	defer recovery.Close()
	// Simulate SIGKILL immediately after veth creation, before either move.
	for _, command := range []Command{
		{"ip", "netns", "add", plan.FabricNamespace},
		{"ip", "link", "add", plan.ManagementHost, "type", "veth", "peer", "name", plan.ManagementPeer},
		{"ip", "link", "add", plan.Links[0].HostLink, "type", "veth", "peer", "name", plan.Links[0].PeerLink},
	} {
		_, err := runCommand(ctx, command)
		require.NoError(t, err)
	}
	cleaner := &Topology{runner: runCommand, logf: t.Logf}
	require.NoError(t, cleaner.cleanStale(ctx))
	assertRemoved(t, plan)
}
