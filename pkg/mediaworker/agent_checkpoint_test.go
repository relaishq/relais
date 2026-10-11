package mediaworker

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/agent"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type agentCheckpointStore struct {
	sessionstore.Store
	mu          sync.Mutex
	beforePut   func(sessionstore.Lease)
	beforeClock func()
}

func (s *agentCheckpointStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	s.mu.Lock()
	hook := s.beforePut
	s.mu.Unlock()
	if hook != nil {
		hook(lease)
	}
	return s.Store.PutState(ctx, lease, data)
}
func (s *agentCheckpointStore) Clock(ctx context.Context, id string) (time.Time, error) {
	s.mu.Lock()
	hook := s.beforeClock
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.Store.Clock(ctx, id)
}

func checkpointAgentWorkers(t *testing.T, factory agent.Factory) (*agentCheckpointStore, *Worker, *Worker) {
	t.Helper()
	store := &agentCheckpointStore{Store: sessionstore.NewMemory()}
	r, err := relay.New(relay.Config{Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	create := func() *Worker {
		w, err := New(Config{Agent: AgentConfig{Factory: factory}, SnapshotInterval: time.Hour, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr()}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		r.AddWorker(w.LocalAddr())
		return w
	}
	return store, create(), create()
}

func TestAgentCancelledResumeCheckpointPreservesLease(t *testing.T) {
	for _, echo := range []bool{true, false} {
		name := "custom"
		if echo {
			name = "echo"
		}
		t.Run(name, func(t *testing.T) {
			factory := func(string) agent.Agent {
				if echo {
					return &agent.Echo{}
				}
				return &countingAgent{}
			}
			store, source, target := checkpointAgentWorkers(t, factory)
			call, _ := dialDTLSCaller(t, source)
			require.Eventually(t, func() bool { _, err := store.GetState(context.Background(), call.id); return err == nil }, time.Second, time.Millisecond)
			state, err := source.ExportSession(call.id)
			require.NoError(t, err)
			lease, err := store.Get(context.Background(), call.id)
			require.NoError(t, err)
			lease, err = store.Transfer(context.Background(), lease, target.LocalAddr(), time.Minute)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store.mu.Lock()
			store.beforePut = func(l sessionstore.Lease) {
				if l.Worker == target.LocalAddr() {
					cancel()
				}
			}
			store.mu.Unlock()
			_, err = target.ResumeSession(state, ResumeOptions{Lease: lease, Context: ctx})
			require.ErrorIs(t, err, context.Canceled)
			require.Zero(t, target.SessionCount())
			require.Zero(t, target.AgentStats().SaveFailures)
			retained, err := store.Get(context.Background(), call.id)
			require.NoError(t, err)
			require.Equal(t, lease.Epoch, retained.Epoch)
			_, err = store.GetState(context.Background(), call.id)
			require.NoError(t, err)
			store.mu.Lock()
			store.beforePut = nil
			store.mu.Unlock()
			lease, err = store.Transfer(context.Background(), retained, source.LocalAddr(), time.Minute)
			require.NoError(t, err)
			_, err = source.ResumeSession(state, ResumeOptions{Lease: lease})
			require.NoError(t, err)
		})
	}
}

func TestAgentCheckpointSkipAndRequiredWrite(t *testing.T) {
	store, source, _ := checkpointAgentWorkers(t, func(string) agent.Agent { return &countingAgent{} })
	call, _ := dialDTLSCaller(t, source)
	s := source.session(call.id)
	require.Eventually(t, func() bool { return s.snapshotStored.Load() }, time.Second, time.Millisecond)
	// Synchronize the initial write before changing the published state.
	s.snapshotMu.Lock()
	s.mu.Lock()
	s.worker.cfg.Agent.SaveInterval = time.Hour
	s.lastCheckpoint = time.Now()
	s.state.Agent.State.Bytes = make([]byte, 8)
	binary.BigEndian.PutUint64(s.state.Agent.State.Bytes, 2)
	s.state.Agent.Progress.Consumed = 2
	attempts := s.state.Checkpoint.Attempts
	s.mu.Unlock()
	s.snapshotMu.Unlock()
	require.ErrorIs(t, s.persistSnapshot(), errCheckpointSkipped)
	s.mu.Lock()
	require.Equal(t, attempts, s.state.Checkpoint.Attempts)
	s.mu.Unlock()
	store.mu.Lock()
	store.beforePut = func(sessionstore.Lease) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.state.Agent.State.Bytes = make([]byte, 8)
		binary.BigEndian.PutUint64(s.state.Agent.State.Bytes, 3)
		s.state.Agent.Progress.Consumed = 3
	}
	store.mu.Unlock()
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	s.mu.Lock()
	durable := cloneAgentState(s.durableAgent)
	s.mu.Unlock()
	require.EqualValues(t, 2, binary.BigEndian.Uint64(durable.State.Bytes))
	require.EqualValues(t, 2, durable.Progress.Consumed)
	data, err := store.GetState(context.Background(), call.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.Equal(t, durable, saved.State.Agent)
	store.mu.Lock()
	store.beforePut = nil
	store.mu.Unlock()
}

func TestAgentExportFlushBelowRelayHoldTimeout(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := &countingAgent{process: func(context.Context, agent.Input) error { close(entered); <-release; return nil }}
	w, s, caller := agentPacketSession(t, a, time.Second)
	defer close(release)
	// Even this small queue would wait four seconds without the hold cap.
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	<-entered
	start := time.Now()
	err := s.flushAgent()
	require.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
	require.Less(t, time.Since(start), relay.DefaultHoldTimeout)
	require.Zero(t, w.SessionCount())
}

func TestAgentCheckpointRateLimitUsesCapturedPair(t *testing.T) {
	store, source, _ := checkpointAgentWorkers(t, func(string) agent.Agent { return &countingAgent{} })
	call, _ := dialDTLSCaller(t, source)
	s := source.session(call.id)
	require.Eventually(t, func() bool { return s.snapshotStored.Load() }, time.Second, time.Millisecond)
	s.snapshotMu.Lock()
	s.mu.Lock()
	s.worker.cfg.Agent.SaveInterval = time.Hour
	s.lastCheckpoint = time.Now()
	before := s.state.Checkpoint
	s.mu.Unlock()
	s.snapshotMu.Unlock()
	store.mu.Lock()
	store.beforeClock = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.state.Agent.State.Bytes = make([]byte, 8)
		binary.BigEndian.PutUint64(s.state.Agent.State.Bytes, 2)
		s.state.Agent.Progress.Consumed = 2
	}
	store.mu.Unlock()
	require.ErrorIs(t, s.persistSnapshot(), errCheckpointSkipped, "a callback accepted during Clock must not bypass the write cap")
	store.mu.Lock()
	store.beforeClock = nil
	store.mu.Unlock()
	s.mu.Lock()
	after := s.state.Checkpoint
	s.mu.Unlock()
	require.Equal(t, before, after, "a skipped captured pair records no attempt, success, or failure")
	require.NoError(t, s.persistSnapshotMode(context.Background(), true))
	data, err := store.GetState(context.Background(), call.id)
	require.NoError(t, err)
	saved, err := decodeSnapshot(data)
	require.NoError(t, err)
	require.Equal(t, before.Attempts+1, saved.State.Checkpoint.Attempts)
	require.Equal(t, before.Successes+1, saved.State.Checkpoint.Successes)
	require.Equal(t, before.Failures, saved.State.Checkpoint.Failures)
}

func TestAgentFlushBudgetWaitsForTimelyInFlightPair(t *testing.T) {
	entered := make(chan struct{})
	a := &countingAgent{process: func(ctx context.Context, _ agent.Input) error {
		close(entered)
		select {
		case <-time.After(1600 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	w, s, caller := agentPacketSession(t, a, 2*time.Second)
	s.handleRTP(testEncrypt(t, caller, 456, 1))
	<-entered
	s.handleRTP(testEncrypt(t, caller, 456, 2))
	s.handleRTP(testEncrypt(t, caller, 456, 3))
	require.NoError(t, s.flushAgent(), "flush budget expiry cannot fail an in-flight callback that meets its deadline")
	state, progress, err := w.SessionAgent(s.id)
	require.NoError(t, err)
	require.EqualValues(t, 1, binary.BigEndian.Uint64(state.Bytes))
	require.EqualValues(t, 1, progress.Consumed)
	require.EqualValues(t, 1, progress.Index)
	require.EqualValues(t, 2, w.AgentStats().InputDrops)
	require.Zero(t, w.AgentStats().CallbackDeadlines)
	require.Equal(t, 1, w.SessionCount())
}
