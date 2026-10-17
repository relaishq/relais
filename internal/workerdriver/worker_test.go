package workerdriver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerSelection(t *testing.T) {
	args := []string{"-name", "one", "-http", "127.0.0.1:0"}
	require.Equal(t, append([]string{filepath.Join("bin", "relais-worker")}, args...), (Options{}).Command("bin", args...))
	path := filepath.Join(t.TempDir(), "custom")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0700))
	selected := Options{Binary: path, Agent: "delayed-loopback/v1"}
	require.NoError(t, selected.Check(t.TempDir()), "stock worker need not exist")
	require.Equal(t, append(append([]string{path}, args...), "-agent", "delayed-loopback/v1"), selected.Command("absent", args...))
	require.ErrorContains(t, (Options{Binary: filepath.Dir(path), Agent: selected.Agent}).Check("bin"), "missing executable worker")
	require.NoError(t, os.Chmod(path, 0600))
	require.Error(t, selected.Check("bin"))
}

func TestBinaryOverrideRequiresAgent(t *testing.T) {
	o := Options{Binary: "/bin/sh"}
	require.ErrorContains(t, o.Check(t.TempDir()), "-agent is required with -worker-bin")
}
