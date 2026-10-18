package redistestcluster

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFailureArtifacts(t *testing.T) {
	if os.Getenv("RELAIS_ARTIFACT_FAILURE_HELPER") == "1" {
		dir := Directory(t, "trial")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "trial.log"), []byte("evidence"), 0600))
		t.Error("intentional fixture failure")
		return
	}
	root := t.TempDir()
	t.Setenv("RELAIS_TEST_ARTIFACT_DIR", root)
	command := exec.Command(os.Args[0], "-test.run=^TestFailureArtifacts$", "-test.count=1")
	command.Env = append(os.Environ(), "RELAIS_ARTIFACT_FAILURE_HELPER=1")
	require.Error(t, command.Run())
	matches, err := filepath.Glob(filepath.Join(root, "trial-*", "trial.log"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	data, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	require.Equal(t, "evidence", string(data))
	var successful string
	t.Run("successful", func(t *testing.T) { successful = Directory(t, "trial") })
	_, err = os.Stat(successful)
	require.True(t, os.IsNotExist(err))
}
