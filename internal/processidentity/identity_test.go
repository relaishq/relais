package processidentity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/socketbind"
	"github.com/stretchr/testify/require"
)

func TestCurrentStable(t *testing.T) {
	a, err := Current()
	require.NoError(t, err)
	b, err := Current()
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.NotEmpty(t, a.Start)
}

func TestLinuxHostDiscoveryAndOverride(t *testing.T) {
	reads := 0
	missing := func(string) ([]byte, error) { reads++; return nil, os.ErrNotExist }
	require.Equal(t, UnknownHost, linuxHost("", missing))
	require.Equal(t, 2, reads)
	reads = 0
	require.Equal(t, "host-id:unique-host", linuxHost("unique-host", missing))
	require.Zero(t, reads, "override must bypass machine ID discovery")
	for _, primary := range []string{"", "primary"} {
		host := linuxHost("", func(path string) ([]byte, error) {
			if path == "/etc/machine-id" {
				return []byte(primary), nil
			}
			return []byte(" fallback\n"), nil
		})
		want := "linux:primary"
		if primary == "" {
			want = "linux:fallback"
		}
		require.Equal(t, want, host)
	}
}

func TestExplicitHostIdentityAndUnknownRefusal(t *testing.T) {
	local, err := CurrentWithHostID(" unique-host ")
	require.NoError(t, err)
	require.Equal(t, "host-id:unique-host", local.Host)
	require.NotEmpty(t, local.Namespace)
	require.NotEmpty(t, local.Executable)
	live, err := GoneFrom(context.Background(), local, local)
	require.NoError(t, err)
	require.False(t, live, "fencing checks must use the configured local host ID")
	target := local
	target.Host = UnknownHost
	for _, id := range []Identity{local, target} {
		exited, err := GoneFrom(context.Background(), id, target)
		require.ErrorIs(t, err, ErrForeignScope)
		require.False(t, exited)
		require.ErrorIs(t, FenceFrom(context.Background(), id, target), ErrForeignScope)
	}
}
func TestFenceMismatchSendsNoSignal(t *testing.T) {
	signalled := false
	err := fence(context.Background(), Identity{PID: 42, Start: "old"}, func(int) (string, bool, error) { return "new", false, nil }, func(int) error { signalled = true; return nil })
	require.NoError(t, err)
	require.False(t, signalled)
}
func TestFenceReadOrSignalFailure(t *testing.T) {
	denied := syscall.EPERM
	for _, readErr := range []error{denied, nil} {
		signals := 0
		err := fence(context.Background(), Identity{PID: 42, Start: "old"}, func(int) (string, bool, error) { return "old", false, readErr }, func(int) error { signals++; return denied })
		require.ErrorIs(t, err, denied)
		if readErr != nil {
			require.Zero(t, signals)
		} else {
			require.Equal(t, 1, signals)
		}
	}
}
func TestFenceStoppedProcess(t *testing.T) {
	if os.Getenv("RELAIS_PROCESSIDENTITY_STOPPED_CHILD") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFenceStoppedProcess$")
	cmd.Env = append(os.Environ(), "RELAIS_PROCESSIDENTITY_STOPPED_CHILD=1")
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	require.NoError(t, cmd.Process.Signal(syscall.SIGSTOP))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, Fence(ctx, identityForPID(t, cmd.Process.Pid)))
	// SIGCONT cannot resurrect the fenced process. Do not send to a reaped PID.
	select {
	case err := <-done:
		require.Error(t, err)
		done <- err
	case <-ctx.Done():
		t.Fatal("stopped process did not exit")
	}
}
func TestRealMismatchedStartKeepsChildAlive(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	err := Fence(context.Background(), identityForPIDStart(t, cmd.Process.Pid, "wrong"))
	require.NoError(t, err)
	require.NoError(t, cmd.Process.Signal(syscall.Signal(0)))
}

func TestFenceMissingProcessSendsNoSignal(t *testing.T) {
	for _, errno := range []error{syscall.ENOENT, syscall.ESRCH} {
		t.Run(errno.Error(), func(t *testing.T) {
			signals := 0
			err := fence(context.Background(), Identity{PID: 42, Start: "old"},
				func(int) (string, bool, error) {
					return "", false, &os.PathError{Op: "read", Path: "/proc/42/stat", Err: errno}
				}, func(int) error { signals++; return nil })
			require.NoError(t, err)
			require.Zero(t, signals, "an absent PID must never be signalled")
		})
	}
}

func TestFenceExitAfterSignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		dead bool
	}{
		{name: "ENOENT", err: &os.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.ENOENT}},
		{name: "ESRCH", err: &os.PathError{Op: "read", Path: "/proc/42/stat", Err: syscall.ESRCH}},
		{name: "zombie", dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, signals := 0, 0
			err := fence(context.Background(), Identity{PID: 42, Start: "old"},
				func(int) (string, bool, error) {
					reads++
					if reads == 1 {
						return "old", false, nil
					}
					return "old", tc.dead, tc.err
				}, func(int) error { signals++; return nil })
			require.NoError(t, err)
			require.Equal(t, 1, signals)
			require.Equal(t, 2, reads, "exit must not depend on parent reaping")
		})
	}
}

func TestFenceWaitStillFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, start string
		dead        bool
		err, want   error
	}{
		{name: "permission", err: syscall.EACCES, want: syscall.EACCES},
		{name: "reused live PID", start: "new"},
		{name: "reused zombie PID", start: "new", dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			err := fence(context.Background(), Identity{PID: 42, Start: "old"},
				func(int) (string, bool, error) {
					reads++
					if reads == 1 {
						return "old", false, nil
					}
					return tc.start, tc.dead, tc.err
				}, func(int) error { return nil })
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}

func TestFenceUnreapedProcessReleasesSockets(t *testing.T) {
	type addresses struct{ UDP, TCP string }
	if os.Getenv("RELAIS_PROCESSIDENTITY_SOCKET_CHILD") == "1" {
		udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = udp.Close() }()
		tcp, err := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, err)
		defer func() { _ = tcp.Close() }()
		require.NoError(t, json.NewEncoder(os.Stdout).Encode(addresses{UDP: udp.LocalAddr().String(), TCP: tcp.Addr().String()}))
		for {
			time.Sleep(time.Hour)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFenceUnreapedProcessReleasesSockets$")
	cmd.Env = append(os.Environ(), "RELAIS_PROCESSIDENTITY_SOCKET_CHILD=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// Do not start cmd.Wait: the target must remain a zombie through all the
	// assertions, as when the standby is not the old relay's parent.
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan struct {
		addresses
		err error
	}, 1)
	go func() {
		var addr addresses
		err := json.NewDecoder(stdout).Decode(&addr)
		ready <- struct {
			addresses
			err error
		}{addr, err}
	}()
	var addr addresses
	select {
	case reply := <-ready:
		require.NoError(t, reply.err)
		addr = reply.addresses
	case <-time.After(5 * time.Second):
		t.Fatal("socket-owning child did not become ready")
	}
	udp, err := net.ListenPacket("udp4", addr.UDP)
	if udp != nil {
		_ = udp.Close()
	}
	require.Error(t, err, "live child must exclusively own its UDP socket")
	tcp, err := net.Listen("tcp4", addr.TCP)
	if tcp != nil {
		_ = tcp.Close()
	}
	require.Error(t, err, "live child must exclusively own its TCP socket")
	start, dead, err := inspect(cmd.Process.Pid)
	require.NoError(t, err)
	require.False(t, dead)
	require.NoError(t, cmd.Process.Signal(syscall.SIGSTOP))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, Fence(ctx, identityForPID(t, cmd.Process.Pid)))
	current, dead, err := inspect(cmd.Process.Pid)
	require.NoError(t, err, "child must still exist without parent reaping")
	require.Equal(t, start, current)
	require.True(t, dead, "Fence must return for an unreaped zombie")
	exited, err := Gone(ctx, identityForPIDStart(t, cmd.Process.Pid, start))
	require.NoError(t, err)
	require.True(t, exited, "dead-holder check must recognize an unreaped zombie")
	bindCtx, stopBind := context.WithTimeout(context.Background(), socketbind.DefaultTimeout)
	defer stopBind()
	reported := false
	udp, udpWait, err := socketbind.Retry(bindCtx, func() (net.PacketConn, error) {
		socket, err := net.ListenPacket("udp4", addr.UDP)
		if errors.Is(err, syscall.EADDRINUSE) && !reported {
			reported = true
			logSocketExitState(t, cmd.Process.Pid, addr.UDP)
		}
		return socket, err
	})
	require.NoError(t, err, "fenced child must release UDP within the bind budget without reaping")
	defer func() { _ = udp.Close() }()
	tcp, tcpWait, err := socketbind.Retry(bindCtx, func() (net.Listener, error) { return net.Listen("tcp4", addr.TCP) })
	require.NoError(t, err, "fenced child must release TCP within the shared bind budget without reaping")
	defer func() { _ = tcp.Close() }()
	current, dead, err = inspect(cmd.Process.Pid)
	require.NoError(t, err)
	require.Equal(t, start, current)
	require.True(t, dead, "socket release must not require parent reaping")
	t.Logf("fenced unreaped child bind_wait_ms=%.3f", float64(udpWait+tcpWait)/float64(time.Millisecond))
}

func TestFenceReusedOwnPIDAndPIDOne(t *testing.T) {
	for _, pid := range []int{os.Getpid(), 1} {
		signals := 0
		err := fence(context.Background(), Identity{PID: pid, Start: "previous-container-or-boot"},
			func(int) (string, bool, error) { return "current", false, nil }, func(int) error { signals++; return nil })
		require.NoError(t, err)
		require.Zero(t, signals)
	}
}

func TestFenceMatchingOtherExecutableFailsClosed(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	start, _, err := inspect(cmd.Process.Pid)
	require.NoError(t, err)
	id, err := Current()
	require.NoError(t, err)
	id.PID, id.Start = cmd.Process.Pid, start
	err = Fence(context.Background(), id)
	require.ErrorIs(t, err, ErrExecutable)
	require.NoError(t, cmd.Process.Signal(syscall.Signal(0)))
}
func TestFenceOwnCurrentIdentityFailsClosed(t *testing.T) {
	id, err := Current()
	require.NoError(t, err)
	require.ErrorContains(t, Fence(context.Background(), id), "current process")
}

func TestFenceOwnPreviousStartAlreadyFenced(t *testing.T) {
	id, err := Current()
	require.NoError(t, err)
	id.Start = "previous-container:" + id.Start
	require.NoError(t, Fence(context.Background(), id))
}

func TestGoneUsesIdentityWithoutSignalling(t *testing.T) {
	for _, tc := range []struct {
		name, start     string
		dead            bool
		readErr, exeErr error
		want            bool
	}{
		{name: "absent ESRCH", readErr: syscall.ESRCH, want: true},
		{name: "absent ENOENT", readErr: os.ErrNotExist, want: true},
		{name: "zombie", start: "old", dead: true, want: true},
		{name: "reused PID", start: "new", want: true},
		{name: "live or stopped", start: "old"},
		{name: "read EPERM", readErr: syscall.EPERM},
		{name: "executable EPERM", start: "old", exeErr: syscall.EPERM},
		{name: "other executable", start: "old", exeErr: ErrExecutable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			exited, err := gone(context.Background(), Identity{PID: 42, Start: "old"},
				func(int) (string, bool, error) { return tc.start, tc.dead, tc.readErr }, func(int) error { checks++; return tc.exeErr })
			require.Equal(t, tc.want, exited)
			if tc.readErr != nil && !tc.want {
				require.ErrorIs(t, err, tc.readErr)
			} else if tc.exeErr != nil {
				require.ErrorIs(t, err, tc.exeErr)
			} else {
				require.NoError(t, err)
			}
			if tc.want || tc.readErr != nil {
				require.Zero(t, checks)
			} else {
				require.Equal(t, 1, checks)
			}
		})
	}
}
func TestGoneLiveCurrentProcess(t *testing.T) {
	id, err := Current()
	require.NoError(t, err)
	exited, err := Gone(context.Background(), id)
	require.NoError(t, err)
	require.False(t, exited)
	id.Start = "previous:" + id.Start
	exited, err = Gone(context.Background(), id)
	require.NoError(t, err)
	require.True(t, exited)
}

func identityForPIDStart(t *testing.T, pid int, start string) Identity {
	t.Helper()
	id, err := Current()
	require.NoError(t, err)
	id.PID, id.Start = pid, start
	return id
}
func identityForPID(t *testing.T, pid int) Identity {
	t.Helper()
	start, _, err := inspect(pid)
	require.NoError(t, err)
	id := identityForPIDStart(t, pid, start)
	id.Executable, err = executableIdentity(pid)
	require.NoError(t, err)
	return id
}

func TestForeignScopeCannotBeGoneOrFenced(t *testing.T) {
	local, err := Current()
	require.NoError(t, err)
	for _, field := range []string{"host", "namespace", "missing"} {
		t.Run(field, func(t *testing.T) {
			id := local
			id.PID, id.Start = 2147483647, "unseen-start"
			switch field {
			case "host":
				id.Host = "another-host"
			case "namespace":
				id.Namespace = "another-namespace"
			case "missing":
				id.Host = ""
			}
			exited, err := Gone(context.Background(), id)
			require.False(t, exited)
			require.ErrorIs(t, err, ErrForeignScope)
			require.ErrorIs(t, Fence(context.Background(), id), ErrForeignScope)
		})
	}
}
func TestSameMachinePreviousBootIsNotForeignNamespace(t *testing.T) {
	local := Identity{Host: "same-machine", Namespace: "current-namespace", Start: "12345678-1234-1234-1234-123456789abc:42"}
	old := Identity{Host: local.Host, Namespace: "old-namespace", Start: "87654321-1234-1234-1234-123456789abc:42"}
	require.NoError(t, SameScope(local, old))
	old.Start = local.Start
	require.ErrorIs(t, SameScope(local, old), ErrForeignScope)
	old.Start = "unknown:42"
	require.ErrorIs(t, SameScope(local, old), ErrForeignScope)
}
func TestPinnedUnreadableIdentityDoesNotProveExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe error
		want  bool
	}{
		{"hidden live process", nil, false},
		{"permission denied", syscall.EPERM, false},
		{"confirmed exit", syscall.ESRCH, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			read := func(pid int) (string, bool, error) {
				return inspectPinnedWith(pid, func(int) (string, bool, error) { return "", false, os.ErrNotExist }, func() error { return nil }, func() error { probes++; return tc.probe })
			}
			exited, err := gone(context.Background(), Identity{PID: 42, Start: "old"}, read, func(int) error { t.Fatal("hidden identity cannot reach executable verification"); return nil })
			require.Equal(t, tc.want, exited)
			if tc.want {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.False(t, errors.Is(err, os.ErrNotExist), "hidden proc entry must not masquerade as confirmed absence")
			}
			require.Equal(t, 1, probes)
		})
	}
}
func TestFenceUpgradedExecutable(t *testing.T) {
	if os.Getenv("RELAIS_UPGRADE_ACTIVE") == "1" {
		fmt.Println("UPGRADE_ACTIVE_READY")
		for {
			time.Sleep(time.Hour)
		}
	}
	if raw := os.Getenv("RELAIS_UPGRADE_FENCE"); raw != "" {
		var id Identity
		require.NoError(t, json.Unmarshal([]byte(raw), &id))
		self, err := Current()
		require.NoError(t, err)
		require.NotEqual(t, id.Executable, self.Executable, "standby must use the replacement inode")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, Fence(ctx, id), "upgraded standby must fence the recorded old executable")
		return
	}
	data, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "relay")
	require.NoError(t, os.WriteFile(path, data, 0700))
	active := exec.Command(path, "-test.run=^TestFenceUpgradedExecutable$")
	active.Env = append(os.Environ(), "RELAIS_UPGRADE_ACTIVE=1", "GORACE=atexit_sleep_ms=0")
	stdout, err := active.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, active.Start())
	t.Cleanup(func() { _ = active.Process.Kill(); _ = active.Wait() })
	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan())
	require.Equal(t, "UPGRADE_ACTIVE_READY", scanner.Text())
	id := identityForPID(t, active.Process.Pid)
	require.NoError(t, active.Process.Signal(syscall.SIGSTOP))
	replacement := path + ".new"
	require.NoError(t, os.WriteFile(replacement, data, 0700))
	require.NoError(t, os.Rename(replacement, path))
	actual, err := executableIdentity(active.Process.Pid)
	require.NoError(t, err)
	require.Equal(t, id.Executable, actual, "mapped old inode survives path replacement")
	recorded, err := json.Marshal(id)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	standby := exec.CommandContext(ctx, path, "-test.run=^TestFenceUpgradedExecutable$")
	standby.Env = append(os.Environ(), "RELAIS_UPGRADE_FENCE="+string(recorded), "GORACE=atexit_sleep_ms=0")
	output, err := standby.CombinedOutput()
	require.NoError(t, err, string(output))
}
