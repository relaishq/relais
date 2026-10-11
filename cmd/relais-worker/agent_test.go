package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/redisendpoint"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/agent"
	agentdemo "github.com/relais/pkg/agent/demo"
	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type demoSystem struct {
	h       *callharness.Harness
	plane   *controlplane.Plane
	workers []*mediaworker.Worker
	servers []*httptest.Server
}

func demoStores(t *testing.T, scenario func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { scenario(t, sessionstore.NewMemory()) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("dedicated Redis required")
			}
			t.Skip("dedicated Redis not configured")
		}
		require.NoError(t, redisendpoint.Validate(addr))
		token := make([]byte, 16)
		_, err := rand.Read(token)
		require.NoError(t, err)
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "demo-agent-test:" + hex.EncodeToString(token) + ":"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		scenario(t, store)
	})
}
func startDemoSystem(t *testing.T, store sessionstore.Store) *demoSystem {
	t.Helper()
	disable := workerprobe.Enable()
	t.Cleanup(disable)
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	plane := controlplane.New(r, store)
	control := httptest.NewServer(plane.ProcessHandler())
	t.Cleanup(control.Close)
	sys := &demoSystem{plane: plane}
	cfg, err := selectAgent("demo")
	require.NoError(t, err)
	for i := range 2 {
		w, err := mediaworker.New(mediaworker.Config{Agent: cfg, DisableFrameCache: true, Relay: &mediaworker.RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, err)
		sys.workers = append(sys.workers, w)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		server := httptest.NewServer(agentHandler(w, "demo"))
		sys.servers = append(sys.servers, server)
		t.Cleanup(server.Close)
		r.AddWorker(w.LocalAddr())
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), &controlplane.RemoteWorker{URL: server.URL, Client: server.Client()}))
		w.StartHeartbeats(&controlplane.RemoteHeartbeats{URL: control.URL, Name: strconv.Itoa(i), Client: control.Client()})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = plane.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	sys.h, err = callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: control.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sys.h.Close()) })
	return sys
}
func (s *demoSystem) status(t *testing.T, ctx context.Context, worker int, id string) agentdemo.Status {
	t.Helper()
	var result agentdemo.Status
	server := s.servers[worker]
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodGet, "/demo/agents/"+id, nil, &result, nil))
	return result
}
func TestDemoAgentPlannedMoveMemoryAndRedis(t *testing.T) {
	demoStores(t, func(t *testing.T, store sessionstore.Store) {
		sys := startDemoSystem(t, store)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, callharness.CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 740*time.Millisecond))
		require.Eventually(t, func() bool {
			_, p, err := sys.workers[0].SessionAgent(call.SessionID())
			return err == nil && p.Consumed == 37
		}, time.Second, time.Millisecond)
		before := sys.status(t, ctx, 0, call.SessionID())
		require.Equal(t, agentdemo.Position{Count: 2, PhaseSamples: 12 * agentdemo.FrameSamples}, before.Position)
		move, err := sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		require.Empty(t, move.Error)
		exported := sys.status(t, ctx, 0, call.SessionID()).Exported
		after := sys.status(t, ctx, 1, call.SessionID())
		require.NotNil(t, exported)
		require.NotNil(t, after.Checkpoint)
		require.NotNil(t, after.LastResume)
		require.Equal(t, *exported, *after.Checkpoint)
		require.Equal(t, before.Position, after.Position)
		require.Equal(t, before.Progress, after.LastResume.Progress)
		require.Equal(t, agent.PlannedMove, after.LastResume.Kind)
		require.False(t, after.LastResume.InputMayBeDuplicated)
		require.NoError(t, call.SendMedia(ctx, 260*time.Millisecond))
		require.Eventually(t, func() bool {
			_, p, err := sys.workers[1].SessionAgent(call.SessionID())
			return err == nil && p.Consumed == 50
		}, time.Second, time.Millisecond)
		final := sys.status(t, ctx, 1, call.SessionID())
		require.Equal(t, agentdemo.Position{Count: 3}, final.Position)
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.True(t, report.ConnectedThroughout())
		require.Zero(t, report.DecryptionFailures.Total())
		require.Zero(t, report.ICERestarts)
		require.Zero(t, report.Renegotiations)
		audio := report.Track("audio")
		require.NotNil(t, audio)
		require.GreaterOrEqual(t, audio.Packets, 49)
		require.Equal(t, audio.Packets, audio.UnmatchedPayloads, "caller receives synthesized audio rather than its input")
		t.Logf("DEMO_MOVE source=%+v restored=%+v final=%+v packets=%d", before.Position, after.LastResume.Position, final.Position, audio.Packets)
	})
}
func TestDemoAgentTakeoverMemoryAndRedis(t *testing.T) {
	demoStores(t, func(t *testing.T, store sessionstore.Store) {
		sys := startDemoSystem(t, store)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, callharness.CallOptions{})
		require.NoError(t, err)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(ctx, 2200*time.Millisecond) }()
		require.Eventually(t, func() bool {
			_, p, err := sys.workers[0].SessionAgent(call.SessionID())
			return err == nil && p.Consumed >= 20
		}, time.Second, time.Millisecond)
		ready := make(chan struct{})
		var once sync.Once
		require.NoError(t, workerprobe.SetBeforeSnapshot(sys.workers[0].LocalAddr(), func(lifetime context.Context, id string) {
			if id == call.SessionID() {
				once.Do(func() { close(ready) })
				<-lifetime.Done()
			}
		}))
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		data, err := store.GetState(ctx, call.SessionID())
		require.NoError(t, err)
		checkpoint, err := agentdemo.FromSnapshot(data)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			_, p, err := sys.workers[0].SessionAgent(call.SessionID())
			return err == nil && p.Consumed >= checkpoint.Progress.Consumed+4
		}, time.Second, time.Millisecond)
		before := sys.status(t, ctx, 0, call.SessionID())
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		require.Eventually(t, func() bool {
			status, err := sys.plane.Status(ctx)
			return err == nil && len(status.Takeovers) == 1 && len(status.Calls) == 1 && status.Calls[0].Owner == "1"
		}, 3*time.Second, 5*time.Millisecond)
		after := sys.status(t, ctx, 1, call.SessionID())
		require.NotNil(t, after.LastResume)
		require.NotNil(t, after.Checkpoint)
		require.Equal(t, checkpoint, *after.Checkpoint)
		require.Equal(t, checkpoint.Position, after.LastResume.Position)
		require.Equal(t, checkpoint.Progress, after.LastResume.Progress)
		require.Equal(t, agent.Takeover, after.LastResume.Kind)
		status, err := sys.plane.Status(ctx)
		require.NoError(t, err)
		require.Equal(t, float64(status.Takeovers[0].CheckpointAge)/1e6, after.LastResume.CheckpointAgeMS)
		require.Equal(t, float64(status.Takeovers[0].SnapshotAge)/1e6, after.LastResume.SnapshotAgeMS)
		rollback := float64((before.Position.Count-checkpoint.Position.Count)*agentdemo.CountSamples) + float64(before.Position.PhaseSamples) - float64(checkpoint.Position.PhaseSamples)
		require.LessOrEqual(t, rollback/48, after.LastResume.SnapshotAgeMS+20)
		require.False(t, after.LastResume.InputMayBeDuplicated, "#27 replay is not installed on this base")
		require.NoError(t, <-sent)
		final := sys.status(t, ctx, 1, call.SessionID())
		require.Greater(t, final.Progress.Consumed, checkpoint.Progress.Consumed+20)
		require.EqualValues(t, final.Progress.Consumed*agentdemo.FrameSamples, (final.Position.Count-1)*agentdemo.CountSamples+uint64(final.Position.PhaseSamples))
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.True(t, report.ConnectedThroughout())
		require.Zero(t, report.DecryptionFailures.Total())
		require.Zero(t, report.ICERestarts)
		require.Zero(t, report.Renegotiations)
		audio := report.Track("audio")
		require.NotNil(t, audio)
		require.Greater(t, audio.Packets, 50)
		t.Logf("DEMO_TAKEOVER before=%+v checkpoint=%+v final=%+v checkpoint_age_ms=%.3f snapshot_age_ms=%.3f", before.Position, checkpoint.Position, final.Position, after.LastResume.CheckpointAgeMS, after.LastResume.SnapshotAgeMS)
	})
}
func TestDemoAgentSelectionAndBoundedHistory(t *testing.T) {
	_, err := selectAgent("invalid")
	require.Error(t, err)
	echo, err := selectAgent("echo")
	require.NoError(t, err)
	require.Nil(t, echo.Factory)
	history := &agentHistory{}
	for i := range 200 {
		history.put(strconv.Itoa(i), func(status *agentdemo.Status) {
			status.Exported = &agentdemo.Evidence{Position: agentdemo.Position{Count: 1}}
		})
	}
	require.Len(t, history.values, 128)
	require.Nil(t, history.get("0").Exported)
	require.NotNil(t, history.get("199").Exported)
}
