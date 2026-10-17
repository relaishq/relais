package callharness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestDifferentOperatorLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("move/takeover between operator configurations; fast content refusal tests run in units")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		for _, mode := range []string{"move", "takeover"} {
			t.Run(mode, func(t *testing.T) {
				reg := agenthost.Registration{Name: "operator/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &limitHarnessAgent{} }}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, snapshotLimits: []mediaworker.SnapshotLimits{{MoveBytesPerSession: 1 << 20}, {MoveBytesPerSession: 128 << 10}}})
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				require.NoError(t, call.SendMedia(ctx, 120*time.Millisecond))
				persisted, err := store.GetState(ctx, call.SessionID())
				require.NoError(t, err)
				var snapshot struct {
					State struct {
						Agent struct {
							V2 struct {
								Limits *agent.Limits `json:"limits"`
								Bounds json.RawMessage
							}
						}
					}
				}
				require.NoError(t, json.Unmarshal(persisted, &snapshot))
				require.NotNil(t, snapshot.State.Agent.V2.Limits, "persist declared zero defaults, not resolved ceilings")
				require.Equal(t, agent.Limits{}, *snapshot.State.Agent.V2.Limits)
				require.Empty(t, snapshot.State.Agent.V2.Bounds)
				if mode == "move" {
					_, err = sys.plane.Move(ctx, call.SessionID(), "1")
					require.NoError(t, err)
				} else {
					readV2Snapshot(t, store, call.SessionID(), true)
					require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
					require.Eventually(t, func() bool { return sys.workers[1].SessionCount() == 1 }, 4*time.Second, 10*time.Millisecond)
				}
				require.NoError(t, call.SendMedia(ctx, 120*time.Millisecond))
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		}
	})
}

type moveBudgetAgent struct{ payload []byte }

func (*moveBudgetAgent) Handle(context.Context, agent.Event) (agent.Effects, error) {
	return agent.Effects{}, nil
}
func (a *moveBudgetAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: len(a.payload)}, nil
}
func (*moveBudgetAgent) Restore(context.Context, agent.State) error     { return nil }
func (a *moveBudgetAgent) SaveMoveOnly(context.Context) ([]byte, error) { return a.payload, nil }
func (a *moveBudgetAgent) RestoreMoveOnly(_ context.Context, p []byte) error {
	a.payload = append([]byte(nil), p...)
	return nil
}
func TestLocalMoveCeilingRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("real typed move refusal on both stores; unit content checks cover the fast path")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		reg := agenthost.Registration{Name: "local-ceiling/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &moveBudgetAgent{payload: make([]byte, 4096)} }}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, snapshotLimits: []mediaworker.SnapshotLimits{{MoveBytesPerSession: 1 << 20}, {MoveBytesPerSession: 2048}}})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 120*time.Millisecond))
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.ErrorIs(t, err, agent.ErrCapacity)
		require.NotErrorIs(t, err, agent.ErrIncompatibleAgent, "matching declarations with smaller local capacity are not identity mismatches")
		require.Equal(t, 1, sys.workers[0].SessionCount(), "definitive preflight refusal must roll back to the source")
		require.Zero(t, sys.workers[1].SessionCount())
		require.NoError(t, call.SendMedia(ctx, 120*time.Millisecond))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
