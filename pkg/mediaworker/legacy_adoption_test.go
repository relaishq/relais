package mediaworker_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type legacyMovingAgent struct {
	state agent.State
	move  []byte
}

func (*legacyMovingAgent) Handle(context.Context, agent.Event) (agent.Effects, error) {
	return agent.Effects{}, nil
}
func (a *legacyMovingAgent) Save(context.Context) (agent.State, error)      { return a.state, nil }
func (a *legacyMovingAgent) Restore(_ context.Context, s agent.State) error { a.state = s; return nil }
func (a *legacyMovingAgent) SaveMoveOnly(context.Context) ([]byte, error)   { return a.move, nil }
func (a *legacyMovingAgent) RestoreMoveOnly(_ context.Context, p []byte) error {
	a.move = bytes.Clone(p)
	return nil
}

func TestLegacyMoveRefusalRollsBackToAdmittedSplitSource(t *testing.T) {
	store := sessionstore.NewMemory()
	router, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, router.Close()) })
	plane := controlplane.New(router, store)
	var targetCreated atomic.Int32
	factory := func(target bool) legacy.Factory {
		return agenthost.Factory(agenthost.Registration{Name: "legacy-move/v1", Bounds: agenthost.Bounds{MaxStateBytes: 4096}, Factory: func(agent.FactoryArgs) agent.Agent {
			if target {
				targetCreated.Add(1)
			}
			return &legacyMovingAgent{state: agent.State{Version: 1, Bytes: []byte("accepted"), HasMoveOnly: true, MoveOnlyLength: 128 << 10}, move: bytes.Repeat([]byte{'m'}, 128<<10)}
		}})
	}
	makeWorker := func(split bool, cap int, target bool) *mediaworker.Worker {
		w, e := mediaworker.New(mediaworker.Config{SplitCheckpoints: split, SnapshotLimits: mediaworker.SnapshotLimits{MaxBytes: cap, BytesPerSecond: 81920}, Agent: mediaworker.AgentConfig{Factory: factory(target), CallbackTimeout: time.Second}, DisableFrameCache: true, Relay: &mediaworker.RelayConfig{Owners: store, Addr: router.WorkerAddr(), PublicAddr: router.PublicAddr()}})
		require.NoError(t, e)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		router.AddWorker(w.LocalAddr())
		return w
	}
	origin := makeWorker(false, 7168, false)
	source := makeWorker(true, 7168, false)
	target := makeWorker(true, 3072, true)
	require.NoError(t, plane.Register("0", origin.LocalAddr(), origin))
	server := httptest.NewServer(plane.ProcessHandler())
	defer server.Close()
	h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: router.PublicAddr()}})
	require.NoError(t, err)
	defer func() { require.NoError(t, h.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, callharness.CallOptions{})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 80*time.Millisecond))
	require.NoError(t, plane.Register("1", source.LocalAddr(), source))
	require.NoError(t, plane.Register("2", target.LocalAddr(), target))
	// A split worker admits a legacy snapshot by actual bytes, despite the
	// 4096-byte declaration's 11028-byte estimate. Move-only bytes are separate.
	moved, err := plane.Move(ctx, call.SessionID(), "1")
	require.NoError(t, err)
	require.False(t, moved.Lost)
	require.Equal(t, 1, source.SessionCount())
	moved, err = plane.Move(ctx, call.SessionID(), "2")
	require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
	require.True(t, moved.Result.RolledBack, "move result: %+v; error: %v", moved, err)
	require.False(t, moved.Lost)
	require.Equal(t, 1, source.SessionCount())
	require.Zero(t, target.SessionCount())
	require.Zero(t, targetCreated.Load())
	owner, err := store.Get(ctx, call.SessionID())
	require.NoError(t, err)
	require.Equal(t, source.LocalAddr(), owner.Worker)
	require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}
