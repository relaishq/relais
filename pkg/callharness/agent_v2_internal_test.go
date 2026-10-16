package callharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/workerprobe"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type v2Observation struct {
	event   agent.Event
	starts  int
	audio   uint64
	payload []byte
}
type v2HarnessStats struct {
	events    chan v2Observation
	saves     atomic.Int64
	exports   atomic.Int64
	imports   atomic.Int64
	callbacks atomic.Int64
	overlap   atomic.Bool
}
type v2HarnessAgent struct {
	stats *v2HarnessStats
	state struct {
		Starts int
		Audio  uint64
	}
	data   []byte
	has    bool
	length int
}

func (a *v2HarnessAgent) enter() func() {
	if a.stats.callbacks.Add(1) != 1 {
		a.stats.overlap.Store(true)
	}
	return func() { a.stats.callbacks.Add(-1) }
}
func (a *v2HarnessAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	defer a.enter()()
	if _, ok := ctx.Deadline(); !ok {
		return agent.Effects{}, errors.New("missing callback deadline")
	}
	switch e.Kind {
	case agent.Started:
		a.state.Starts++
	case agent.AudioInput:
		a.state.Audio++
		return agent.Effects{}, nil
	case agent.Resumed:
	default:
		return agent.Effects{}, nil
	}
	a.stats.events <- v2Observation{e, a.state.Starts, a.state.Audio, bytes.Clone(a.data)}
	if e.Resume != nil && e.Resume.MoveOnly == agent.OutcomeUnknown {
		a.data = nil
		a.has = false
		a.length = 0
	}
	return agent.Effects{}, nil
}
func (a *v2HarnessAgent) Save(context.Context) (agent.State, error) {
	defer a.enter()()
	a.stats.saves.Add(1)
	data, err := json.Marshal(a.state)
	return agent.State{Version: 17, Bytes: data, HasMoveOnly: a.has, MoveOnlyLength: a.length}, err
}
func (a *v2HarnessAgent) Restore(_ context.Context, state agent.State) error {
	defer a.enter()()
	if state.Version != 17 {
		return agent.ErrVersion
	}
	a.has, a.length = state.HasMoveOnly, state.MoveOnlyLength
	return json.Unmarshal(state.Bytes, &a.state)
}
func (a *v2HarnessAgent) SaveMoveOnly(context.Context) ([]byte, error) {
	defer a.enter()()
	a.stats.exports.Add(1)
	return a.data, nil
}
func (a *v2HarnessAgent) RestoreMoveOnly(_ context.Context, data []byte) error {
	defer a.enter()()
	a.stats.imports.Add(1)
	a.data = data
	return nil
}
func v2Registrations(payload []byte) ([]agenthost.Registration, []*v2HarnessStats) {
	regs := make([]agenthost.Registration, 2)
	stats := make([]*v2HarnessStats, 2)
	for i := range regs {
		stats[i] = &v2HarnessStats{events: make(chan v2Observation, 32)}
		regs[i] = agenthost.Registration{Name: "harness/v1", TaskKinds: []string{"stt/v1", "tts/v1"}, Factory: func(agent.FactoryArgs) agent.Agent {
			var data []byte
			if i == 0 {
				data = bytes.Clone(payload)
			}
			return &v2HarnessAgent{stats: stats[i], data: data, has: len(data) > 0, length: len(data)}
		}}
	}
	return regs, stats
}
func awaitV2(t *testing.T, stats *v2HarnessStats, kind agent.EventKind) v2Observation {
	t.Helper()
	select {
	case got := <-stats.events:
		require.Equal(t, kind, got.event.Kind)
		return got
	case <-time.After(4 * time.Second):
		t.Fatal("missing v2 event", kind)
		return v2Observation{}
	}
}

type v2HarnessSnapshot struct {
	Version       int
	AgentMoveOnly *struct{ Bytes []byte }
	State         struct {
		Agent struct {
			State    legacy.State
			Progress legacy.Progress
			V2       struct {
				Started        bool
				HasMoveOnly    bool
				MoveOnlyLength int
				Name           string
				Contract       string
				TaskKinds      []string
				Bounds         agenthost.Bounds
			}
		}
	}
}

func readV2Snapshot(t *testing.T, store sessionstore.Store, id string, started bool) v2HarnessSnapshot {
	t.Helper()
	var snap v2HarnessSnapshot
	require.Eventually(t, func() bool {
		data, err := store.GetState(context.Background(), id)
		return err == nil && json.Unmarshal(data, &snap) == nil && snap.State.Agent.V2.Started == started
	}, 2*time.Second, time.Millisecond)
	require.Equal(t, 8, snap.Version)
	require.Nil(t, snap.AgentMoveOnly, "checkpoints must never serialize move payloads")
	return snap
}

func TestV2AgentPlannedMoveExact(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		payload := bytes.Repeat([]byte("recording"), 1<<16) // 576 KiB, far above crash-state budget
		regs, stats := v2Registrations(payload)
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		first := awaitV2(t, stats[0], agent.Started)
		require.Equal(t, 1, first.starts)
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		snap := readV2Snapshot(t, store, call.SessionID(), true)
		require.Equal(t, len(payload), snap.State.Agent.V2.MoveOnlyLength)
		require.True(t, snap.State.Agent.V2.HasMoveOnly)
		require.Equal(t, "harness/v1", snap.State.Agent.V2.Name)
		require.Equal(t, "agent/v2", snap.State.Agent.V2.Contract)
		require.Equal(t, []string{"stt/v1", "tts/v1"}, snap.State.Agent.V2.TaskKinds)
		require.EqualValues(t, 1024, snap.State.Agent.V2.Bounds.MaxStateBytes)
		require.Zero(t, stats[0].exports.Load())
		_, err = sys.plane.Move(ctx, call.SessionID(), "1")
		require.NoError(t, err)
		resumed := awaitV2(t, stats[1], agent.Resumed)
		require.Equal(t, legacy.PlannedMove, resumed.event.Resume.Kind)
		require.Equal(t, agent.OK, resumed.event.Resume.MoveOnly)
		require.Equal(t, payload, resumed.payload)
		require.Positive(t, resumed.audio, "planned move must include consumed audio")
		require.EqualValues(t, resumed.audio, resumed.event.Resume.Progress.Consumed)
		require.Equal(t, 1, resumed.starts, "checkpointed Started must not redeliver")
		require.EqualValues(t, 1, stats[0].exports.Load(), "exactly one SaveMoveOnly per export")
		require.EqualValues(t, 1, stats[1].imports.Load())
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		select {
		case e := <-stats[1].events:
			t.Fatalf("unexpected event after checkpointed Started: %v", e.event.Kind)
		default:
		}
		_, err = sys.plane.Move(ctx, call.SessionID(), "0")
		require.NoError(t, err)
		returned := awaitV2(t, stats[0], agent.Resumed)
		require.Equal(t, payload, returned.payload)
		require.Equal(t, 1, returned.starts)
		require.EqualValues(t, 1, stats[0].exports.Load())
		require.EqualValues(t, 1, stats[1].exports.Load(), "second export calls SaveMoveOnly once on its own actor")
		require.EqualValues(t, 1, stats[0].imports.Load())
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
		for _, stat := range stats {
			require.False(t, stat.overlap.Load())
		}
	})
}

// freezeStartedStore acknowledges only the pre-Started handshake snapshot for
// one source. The real actor accepts Started, but a crash must redeliver it.
type freezeStartedStore struct {
	sessionstore.Store
	frozen atomic.Bool
	source netip.AddrPort
}

func (s *freezeStartedStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	if lease.Worker != s.source {
		return s.Store.PutState(ctx, lease, data)
	}
	var snap v2HarnessSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	if !snap.State.Agent.V2.Started {
		s.frozen.Store(true)
	}
	if snap.State.Agent.V2.Started && s.frozen.Load() {
		return sessionstore.ErrTransient
	}
	return s.Store.PutState(ctx, lease, data)
}

func TestV2AgentTakeoverMoveOnlyUnknown(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		regs, stats := v2Registrations([]byte("crash-loses-recording"))
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		awaitV2(t, stats[0], agent.Started)
		readV2Snapshot(t, store, call.SessionID(), true)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		resumed := awaitV2(t, stats[1], agent.Resumed)
		require.Equal(t, legacy.Takeover, resumed.event.Resume.Kind)
		require.Equal(t, agent.OutcomeUnknown, resumed.event.Resume.MoveOnly)
		require.Empty(t, resumed.payload)
		require.Equal(t, 1, resumed.starts)
		readV2Snapshot(t, store, call.SessionID(), true)
		require.Zero(t, stats[0].exports.Load())
		require.Zero(t, stats[1].imports.Load(), "crash never calls RestoreMoveOnly")
		require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
		select {
		case e := <-stats[1].events:
			t.Fatalf("unexpected Started redelivery: %v", e.event.Kind)
		default:
		}
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
		for _, stat := range stats {
			require.False(t, stat.overlap.Load())
		}
	})
}

func TestV2AgentStartedRedeliveryUntilCheckpoint(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		frozen := &freezeStartedStore{Store: store}
		regs, stats := v2Registrations(nil)
		sys := startAgentHarness(t, frozen, agentHarnessSettings{v2: regs})
		frozen.source = sys.workers[0].LocalAddr()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		original := awaitV2(t, stats[0], agent.Started)
		readV2Snapshot(t, store, call.SessionID(), false)
		// Permit the required adoption checkpoint on the target; the source's
		// pre-Started snapshot remains the only crash-recoverable pair.
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		resumed := awaitV2(t, stats[1], agent.Resumed)
		require.Equal(t, legacy.Takeover, resumed.event.Resume.Kind)
		require.Zero(t, resumed.starts)
		redelivered := awaitV2(t, stats[1], agent.Started)
		require.Equal(t, original.event.ID, redelivered.event.ID)
		require.Equal(t, 1, redelivered.starts)
		readV2Snapshot(t, store, call.SessionID(), true)
		select {
		case e := <-stats[1].events:
			t.Fatalf("extra event: %v", e.event.Kind)
		default:
		}
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func TestV2AgentIncompatibleTargetTypedLoss(t *testing.T) {
	for _, mismatch := range []string{"name", "kinds", "bounds", "legacy-echo", "remote-name"} {
		t.Run(mismatch, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				regs, stats := v2Registrations([]byte("private-recording"))
				switch mismatch {
				case "name", "remote-name":
					regs[1].Name = "different/v1"
				case "kinds":
					regs[1].TaskKinds = []string{"other/v1"}
				case "bounds":
					regs[1].Bounds.MaxStateBytes = 2048
				}
				sys := startAgentHarness(t, store, agentHarnessSettings{v2: regs, targetEcho: mismatch == "legacy-echo", remoteTarget: mismatch == "remote-name"})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				call, err := sys.h.Dial(ctx, CallOptions{})
				require.NoError(t, err)
				var output atomic.Bool
				call.pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
					output.Store(true)
					call.onTrack(track, receiver)
				})
				awaitV2(t, stats[0], agent.Started)
				require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
				readV2Snapshot(t, store, call.SessionID(), true)
				// The planned refusal rolls back to the exact source, retaining cause.
				result, err := sys.plane.Move(ctx, call.SessionID(), "1")
				require.ErrorIs(t, err, agent.ErrIncompatibleAgent)
				require.True(t, result.Result.RolledBack)
				require.Zero(t, sys.workers[1].SessionCount())
				awaitV2(t, stats[0], agent.Resumed)
				require.NoError(t, call.SendMedia(ctx, 100*time.Millisecond))
				if mismatch == "legacy-echo" {
					require.Zero(t, sys.workers[1].AgentStats().CallbackFailures)
					require.EqualValues(t, 1, sys.workers[1].AgentStats().RestoreFailures, "native Echo target must report identity refusal before adoption")
				} else {
					require.Zero(t, stats[1].saves.Load(), "refuse before target callbacks")
				}
				// With the compatible source gone, takeover must use typed loss cleanup.
				require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
				require.Eventually(t, func() bool {
					status, err := sys.plane.Status(ctx)
					return err == nil && len(status.Takeovers) > 0 && status.Takeovers[len(status.Takeovers)-1].Lost
				}, 4*time.Second, 5*time.Millisecond)
				status, err := sys.plane.Status(ctx)
				require.NoError(t, err)
				require.Contains(t, status.Takeovers[len(status.Takeovers)-1].Error, agent.ErrIncompatibleAgent.Error())
				_, err = store.Get(ctx, call.SessionID())
				require.Error(t, err)
				_, err = store.GetState(ctx, call.SessionID())
				require.Error(t, err)
				require.Zero(t, sys.workers[1].SessionCount())
				require.Zero(t, stats[1].exports.Load())
				require.Zero(t, stats[1].imports.Load())
				require.Empty(t, stats[1].events)
				require.False(t, output.Load(), "incompatible target must never substitute Echo output")
			})
		})
	}
}
