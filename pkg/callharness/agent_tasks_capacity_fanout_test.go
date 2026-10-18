package callharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type capacityState struct {
	Audio int
	Fired bool
}
type capacityAgent struct {
	state        capacityState
	mode         string
	starts, wide *atomic.Int32
	resumed      *atomic.Bool
}

func (a *capacityAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	width := 0
	switch e.Kind {
	case agent.Resumed:
		if a.resumed != nil {
			a.resumed.Store(true)
		}
	case agent.Started:
		a.starts.Add(1)
		if a.mode == "duplicate" {
			width = 12
		}
		if a.mode == "started" {
			width = 20
		}
	case agent.AudioInput:
		a.state.Audio++
		if a.mode == "input" && !a.state.Fired {
			a.state.Fired = true
			a.wide.Add(1)
			width = 20
		}
		if a.mode == "receipts" {
			width = 1
			if !a.state.Fired {
				a.state.Fired = true
				width = 12
			}
		}
	case agent.TaskMessage:
		if a.mode == "receipts" && e.Result.Outcome == agent.Capacity {
			return agent.Effects{StartTasks: []agent.TaskSpec{{Step: strings.Repeat("x", 4096), Kind: "test/v1", Class: agent.ReadOnly}}}, nil
		}
	}
	var effects agent.Effects
	for i := range width {
		effects.StartTasks = append(effects.StartTasks, agent.TaskSpec{Step: fmt.Sprint(i), Kind: "test/v1", Class: agent.ReadOnly})
	}
	return effects, nil
}
func (a *capacityAgent) Save(context.Context) (agent.State, error) {
	b, err := json.Marshal(a.state)
	return agent.State{Version: 1, Bytes: b}, err
}
func (a *capacityAgent) Restore(_ context.Context, s agent.State) error {
	return json.Unmarshal(s.Bytes, &a.state)
}
func TestV2TaskCapacityFanOutAndReceipts(t *testing.T) {
	for _, mode := range []string{"started", "input", "receipts"} {
		t.Run(mode, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				starts, wide := &atomic.Int32{}, &atomic.Int32{}
				handler := func(ctx context.Context, _ *agent.TaskRun) error {
					if mode != "receipts" {
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}
				reg := agenthost.Registration{Name: "capacity/v1", Tasks: map[string]agent.TaskHandler{"test/v1": handler}, Factory: func(agent.FactoryArgs) agent.Agent {
					return &capacityAgent{mode: mode, starts: starts, wide: wide}
				}}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}})
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				duration := 700 * time.Millisecond
				minimum := 15
				if mode == "receipts" {
					duration = 3 * time.Second
					minimum = 20
				}
				require.NoError(t, call.SendMedia(ctx, duration))
				require.Equal(t, 1, sys.workers[0].SessionCount())
				wire, err := sys.workers[0].SnapshotSession(call.SessionID())
				require.NoError(t, err)
				var snap taskSnapshot
				require.NoError(t, json.Unmarshal(wire, &snap))
				var saved capacityState
				require.NoError(t, json.Unmarshal(snap.State.Agent.State.Bytes, &saved))
				require.GreaterOrEqual(t, saved.Audio, minimum, "accepted audio must advance despite metadata pressure")
				require.Equal(t, int32(1), starts.Load(), "Started must not spin")
				if mode == "input" {
					require.Equal(t, int32(1), wide.Load(), "held input must not spin")
				}
				_, err = call.Hangup(ctx)
				require.NoError(t, err)
			})
		})
	}
}

type duplicateCapacityStore struct {
	sessionstore.Store
	source   netip.AddrPort
	injected atomic.Bool
}

func (s *duplicateCapacityStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	if lease.Worker == s.source {
		var root, state, host, v2 map[string]json.RawMessage
		if json.Unmarshal(data, &root) == nil && json.Unmarshal(root["State"], &state) == nil && json.Unmarshal(state["Agent"], &host) == nil && json.Unmarshal(host["V2"], &v2) == nil {
			var pending []json.RawMessage
			if json.Unmarshal(v2["Capacity"], &pending) == nil && len(pending) > 0 {
				pending = append(pending, pending[0])
				v2["Capacity"], _ = json.Marshal(pending)
				host["V2"], _ = json.Marshal(v2)
				state["Agent"], _ = json.Marshal(host)
				root["State"], _ = json.Marshal(state)
				data, _ = json.Marshal(root)
				if err := s.Store.PutState(ctx, lease, data); err != nil {
					return err
				}
				s.injected.Store(true)
				return nil
			}
		}
	}
	return s.Store.PutState(ctx, lease, data)
}
func TestV2TaskCapacityDuplicateCheckpointTakeover(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		wrapped := &duplicateCapacityStore{Store: store}
		resumed := &atomic.Bool{}
		starts, wide := &atomic.Int32{}, &atomic.Int32{}
		reg := agenthost.Registration{Name: "capacity-repair/v1", Tasks: map[string]agent.TaskHandler{"test/v1": func(ctx context.Context, _ *agent.TaskRun) error { <-ctx.Done(); return ctx.Err() }}, Factory: func(agent.FactoryArgs) agent.Agent {
			return &capacityAgent{mode: "duplicate", starts: starts, wide: wide}
		}}
		target := reg
		target.Factory = func(agent.FactoryArgs) agent.Agent {
			return &capacityAgent{mode: "duplicate", starts: starts, wide: wide, resumed: resumed}
		}
		sys := startAgentHarness(t, wrapped, agentHarnessSettings{v2: []agenthost.Registration{reg, target}})
		wrapped.source = sys.workers[0].LocalAddr()
		require.NoError(t, workerprobe.SetHoldTaskResults(wrapped.source, true))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return wrapped.injected.Load() }, 2*time.Second, time.Millisecond)
		require.NoError(t, workerprobe.Kill(wrapped.source))
		require.Eventually(t, func() bool { return resumed.Load() && sys.workers[1].SessionCount() == 1 }, 3*time.Second, time.Millisecond)
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}
