//go:build linux

package nettopology

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Open requires root inside Linux; it changes only owned links/namespaces.
// It never changes the host's forwarding or firewall settings.
func Open(ctx context.Context, logf func(string, ...any)) (*Topology, error) {
	return OpenManaged(ctx, logf, nil)
}

// OpenManaged registers cleanup before setup can create any resources.
func OpenManaged(ctx context.Context, logf func(string, ...any), register func(func()) error) (*Topology, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("netns topology requires root on Linux: build first, then run sudo -E ./bin/crash-run -topology=netns -profile=lan")
	}
	for _, tool := range []string{"ip", "iptables", "sysctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("netns topology requires %s: %w", tool, err)
		}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	t := &Topology{runner: runCommand, logf: logf}
	if err := t.cleanStale(ctx); err != nil {
		return nil, err
	}
	routes, err := runCommand(ctx, Command{"ip", "-4", "route", "show", "table", "all"})
	if err != nil {
		return nil, err
	}
	found := false
	for attempt := 0; attempt < 32; attempt++ {
		var token [5]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, err
		}
		p, err := NewPlan(os.Getuid(), os.Getpid(), hex.EncodeToString(token[:]), 2)
		if err != nil {
			return nil, err
		}
		if overlapsRoutes(p, routes) {
			continue
		}
		t.Plan = p
		found = true
		break
	}
	if !found {
		return nil, errors.New("no unused private topology subnets found; use a Linux VM with no overlapping private routes")
	}
	logf("topology=%s datacentre=%s caller=%s profile=lan", t.Plan.ID, t.Plan.Datacentre, t.Plan.CallerNet)
	if register != nil {
		if err := register(func() {
			if err := t.Close(); err != nil {
				t.logf("topology cleanup failed: %v", err)
			}
		}); err != nil {
			return nil, err
		}
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, context.Canceled
	}
	setupErr := t.execute(ctx, t.Plan.SetupCommands())
	t.mu.Unlock()
	if setupErr != nil {
		cleanup := t.Close()
		return nil, errors.Join(setupErr, cleanup)
	}
	return t, nil
}
func runCommand(ctx context.Context, command Command) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s: %w: %s", strings.Join(command, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
func (t *Topology) cleanStale(ctx context.Context) error {
	output, err := t.runner(ctx, Command{"ip", "netns", "list"})
	if err != nil {
		return err
	}
	bridges := map[string]bool{}
	var staleNames []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, prefix, stale := StaleOwner(fields[0], os.Getuid(), func(pid int) bool { return unix.Kill(pid, 0) != unix.ESRCH })
		if !stale || id == t.Plan.ID {
			continue
		}
		t.logf("clean stale namespace %s", fields[0])
		staleNames = append(staleNames, fields[0])
		bridges[prefix] = true
	}
	if len(bridges) > 0 {
		output, err := t.runner(ctx, Command{"ip", "-j", "link", "show"})
		if err != nil {
			return err
		}
		var links []struct {
			Name string `json:"ifname"`
		}
		if err := json.Unmarshal([]byte(output), &links); err != nil {
			return err
		}
		for _, link := range links {
			for prefix := range bridges {
				if OwnedLink(link.Name, prefix) {
					if err := t.removeLink(link.Name); err != nil {
						t.logf("cannot clean stale link %s; continuing: %v", link.Name, err)
					}
				}
			}
		}
	}
	for _, name := range staleNames {
		if err := t.removeNamespace(name); err != nil {
			t.logf("cannot clean stale namespace %s; continuing: %v", name, err)
		}
	}
	return nil
}
func (t *Topology) removeNamespace(ns string) error {
	target, err := os.Stat("/var/run/netns/" + ns)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	output, err := t.cleanupCommand(Command{"ip", "netns", "pids", ns})
	if err != nil {
		return err
	}
	for _, field := range strings.Fields(output) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return err
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		current, statErr := os.Stat(fmt.Sprintf("/proc/%d/ns/net", pid))
		// A pidfd prevents numeric PID reuse between the namespace check and kill.
		if statErr == nil && os.SameFile(target, current) {
			err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		}
		_ = unix.Close(fd)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	if strings.TrimSpace(output) != "" {
		deadline := time.Now().Add(time.Second)
		for {
			remaining, err := t.cleanupCommand(Command{"ip", "netns", "pids", ns})
			if err != nil {
				return err
			}
			if strings.TrimSpace(remaining) == "" {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("namespace %s still has processes after SIGKILL: %s", ns, remaining)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	_, err = t.cleanupCommand(Command{"ip", "netns", "delete", ns})
	return err
}

// withNamespace confines per-thread state to a dedicated locked goroutine.
// If restoration fails, leave the thread locked so Go terminates it rather
// than returning a thread in the caller namespace to the runtime pool.
func withNamespace(namespace string, action func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		restored := true
		defer func() {
			if restored {
				runtime.UnlockOSThread()
			}
		}()
		original, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
		if err != nil {
			result <- err
			return
		}
		defer original.Close()
		target, err := os.Open("/var/run/netns/" + namespace)
		if err != nil {
			result <- err
			return
		}
		defer target.Close()
		if err = unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			result <- fmt.Errorf("enter caller namespace (root/CAP_SYS_ADMIN required): %w", err)
			return
		}
		restored = false
		err = action()
		restoreErr := unix.Setns(int(original.Fd()), unix.CLONE_NEWNET)
		restored = restoreErr == nil
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore driver network namespace: %w", restoreErr))
		}
		result <- err
	}()
	return <-result
}
func (t *Topology) CallerSocket() (net.PacketConn, error) {
	var conn *net.UDPConn
	err := withNamespace(t.Plan.Roles["caller"].Namespace, func() error {
		var err error
		conn, err = net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(t.Plan.Roles["caller"].Public, 0)))
		return err
	})
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, err
	}
	return conn, nil
}
