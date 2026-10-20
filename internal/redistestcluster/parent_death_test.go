//go:build !windows

package redistestcluster

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processidentity"
	"github.com/stretchr/testify/require"
)

func TestClusterServerParentDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("owns a subprocess")
	}
	if os.Getenv("RELAIS_PARENT_DEATH_HELPER") == "1" {
		dir := os.Getenv("RELAIS_PARENT_DEATH_DIR")
		child, err := clusterprocess.StartSupervised(dir, "server", os.Environ(), "sleep", "60")
		require.NoError(t, err)
		watchServer(t, child)
		id, err := processidentity.Observe(child.PID())
		require.NoError(t, err)
		data, err := json.Marshal(id)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "identity.json"), data, 0600))
		<-child.Done()
		os.Exit(1) // Healthy parent must not lose its child through thread retirement.
	}
	dir := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestClusterServerParentDeath$", "-test.count=1")
	command.Env = append(os.Environ(), "RELAIS_PARENT_DEATH_HELPER=1", "RELAIS_PARENT_DEATH_DIR="+dir)
	require.NoError(t, command.Start())
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	var id processidentity.Identity
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "identity.json"))
		return err == nil && json.Unmarshal(data, &id) == nil
	}, 5*time.Second, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gone, err := processidentity.Gone(ctx, id)
	require.NoError(t, err)
	require.False(t, gone)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = processidentity.Fence(ctx, id)
	})
	require.NoError(t, command.Process.Kill())
	_ = command.Wait()
	require.Eventually(t, func() bool { gone, err := processidentity.Gone(ctx, id); return err == nil && gone }, 3*time.Second, 20*time.Millisecond)
}
