package clusterprocess

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStopReapedChildCannotSignalReusedGroup(t *testing.T) {
	dir := t.TempDir()
	exited, err := Start(dir, "exited", os.Environ(), "/usr/bin/true")
	require.NoError(t, err)
	<-exited.done
	victim, err := Start(dir, "owned-victim", os.Environ(), "/bin/sleep", "60")
	require.NoError(t, err)
	defer victim.Stop()
	// Simulate numeric PID reuse using another process group owned by this test.
	// No signal ever targets an unrelated process on the machine.
	exited.pid = victim.pid
	exited.Stop()
	select {
	case <-victim.done:
		t.Fatal("stop signalled a reused group after reaping its leader")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWaitLogReadinessBelongsToChild(t *testing.T) {
	manager := &Manager{}
	defer manager.Stop(false)
	child, err := manager.Start(t.TempDir(), "ready-log", os.Environ(), "/bin/sh", "-c", "echo 'Ready to accept connections'; exec sleep 60")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, child.WaitLog(ctx, "Ready to accept connections"))
	missing, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	require.ErrorIs(t, child.WaitLog(missing, "missing marker"), context.DeadlineExceeded)
	child.Stop()
	require.Error(t, child.WaitLog(ctx, "Ready to accept connections"), "exited child log must not prove live readiness")
}

func TestReadyIncludesFinalExitLog(t *testing.T) {
	for i := 0; i < 20; i++ {
		child, err := Start(t.TempDir(), "startup-failure", os.Environ(), "/bin/sh", "-c", "sleep 0.003; echo 'fatal startup: Redis authentication failed' >&2; exit 23")
		require.NoError(t, err)
		defer child.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = child.Ready(ctx)
		cancel()
		require.ErrorContains(t, err, "fatal startup: Redis authentication failed")
		require.ErrorContains(t, err, "status 23")
	}
}

func TestReapedChildSignalCannotTargetReusedPID(t *testing.T) {
	exited, err := Start(t.TempDir(), "exited", os.Environ(), "/usr/bin/true")
	require.NoError(t, err)
	<-exited.Done()
	defer exited.Stop()
	victim, err := Start(t.TempDir(), "victim", os.Environ(), "/bin/sleep", "60")
	require.NoError(t, err)
	defer victim.Stop()
	exited.pid = victim.pid
	require.ErrorIs(t, exited.Signal(syscall.SIGKILL), os.ErrProcessDone)
	select {
	case <-victim.Done():
		t.Fatal("reaped child's signal hit reused PID")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReadyAcceptsDialOutWithoutHTTPListener(t *testing.T) {
	child, err := Start(t.TempDir(), "dial-out-ready", os.Environ(), "/bin/sh", "-c", `printf '{"pid":%s,"connect":"dial-out","media":"127.0.0.1:17001"}\n' "$$"; exec sleep 60`)
	require.NoError(t, err)
	defer child.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ready, err := child.Ready(ctx)
	require.NoError(t, err)
	require.Empty(t, ready.HTTP)
	require.Equal(t, "127.0.0.1:17001", ready.Media)
}
