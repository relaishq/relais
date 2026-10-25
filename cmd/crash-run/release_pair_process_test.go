//go:build !windows

package main

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/stretchr/testify/require"
)

// A process that ends without being replaced is evidence, not noise.
func TestPairTopologyReportsUnexpectedExits(t *testing.T) {
	dir := t.TempDir()
	start := func(name string, args ...string) *pairProc {
		c, err := clusterprocess.Start(dir, name, nil, args...)
		require.NoError(t, err)
		t.Cleanup(c.Stop)
		return &pairProc{Name: name, Release: &pairRelease{Version: "0.3.0"}, Child: c}
	}
	live := func(name string) *pairProc { return start(name, "/bin/sleep", "30") }
	topology := &pairTopology{workers: [2]*pairProc{live("worker-0"), live("worker-1")}, relay: live("relay-1"), standby: live("relay-2"), control: live("control"), signal: start("signal", "/bin/sh", "-c", "exit 3")}
	store := live("redis").Child
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-topology.signal.Child.Done():
	case <-ctx.Done():
		t.Fatal("signal stand-in did not exit")
	}
	require.Equal(t, []string{"signal (0.3.0, process exited with status 3)"}, topology.unexpectedExits(store))
}
