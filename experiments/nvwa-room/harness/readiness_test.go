package harness

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadinessReportsFirstExitedChildAndRedactsCredential(t *testing.T) {
	dir := t.TempDir()
	top := &Topology{Dir: dir, Root: dir, redisPassword: "credential-sentinel"}
	_, err := top.start("relay", "sh", []string{"-c", "printf 'AUTH credential-sentinel failure\\n'; exit 7"}, os.Environ())
	require.NoError(t, err)
	defer top.Close()
	start := time.Now()
	err = top.waitReady(context.Background(), "worker-1", "127.0.0.1:1", 30*time.Second)
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Contains(t, err.Error(), "startup relay")
	require.Contains(t, err.Error(), "exit status 7")
	require.Contains(t, err.Error(), "AUTH <REDACTED> failure")
	require.False(t, strings.Contains(err.Error(), top.redisPassword))
}
func TestReadinessHasItsOwnDeadline(t *testing.T) {
	top := &Topology{Dir: t.TempDir()}
	start := time.Now()
	err := top.waitReady(context.Background(), "relay", "127.0.0.1:1", 100*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
	require.Contains(t, err.Error(), "startup relay")
}
