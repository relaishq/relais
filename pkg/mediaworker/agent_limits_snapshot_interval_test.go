package mediaworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

func TestEchoFastSnapshotInterval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config AgentConfig
	}{
		{"echo", AgentConfig{}},
		{"legacy", AgentConfig{Factory: func(string) legacy.Agent { return &legacy.Echo{} }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := New(Config{Agent: tc.config, SnapshotInterval: 50 * time.Millisecond})
			require.NoError(t, err)
			require.NoError(t, w.Close())
		})
	}
}
func TestV2FastSnapshotIntervalRefuses(t *testing.T) {
	factory := agenthost.Factory(agenthost.Registration{Name: "fast/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &snapshotBudgetAgent{} }})
	w, err := New(Config{Agent: AgentConfig{Factory: factory, Registrations: map[string]legacy.Factory{"fast/v1": factory}}, SnapshotInterval: 50 * time.Millisecond})
	if w != nil {
		require.NoError(t, w.Close())
	}
	require.ErrorIs(t, err, agent.ErrCapacity)
	require.ErrorContains(t, err, "agent-snapshot-bytes-per-second")
	require.ErrorContains(t, err, "150528")
}
func TestADRDemoBudget(t *testing.T) {
	text, err := os.ReadFile("../../docs/adr/0010-pluggable-agents.md")
	require.NoError(t, err)
	// Read the operator example so documentation drift fails the test.
	var snapshot, rate, burst int
	line := strings.Split(strings.Split(string(text), "Demo A uses an explicit larger **operator** budget: e.g. ")[1], " for an 8 KiB app.")[0]
	_, err = fmt.Sscanf(line, "%d KiB snapshots, %d KiB/s and\n%d KiB burst", &snapshot, &rate, &burst)
	require.NoError(t, err)
	l := SnapshotLimits{MaxBytes: snapshot << 10, BytesPerSecond: rate << 10, BurstBytes: burst << 10}.defaults()
	require.NoError(t, l.ValidateForInterval(100*time.Millisecond))
}

// Test both Clock and PutState: export must not inherit either store wait.
type blockedStore struct {
	sessionstore.Store
	block   atomic.Bool
	phase   string
	entered chan struct{}
	release chan struct{}
}

func (b *blockedStore) wait(ctx context.Context) error {
	if b.block.CompareAndSwap(true, false) {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (b *blockedStore) Clock(ctx context.Context, id string) (time.Time, error) {
	if b.phase == "clock" {
		if err := b.wait(ctx); err != nil {
			return time.Time{}, err
		}
	}
	return b.Store.Clock(ctx, id)
}
func (b *blockedStore) PutState(ctx context.Context, l sessionstore.Lease, data []byte) error {
	if b.phase == "put" && b.block.CompareAndSwap(true, false) {
		close(b.entered)
		<-b.release // Model a successful write whose acknowledgement arrives late.
		return b.Store.PutState(context.Background(), l, data)
	}
	return b.Store.PutState(ctx, l, data)
}

func TestExportDoesNotWaitForSnapshot(t *testing.T) {
	for _, phase := range []string{"clock", "put"} {
		t.Run(phase, func(t *testing.T) {
			forV2Stores(t, func(t *testing.T, base sessionstore.Store) {
				store := &blockedStore{Store: base, phase: phase, entered: make(chan struct{}), release: make(chan struct{})}
				r, err := relay.New(relay.Config{Owners: store})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, r.Close()) })
				reg := agenthost.Registration{Name: "blocked/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &snapshotBudgetAgent{} }}
				w, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}, SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, w.Close()) })
				r.AddWorker(w.LocalAddr())
				call, client := dialDTLSCaller(t, w)
				defer client.Close()
				s := w.session(call.id)
				require.Eventually(t, func() bool { return checkpointKnown(s) }, time.Second, time.Millisecond)
				// Drain and rebase at a fixed clock, removing refill timing from comparison.

				s.checkpoint.wait()
				now := time.Now()
				balance := float64(w.cfg.SnapshotLimits.BurstBytes)
				if phase == "clock" {
					balance = 17
				}
				restoreCheckpointQuota(t, s, balance, now)
				store.block.Store(true)
				debitBefore := s.worker.metrics.snapshotDebitBytes.Load()
				writeDone := make(chan error, 1)
				go func() { writeDone <- s.checkpoint.persist(context.Background(), checkpointRequired) }()
				select {
				case <-store.entered:
				case <-time.After(time.Second):
					t.Fatal("snapshot did not reach blocked store")
				}
				exportDone := make(chan struct{})
				var data []byte
				var exportErr error
				go func() { data, exportErr = w.ExportSession(call.id); close(exportDone) }()
				// Release before asserting, including on the old blocking implementation.
				early := false
				select {
				case <-exportDone:
					early = true
				case <-time.After(150 * time.Millisecond):
				}
				close(store.release)
				<-exportDone
				writeErr := <-writeDone
				require.True(t, early, "export waited behind in-flight "+phase)
				require.NoError(t, exportErr)
				// A fenced/closed source can cancel the store write. Conservative export
				// still reserves the possible successful write and must not grant credit.
				if phase == "put" {
					require.NoError(t, writeErr, "late store acknowledgement must debit the source")
				}
				var snap snapshot
				require.NoError(t, json.Unmarshal(data, &snap))
				require.NotNil(t, snap.State.Agent.V2.Quota)
				quota, err := snap.State.Agent.V2.Quota.balance()
				require.NoError(t, err)
				after := balance - float64(s.worker.metrics.snapshotDebitBytes.Load()-debitBefore)
				require.LessOrEqual(t, float64(quota), after)
				if phase == "clock" {
					require.Zero(t, quota, "conservative debit floors at zero")
				}
				require.LessOrEqual(t, quota, uint64(w.cfg.SnapshotLimits.BurstBytes-w.cfg.SnapshotLimits.MaxBytes), "in-flight export must conservatively reserve one maximum snapshot")
			})
		})
	}
}

func TestFactoryOnlyV2ValidatesWhenResolved(t *testing.T) {
	factory := agenthost.Factory(agenthost.Registration{Name: "fast/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &snapshotBudgetAgent{} }})
	w, err := New(Config{Agent: AgentConfig{Factory: factory}, SnapshotInterval: 50 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	s := sessionFromState(w, sessionState{Version: 7, ID: "factory-only"})
	defer s.close()
	err = s.initAgent(false, ResumeOptions{})
	require.ErrorIs(t, err, agent.ErrCapacity)
	require.ErrorContains(t, err, "150528")
}
func TestCrashDiscardedMetadata(t *testing.T) {
	for _, kind := range []string{"move-only", "audio", "active"} {
		t.Run(kind, func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1}}
			_, s := v2UnitSession(t, a)
			f := agenthost.Factory(agenthost.Registration{Name: "unit/v1", Factory: func(agent.FactoryArgs) agent.Agent { return a }, Tasks: map[string]agent.TaskHandler{"work/v1": func(context.Context, *agent.TaskRun) error { return nil }}})
			s.worker.cfg.Agent.Factory = f
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			saved := s.state
			saved.Agent = cloneAgentState(s.state.Agent)
			switch kind {
			case "move-only":
				saved.Agent.V2.HasMoveOnly = true
				saved.Agent.V2.MoveOnlyLength = 240000
			case "audio":
				saved.Agent.V2.Playback.Length = 240000
				saved.Agent.V2.Playback.Duration = time.Hour
				saved.Agent.V2.Playback.Pending = 1
			case "active":
				for i := 0; i < 5; i++ {
					spec := agent.TaskSpec{Step: fmt.Sprint(i), Kind: "work/v1", Class: agent.ReadOnly}
					saved.Agent.V2.Tasks = append(saved.Agent.V2.Tasks, taskIntent{ID: agent.TaskID(fmt.Sprint(i)), Step: spec.Step, Kind: spec.Kind, Class: spec.Class, Digest: taskDigest(spec), Phase: taskStarted})
				}
			}
			_, target := v2UnitSession(t, a)
			target.worker.cfg.Agent.Factory = f
			target.worker.cfg.SnapshotLimits.MoveBytesPerSession = 128 << 10
			target.state = saved
			a.callback = func(context.Context) { a.state.HasMoveOnly = false; a.state.MoveOnlyLength = 0 }
			require.NoError(t, target.initAgent(true, ResumeOptions{Kind: legacy.Takeover}))
			if kind != "active" {
				require.Equal(t, agent.OutcomeUnknown, a.lastEvent.Resume.MoveOnly)
				require.Zero(t, target.moveReserved)
			}
		})
	}
}
