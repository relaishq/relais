//go:build !windows

package redistestcluster

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/processidentity"
	"github.com/stretchr/testify/require"
)

func TestClusterFixtureStopsNodesOnParentDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("kills a test process owning a six-node cluster")
	}
	if os.Getenv("RELAIS_TEST_REDIS_CLUSTER") != "1" {
		t.Skip("local cluster opt-in required")
	}
	path := os.Getenv("RELAIS_CLUSTER_DEATH_RESULT")
	if path != "" {
		addrs := Start(t)
		data, err := json.Marshal(addrs)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0600))
		select {}
	}
	dir := t.TempDir()
	path = filepath.Join(dir, "addresses.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestClusterFixtureStopsNodesOnParentDeath$", "-test.count=1")
	configureWatchdog(cmd)
	cmd.Env = append(os.Environ(), "RELAIS_CLUSTER_DEATH_RESULT="+path, "RELAIS_TEST_ARTIFACT_DIR="+dir)
	log, err := os.Create(filepath.Join(dir, "parent.log"))
	require.NoError(t, err)
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var addrs []string
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(data, &addrs) == nil
	}, 30*time.Second, 20*time.Millisecond)
	require.Len(t, addrs, 6)
	t.Logf("owned Redis ports: %v", addrs)
	// Keep a cleanup fallback using exact identities if a watchdog assertion fails.
	entries, err := filepath.Glob(filepath.Join(dir, "server-registry", "*.json"))
	require.NoError(t, err)
	for _, path := range entries {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var record serverRecord
		require.NoError(t, json.Unmarshal(data, &record))
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = processidentity.Fence(ctx, record.Server)
		})
	}
	require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
	require.Error(t, cmd.Wait())
	require.Eventually(t, func() bool {
		for _, addr := range addrs {
			c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err == nil {
				_ = c.Close()
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "owned nodes survived the test process")
}
