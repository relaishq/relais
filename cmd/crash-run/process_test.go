package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStopReapedChildCannotSignalReusedGroup(t *testing.T) {
	root := filepath.Join("..", "..", "bin")
	require.NoError(t, os.MkdirAll(root, 0o755))
	dir, err := os.MkdirTemp(root, "process-stop-test-")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	exited, err := startChild(dir, "exited", os.Environ(), "/usr/bin/true")
	require.NoError(t, err)
	<-exited.done
	victim, err := startChild(dir, "owned-victim", os.Environ(), "/bin/sleep", "60")
	require.NoError(t, err)
	defer victim.stop()
	// Simulate numeric PID reuse using another process group owned by this test.
	// No signal ever targets an unrelated process on the machine.
	exited.pid = victim.pid
	exited.stop()
	select {
	case <-victim.done:
		t.Fatal("stop signalled a reused group after reaping its leader")
	case <-time.After(100 * time.Millisecond):
	}
}
