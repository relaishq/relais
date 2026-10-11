package controlplane_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Hiding StartHeartbeats models a remote worker that stopped heartbeating.
type quietWorker struct{ controlplane.Worker }

func TestRelayRestartThenRejoinConnectsNewRealCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store := sessionstore.NewMemory()
	old, err := relay.New(relay.Config{Owners: store, Routes: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, old.Close()) }()
	public, leg := old.PublicAddr(), old.WorkerAddr()
	plane := controlplane.NewWithConfig(old, store, controlplane.Config{DeadAfter: 60 * time.Millisecond, CheckInterval: 10 * time.Millisecond})
	worker, err := mediaworker.New(mediaworker.Config{ListenAddr: "127.0.0.1:0", Relay: &mediaworker.RelayConfig{Addr: leg, PublicAddr: public, Owners: store}})
	require.NoError(t, err)
	defer func() { require.NoError(t, worker.Close()) }()
	old.AddWorker(worker.LocalAddr())
	require.NoError(t, plane.Register("0", worker.LocalAddr(), quietWorker{worker}))
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- plane.Run(runCtx) }()
	defer stop()
	require.Eventually(t, func() bool {
		status, err := plane.Status(ctx)
		return err == nil && len(status.Workers) == 1 && status.Workers[0].Dead && !status.Workers[0].Recovering
	}, time.Second, 10*time.Millisecond)
	stop()
	require.NoError(t, <-done)
	require.NoError(t, old.Close())
	replacement, err := relay.New(relay.Config{PublicAddr: public.String(), WorkerAddr: leg.String(), Owners: store, Routes: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, replacement.Close()) }()
	plane.SetRelay(replacement)
	server := httptest.NewServer(replacement.PrivateHandler())
	defer server.Close()
	remote := &controlplane.RemoteRelay{URL: server.URL, Client: server.Client()}
	var registry controlplane.RelayWorkerRegistry
	require.NoError(t, registry.Sync(ctx, plane, remote)) // Dead worker must be skipped.
	require.ErrorIs(t, plane.Heartbeat(worker.LocalAddr()), mediaworker.ErrRejoinRequired)
	require.NoError(t, plane.Heartbeat(worker.LocalAddr())) // Fence/drop ACK rejoins.
	require.NoError(t, registry.Sync(ctx, plane, remote))   // Same instance, now live.
	signal := httptest.NewServer(plane.Handler())
	defer signal.Close()
	harness, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: signal.URL + "/calls", RelayAddr: public}})
	require.NoError(t, err)
	defer func() { require.NoError(t, harness.Close()) }()
	call, err := harness.Dial(ctx, callharness.CallOptions{Worker: 0})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 500*time.Millisecond))
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Len(t, report.Tracks, 1)
	require.Positive(t, report.Tracks[0].Packets)
}
