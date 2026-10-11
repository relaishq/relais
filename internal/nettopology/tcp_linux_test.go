//go:build linux

package nettopology

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLinuxProfilePrivateRouteTuningAndCounters(t *testing.T) {
	requireLinuxNamespaces(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	topology, err := Open(ctx, t.Logf)
	require.NoError(t, err)
	defer topology.Close()
	rootBefore, err := runCommand(ctx, Command{"ip", "-j", "route", "show", "table", "all"})
	require.NoError(t, err)
	callerCommand := Command{"ip", "-n", topology.Plan.Roles["caller"].Namespace, "-j", "route", "show", "table", "all"}
	callerBefore, err := runCommand(ctx, callerCommand)
	require.NoError(t, err)
	settings, err := topology.TunePrivateRoutes(ctx)
	require.NoError(t, err)
	require.Len(t, settings, 5)
	snapshots := topology.SnapshotTCP(ctx)
	require.Len(t, snapshots, 8)
	for _, s := range snapshots {
		require.Empty(t, s.Error)
		require.Empty(t, s.Missing)
		require.Len(t, s.Counters, len(TCPCounterNames))
	}
	rootAfter, err := runCommand(ctx, Command{"ip", "-j", "route", "show", "table", "all"})
	require.NoError(t, err)
	callerAfter, err := runCommand(ctx, callerCommand)
	require.NoError(t, err)
	require.JSONEq(t, rootBefore, rootAfter)
	require.JSONEq(t, callerBefore, callerAfter)
	require.NoError(t, topology.Close())
}
