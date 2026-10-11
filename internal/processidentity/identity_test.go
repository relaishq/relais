package processidentity

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

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
func TestFenceMismatchSendsNoSignal(t *testing.T) {
	signalled := false
	err := fence(context.Background(), Identity{PID: 42, Start: "old"}, func(int) (string, bool, error) { return "new", false, nil }, func(int) error { signalled = true; return nil })
	require.ErrorIs(t, err, ErrMismatch)
	require.False(t, signalled)
}
func TestFenceReadOrSignalFailure(t *testing.T) {
	denied := errors.New("permission denied")
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
	cmd := exec.Command("sleep", "30")
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
	start, _, err := inspect(cmd.Process.Pid)
	require.NoError(t, err)
	require.NoError(t, cmd.Process.Signal(syscall.SIGSTOP))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, Fence(ctx, Identity{PID: cmd.Process.Pid, Start: start}))
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
	err := Fence(context.Background(), Identity{PID: cmd.Process.Pid, Start: "wrong"})
	require.ErrorIs(t, err, ErrMismatch)
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
		{name: "reused live PID", start: "new", want: ErrMismatch},
		{name: "reused zombie PID", start: "new", dead: true, want: ErrMismatch},
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
			require.ErrorIs(t, err, tc.want)
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
	require.NoError(t, Fence(ctx, Identity{PID: cmd.Process.Pid, Start: start}))
	current, dead, err := inspect(cmd.Process.Pid)
	require.NoError(t, err, "child must still exist without parent reaping")
	require.Equal(t, start, current)
	require.True(t, dead, "Fence must return for an unreaped zombie")
	udp, err = net.ListenPacket("udp4", addr.UDP)
	require.NoError(t, err, "zombie must release its UDP socket before reaping")
	defer func() { _ = udp.Close() }()
	tcp, err = net.Listen("tcp4", addr.TCP)
	require.NoError(t, err, "zombie must release its TCP socket before reaping")
	defer func() { _ = tcp.Close() }()
}
