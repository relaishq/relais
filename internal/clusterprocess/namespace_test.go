package clusterprocess

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNamespaceCommand(t *testing.T) {
	command, err := NamespaceCommand("relais-net-0-123-aabbccddee-worker-0", "/bin/worker", "-http", "10.1.0.2:0")
	require.NoError(t, err)
	require.Equal(t, []string{"ip", "netns", "exec", "relais-net-0-123-aabbccddee-worker-0", "/bin/worker", "-http", "10.1.0.2:0"}, command)
	for _, name := range []string{"", "../namespace", "-bad", "a b", "a/b"} {
		_, err := NamespaceCommand(name, "/bin/worker")
		require.Error(t, err)
	}
	_, err = NamespaceCommand("safe")
	require.Error(t, err)
}
func TestManagerCleanupAfterChildren(t *testing.T) {
	manager := &Manager{}
	child, err := manager.Start(t.TempDir(), "owned", os.Environ(), "/bin/sleep", "60")
	require.NoError(t, err)
	var once sync.Once
	marker := filepath.Join(t.TempDir(), "cleaned")
	require.NoError(t, manager.AddCleanup(func() {
		select {
		case <-child.Done():
		default:
			t.Error("resource cleanup preceded child exit")
		}
		once.Do(func() { require.NoError(t, os.WriteFile(marker, []byte("done"), 0600)) })
	}))
	manager.Stop(true)
	manager.Stop(false)
	_, err = os.Stat(marker)
	require.NoError(t, err)
	require.Error(t, manager.AddCleanup(func() {}))
}
