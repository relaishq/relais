package processidentity

import (
	"context"
	"errors"
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
