package mediaworker

import (
	"context"
	"encoding/json"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type rollbackClockStore struct {
	sessionstore.Store
	offset atomic.Int64
}

func (s *rollbackClockStore) Clock(ctx context.Context, id string) (time.Time, error) {
	now, err := s.Store.Clock(ctx, id)
	return now.Add(time.Duration(s.offset.Load())), err
}

func TestRound1SnapshotClockRollbackCallSurvives(t *testing.T) {
	forV2ReviewStores(t, func(t *testing.T, base sessionstore.Store) {
		store := &rollbackClockStore{Store: base}
		r, err := relay.New(relay.Config{Owners: store})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		reg := agenthost.Registration{Name: "clock/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &snapshotBudgetAgent{} }}
		w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		r.AddWorker(w.LocalAddr())
		call, client := dialDTLSCaller(t, w)
		defer client.Close()
		s := w.session(call.id)
		require.Eventually(t, s.snapshotStored.Load, time.Second, time.Millisecond)
		store.offset.Store(-int64(time.Second))
		time.Sleep(400 * time.Millisecond)
		require.NoError(t, s.ctx.Err(), "a wall-clock step must not debit tokens or end a call")
		s.mu.Lock()
		writes := s.state.Checkpoint.Successes
		s.mu.Unlock()
		require.Greater(t, writes, uint64(2), "periodic checkpoints must continue after rollback")
	})
}
func TestRound1SnapshotClockRollbackRefillResumes(t *testing.T) {
	l := SnapshotLimits{}.defaults()
	at := time.Now()
	b := snapshotBucket{tokens: 17, at: at}
	require.EqualValues(t, 17, b.available(l, at.Add(-time.Second)))
	require.EqualValues(t, 17+float64(l.BytesPerSecond)/10, b.available(l, at.Add(-900*time.Millisecond)))
}

func TestRound1SnapshotSettings(t *testing.T) {
	for _, tc := range []struct {
		name, flag string
		change     func(*SnapshotLimits)
		interval   time.Duration
		minimum    string
	}{
		{"periodic-rate", "agent-snapshot-bytes-per-second", func(l *SnapshotLimits) { l.BytesPerSecond = 30720 }, 100 * time.Millisecond, "78848"},
		{"forced-share", "agent-snapshot-bytes-per-second", func(l *SnapshotLimits) { l.BytesPerSecond = 71680 }, 100 * time.Millisecond, "78848"},
		{"custom-interval", "agent-snapshot-bytes-per-second", func(l *SnapshotLimits) { l.BytesPerSecond = 81920 }, 50 * time.Millisecond, "150528"},
		{"forced-burst", "agent-snapshot-burst-bytes", func(l *SnapshotLimits) { l.BurstBytes = l.MaxBytes }, 100 * time.Millisecond, "14336"},
		{"snapshot-overflow", "agent-max-snapshot-bytes", func(l *SnapshotLimits) { l.MaxBytes = math.MaxInt; l.BurstBytes = math.MaxInt }, 100 * time.Millisecond, ""},
		{"rate-overflow", "agent-snapshot-bytes-per-second", func(l *SnapshotLimits) { l.BytesPerSecond = math.MaxInt }, 100 * time.Millisecond, ""},
		{"burst-overflow", "agent-snapshot-burst-bytes", func(l *SnapshotLimits) { l.BurstBytes = math.MaxInt }, 100 * time.Millisecond, ""},
		{"move-overflow", "agent-move-bytes-per-session", func(l *SnapshotLimits) { l.MoveBytesPerSession = math.MaxInt }, 100 * time.Millisecond, ""},
		{"total-overflow", "agent-move-bytes-total", func(l *SnapshotLimits) { l.MoveBytesTotal = math.MaxInt64 }, 100 * time.Millisecond, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := SnapshotLimits{}.defaults()
			tc.change(&l)
			w, err := New(Config{SnapshotLimits: l, SnapshotInterval: tc.interval, Agent: AgentConfig{Registrations: map[string]legacy.Factory{"unit/v1": agenthost.Factory(agenthost.Registration{})}}})
			if w != nil {
				require.NoError(t, w.Close())
			}
			require.ErrorIs(t, err, agent.ErrCapacity)
			require.ErrorContains(t, err, tc.flag)
			if tc.minimum != "" {
				require.ErrorContains(t, err, tc.minimum)
			}
		})
	}
}

func TestRound1ExportCarriesCurrentQuota(t *testing.T) {
	reg := agenthost.Registration{Name: "quota/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &snapshotBudgetAgent{} }}
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	call, client := dialDTLSCaller(t, w)
	defer client.Close()
	s := w.session(call.id)
	now := s.taskNow()
	s.snapshotBudgetMu.Lock()
	s.snapshotBytesBudget = snapshotBucket{tokens: 17, at: now}
	s.snapshotBudgetMu.Unlock()
	data, err := w.ExportSession(call.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.NotNil(t, snap.State.Agent.V2.Quota, "real export must stamp the live quota")
	n, err := snap.State.Agent.V2.Quota.balance()
	require.NoError(t, err)
	require.Less(t, n, uint64(w.cfg.SnapshotLimits.BurstBytes/2))
	_, err = w.ResumeSession(data, ResumeOptions{})
	require.NoError(t, err)
	target := w.session(call.id)
	targetNow := target.taskNow()
	target.snapshotBudgetMu.Lock()
	available := target.snapshotBytesBudget.available(w.cfg.SnapshotLimits, targetNow)
	target.snapshotBudgetMu.Unlock()
	require.Less(t, available, float64(w.cfg.SnapshotLimits.BurstBytes/2), "planned move must not grant a fresh burst")
}

func TestRound1RestoreContentCeilings(t *testing.T) {
	for _, kind := range []string{"state", "metadata", "mailbox", "active", "audio"} {
		t.Run(kind, func(t *testing.T) {
			_, source := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
			source.worker.cfg.Agent.Factory = agenthost.Factory(agenthost.Registration{Name: "unit/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &v2UnitAgent{state: agent.State{Version: 1}} }, Tasks: map[string]agent.TaskHandler{"work/v1": func(context.Context, *agent.TaskRun) error { return nil }}})
			require.NoError(t, source.initAgent(false, ResumeOptions{}))
			_, target := v2UnitSession(t, &v2UnitAgent{state: agent.State{Version: 1}})
			target.worker.cfg.Agent.Factory = source.worker.cfg.Agent.Factory
			target.state = source.state
			target.state.Agent = cloneAgentState(source.state.Agent)
			// Keep declaration/identity unchanged. Only the target's local host ceiling differs.
			switch kind {
			case "state":
				target.state.Agent.State.Bytes = make([]byte, 1025)
			case "metadata":
				target.state.Agent.V2.Timers = []hostTimer{{ID: string(make([]byte, 2200)), Generation: 1, At: time.Now().Add(time.Second)}}
			case "mailbox":
				target.worker.cfg.SnapshotLimits.MoveBytesPerSession = 1
				target.state.Agent.V2.MoveOnlyLength = 2
				target.state.Agent.V2.HasMoveOnly = true
			case "active":
				for i := 0; i < 5; i++ {
					target.state.Agent.V2.Tasks = append(target.state.Agent.V2.Tasks, taskIntent{ID: agent.TaskID(string(rune('a' + i))), Kind: "work/v1", Step: "step", Digest: taskDigest(agent.TaskSpec{Kind: "work/v1", Step: "step", Class: agent.ReadOnly}), Phase: taskStarted, Class: agent.ReadOnly})
				}
			case "audio":
				target.state.Agent.V2.Playback.Length = 2 << 20
			}
			err := target.initAgent(true, ResumeOptions{}, &movePayload{})
			require.ErrorIs(t, err, agent.ErrCapacity, "content exceeding a local ceiling must refuse with typed capacity")
		})
	}
}

func TestRound1MainV8BoundsRemainReadable(t *testing.T) {
	_, source := v2UnitSession(t, &snapshotBudgetAgent{})
	require.NoError(t, source.initAgent(false, ResumeOptions{}))
	data, err := json.Marshal(source.state)
	require.NoError(t, err)
	// Main f6428f2 wrote Bounds and no limits identity. Use that envelope shape.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	var app map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["Agent"], &app))
	var env map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(app["V2"], &env))
	delete(env, "limits")
	delete(env, "sq")
	env["Bounds"], err = json.Marshal(source.worker.cfg.Agent.Factory("").(*agenthost.Adapter).Registration.Bounds)
	require.NoError(t, err)
	app["V2"], err = json.Marshal(env)
	require.NoError(t, err)
	raw["Agent"], err = json.Marshal(app)
	require.NoError(t, err)
	data, err = json.Marshal(raw)
	require.NoError(t, err)
	_, target := v2UnitSession(t, &snapshotBudgetAgent{})
	require.NoError(t, json.Unmarshal(data, &target.state))
	require.NoError(t, target.initAgent(true, ResumeOptions{}))
}

func TestRound1ExportQuotaOnStores(t *testing.T) {
	forV2ReviewStores(t, func(t *testing.T, store sessionstore.Store) {
		r, err := relay.New(relay.Config{Owners: store})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		reg := agenthost.Registration{Name: "quota-store/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &snapshotBudgetAgent{} }}
		cfg := Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}, SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}}
		source, err := New(cfg)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, source.Close()) })
		r.AddWorker(source.LocalAddr())
		target, err := New(cfg)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, target.Close()) })
		call, client := dialDTLSCaller(t, source)
		defer client.Close()
		s := source.session(call.id)
		require.Eventually(t, s.snapshotStored.Load, time.Second, time.Millisecond)
		now := s.taskNow()
		s.snapshotMu.Lock()
		s.snapshotBudgetMu.Lock()
		s.snapshotBytesBudget = snapshotBucket{tokens: float64(cfg.SnapshotLimits.defaults().MaxBytes + 17), at: now}
		s.snapshotBudgetMu.Unlock()
		s.snapshotMu.Unlock()
		data, err := source.ExportSession(call.id)
		require.NoError(t, err)
		snap, err := decodeSnapshot(data)
		require.NoError(t, err)
		require.NotNil(t, snap.State.Agent.V2.Quota)
		before, err := snap.State.Agent.V2.Quota.balance()
		require.NoError(t, err)
		require.Less(t, before, uint64(source.cfg.SnapshotLimits.BurstBytes*3/4))
		lease, err := store.Get(context.Background(), call.id)
		require.NoError(t, err)
		lease, err = store.Transfer(context.Background(), lease, target.LocalAddr(), time.Minute)
		require.NoError(t, err)
		_, err = target.ResumeSession(data, ResumeOptions{Lease: lease})
		require.NoError(t, err)
		resumed := target.session(call.id)
		now = resumed.taskNow()
		resumed.snapshotBudgetMu.Lock()
		remaining := resumed.snapshotBytesBudget.available(target.cfg.SnapshotLimits, now)
		resumed.snapshotBudgetMu.Unlock()
		require.Less(t, remaining, float64(target.cfg.SnapshotLimits.BurstBytes*3/4), "resume must pay from the transferred balance, not a fresh burst")
	})
}

type round1EmitAgent struct{}

func (*round1EmitAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.Started {
		return agent.Effects{StartTasks: []agent.TaskSpec{{Step: "stream", Kind: "stream/v1", Class: agent.ReadOnly, Resume: agent.Retry}}}, nil
	}
	return agent.Effects{}, nil
}
func (*round1EmitAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1}, nil
}
func (*round1EmitAgent) Restore(context.Context, agent.State) error { return nil }
func TestRound1ExportPreservesEmitSpacing(t *testing.T) {
	first, next := make(chan struct{}, 1), make(chan time.Time, 1)
	reg := agenthost.Registration{Name: "spacing/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &round1EmitAgent{} }, Tasks: map[string]agent.TaskHandler{
		"stream/v1": func(ctx context.Context, r *agent.TaskRun) error {
			if r.After() == 0 {
				if err := r.Emit(ctx, 1, agent.Chunk{Data: []byte("one")}); err != nil {
					return err
				}
				first <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}
			if err := r.Emit(ctx, r.After()+1, agent.Chunk{Data: []byte("two")}); err != nil {
				return err
			}
			next <- time.Now()
			return nil
		},
	}}
	w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	call, client := dialDTLSCaller(t, w)
	defer client.Close()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("missing first immediate Emit")
	}
	data, err := w.ExportSession(call.id)
	require.NoError(t, err)
	before := time.Now()
	_, err = w.ResumeSession(data, ResumeOptions{})
	require.NoError(t, err)
	select {
	case emitted := <-next:
		require.GreaterOrEqual(t, emitted.Sub(before), 100*time.Millisecond)
	case <-time.After(time.Second):
		t.Fatal("resumed Emit never made progress")
	}
}
