//go:build darwin || linux

package redistestcluster

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
	"github.com/stretchr/testify/require"
)

func TestClusterStaleRedisServers(t *testing.T) {
	if testing.Short() {
		t.Skip("owns test redis-server processes")
	}
	if os.Getenv("RELAIS_STALE_SERVER_HELPER") == "1" {
		dir := os.Getenv("RELAIS_STALE_SERVER_DIR")
		child, pidFile := startStandaloneServer(t, dir)
		path, err := recordServer(child, pidFile)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "record-path"), []byte(path), 0600))
		os.Exit(0) // Deliberately emulate a harness exit that omitted cleanup.
	}
	if _, err := exec.LookPath("redis-server"); err != nil {
		t.Skip("redis-server not installed")
	}
	dir := t.TempDir()
	t.Setenv("RELAIS_TEST_ARTIFACT_DIR", filepath.Join(dir, "artifacts"))
	orphanDir := filepath.Join(dir, "orphan")
	require.NoError(t, os.Mkdir(orphanDir, 0700))
	command := exec.Command(os.Args[0], "-test.run=^TestClusterStaleRedisServers$", "-test.count=1")
	command.Env = append(os.Environ(), "RELAIS_STALE_SERVER_HELPER=1", "RELAIS_STALE_SERVER_DIR="+orphanDir)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	recordPath, err := os.ReadFile(filepath.Join(orphanDir, "record-path"))
	require.NoError(t, err)
	data, err := os.ReadFile(string(recordPath))
	require.NoError(t, err)
	var orphan serverRecord
	require.NoError(t, json.Unmarshal(data, &orphan))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = processidentity.Fence(ctx, orphan.Server)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gone, err := processidentity.Gone(ctx, orphan.Server)
	require.NoError(t, err)
	require.False(t, gone)
	liveDir := filepath.Join(dir, "live")
	require.NoError(t, os.Mkdir(liveDir, 0700))
	live, pidFile := startStandaloneServer(t, liveDir)
	defer live.Stop()
	liveRecord, err := recordServer(live, pidFile)
	require.NoError(t, err)
	defer func() { _ = os.Remove(liveRecord) }()
	otherDir := filepath.Join(dir, "unregistered")
	require.NoError(t, os.Mkdir(otherDir, 0700))
	other, _ := startStandaloneServer(t, otherDir)
	defer other.Stop()
	require.NoError(t, cleanupStaleServers(ctx))
	gone, err = processidentity.Gone(ctx, orphan.Server)
	require.NoError(t, err)
	require.True(t, gone)
	for _, child := range []*clusterprocess.Child{live, other} {
		select {
		case <-child.Done():
			t.Fatal("cleanup stopped a live-parent or unregistered server")
		default:
		}
	}
	// A stale PID file cannot authorize killing the process that reused that PID.
	data, err = os.ReadFile(liveRecord)
	require.NoError(t, err)
	var reused serverRecord
	require.NoError(t, json.Unmarshal(data, &reused))
	reused.Parent.Start += "-previous"
	reused.Server.Start += "-previous"
	data, err = json.Marshal(reused)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(liveRecord, data, 0600))
	require.NoError(t, cleanupStaleServers(ctx))
	select {
	case <-live.Done():
		t.Fatal("cleanup signalled a reused PID")
	default:
	}
	identity, err := processidentity.Observe(live.PID())
	require.NoError(t, err)
	reused.Server = identity
	reused.Server.Executable += "-wrong"
	data, err = json.Marshal(reused)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(liveRecord, data, 0600))
	require.ErrorIs(t, cleanupStaleServers(ctx), processidentity.ErrExecutable)
	select {
	case <-live.Done():
		t.Fatal("cleanup signalled an unverified executable")
	default:
	}
}

func startStandaloneServer(t *testing.T, dir string) (*clusterprocess.Child, string) {
	t.Helper()
	addr, err := clusterprocess.FreeTCP()
	require.NoError(t, err)
	_, port, _ := strings.Cut(addr, ":")
	pidFile := filepath.Join(dir, "redis.pid")
	child, err := clusterprocess.Start(dir, "redis", os.Environ(), "redis-server", "--bind", "127.0.0.1", "--port", port, "--pidfile", pidFile, "--save", "", "--appendonly", "no", "--dir", dir)
	require.NoError(t, err)
	t.Cleanup(child.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, child.WaitLog(ctx, "Ready to accept connections"))
	return child, pidFile
}

func TestClusterServerRegistryPermissions(t *testing.T) {
	t.Setenv("RELAIS_TEST_ARTIFACT_DIR", t.TempDir())
	require.NoError(t, os.MkdirAll(registryRoot(), 0755))
	require.NoError(t, os.Chmod(registryRoot(), 0755))
	require.ErrorContains(t, cleanupStaleServers(context.Background()), "private directory")
}
