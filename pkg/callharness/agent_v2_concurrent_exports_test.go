package callharness

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/workerprobe"
	"github.com/relaishq/relais/pkg/mediaworker"

	"github.com/relaishq/relais/internal/agenthost"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Zero-payload export check, exercised through all sixteen
// control-plane drain slots instead of simulating counters in the worker.
type exportAgent struct {
	slowRestoreAgent
	entered chan struct{}
	release chan struct{}
	resumed *atomic.Int64
	unknown *atomic.Int64
}

func (a *exportAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: []byte("s"), HasMoveOnly: true}, nil
}
func (a *exportAgent) SaveMoveOnly(ctx context.Context) ([]byte, error) {
	a.entered <- struct{}{}
	select {
	case <-a.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (*exportAgent) RestoreMoveOnly(context.Context, []byte) error { return nil }
func (a *exportAgent) Handle(_ context.Context, e agent.Event) (agent.Effects, error) {
	if e.Resume != nil {
		a.resumed.Add(1)
		if e.Resume.Kind != legacy.PlannedMove || e.Resume.MoveOnly != agent.OK {
			a.unknown.Add(1)
		}
	}
	return agent.Effects{}, nil
}
func TestV2SixteenConcurrentExports(t *testing.T) {
	if testing.Short() {
		t.Skip("extended v2 matrix runs outside the PR short suite")
	}
	old := runtime.GOMAXPROCS(16)
	defer runtime.GOMAXPROCS(old)
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		entered, release := make(chan struct{}, 16), make(chan struct{})
		var resumed, unknown atomic.Int64
		reg := agenthost.Registration{Name: "exports/v1", Factory: func(agent.FactoryArgs) agent.Agent {
			return &exportAgent{entered: entered, release: release, resumed: &resumed, unknown: &unknown}
		}}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, callbackBudget: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		calls := make([]*Call, 0, 16)
		for range 16 {
			call, err := sys.h.Dial(ctx, CallOptions{})
			require.NoError(t, err)
			calls = append(calls, call)
		}
		require.Equal(t, 16, sys.workers[0].SessionCount())
		var beforeMax uint64
		for _, call := range calls {
			lease, err := store.Get(ctx, call.SessionID())
			require.NoError(t, err)
			beforeMax = max(beforeMax, lease.Epoch)
		}
		done := make(chan error, 1)
		go func() { _, err := sys.plane.Drain(ctx, "0"); done <- err }()
		// Stop collecting well before the 1 s callback budget: admitted exports
		// block until release, so a host that admits only seven stays at seven.
		// The count is the exact concurrency regression assertion.
		n := 0
		timer := time.NewTimer(60 * time.Millisecond)
	collect:
		for n < 16 {
			select {
			case <-entered:
				n++
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		close(release)
		err := <-done
		require.Equal(t, 16, n, "all sixteen zero-payload exports must enter SaveMoveOnly")
		require.NoError(t, err)
		require.EqualValues(t, 16, resumed.Load())
		require.Zero(t, unknown.Load(), "drain must not convert a live source into crash recovery")
		var afterMax uint64
		for _, call := range calls {
			lease, err := store.Get(ctx, call.SessionID())
			require.NoError(t, err)
			afterMax = max(afterMax, lease.Epoch)
			_, err = call.Hangup(ctx)
			require.NoError(t, err)
		}
		require.Equal(t, beforeMax+16, afterMax, "one transfer per call, without extra crash-recovery epochs")
	})
}

// One deadline miss must drop its input, while the same actor accepts later
// packets. The late mutation is rolled back through the real Restore callback.
type slowAudioAgent struct {
	seen  bool
	count byte
}

func (a *slowAudioAgent) Handle(ctx context.Context, e agent.Event) (agent.Effects, error) {
	if e.Kind == agent.AudioInput {
		a.count++
		if !a.seen {
			a.seen = true
			<-ctx.Done()
			time.Sleep(5 * time.Millisecond)
		}
	}
	return agent.Effects{}, nil
}
func (a *slowAudioAgent) Save(context.Context) (agent.State, error) {
	return agent.State{Version: 1, Bytes: []byte{a.count}}, nil
}
func (a *slowAudioAgent) Restore(_ context.Context, state agent.State) error {
	a.count = state.Bytes[0]
	return nil
}
func TestV2SlowAudioContinues(t *testing.T) {
	if testing.Short() {
		t.Skip("extended v2 matrix runs outside the PR short suite")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		reg := agenthost.Registration{Name: "slow-audio/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &slowAudioAgent{} }}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		require.NoError(t, call.SendMedia(ctx, 500*time.Millisecond))
		require.Equal(t, 1, sys.workers[0].SessionCount(), "one late AudioInput must not end the call")
		state, progress, err := sys.workers[0].SessionAgent(call.SessionID())
		require.NoError(t, err)
		require.Greater(t, progress.Consumed, uint64(5))
		require.EqualValues(t, progress.Consumed, state.Bytes[0], "late mutation never commits")
		stats := sys.workers[0].AgentStats()
		require.EqualValues(t, 1, stats.CallbackDeadlines)
		require.GreaterOrEqual(t, stats.InputDrops, uint64(1))
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func TestV2BudgetRetryThroughRemoteWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("extended v2 matrix runs outside the PR short suite")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		var attempts atomic.Int64
		reg := agenthost.Registration{Name: "budget-retry/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &slowRestoreAgent{} }}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, remoteTarget: true, targetHandler: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/resume" && attempts.Add(1) <= 2 {
					privateapi.Error(w, mediaworker.ErrMoveBudget, mediaworker.RemoteErrors)
					return
				}
				next.ServeHTTP(w, r)
			})
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		readV2Snapshot(t, store, call.SessionID(), true)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		require.Eventually(t, func() bool { status, err := sys.plane.Status(ctx); return err == nil && len(status.Takeovers) == 1 }, 4*time.Second, 10*time.Millisecond)
		status, err := sys.plane.Status(ctx)
		require.NoError(t, err)
		require.False(t, status.Takeovers[0].Lost, "temporary pressure must not become no-compatible-target loss")
		require.GreaterOrEqual(t, attempts.Load(), int64(3))
		require.Equal(t, 1, sys.workers[1].SessionCount())
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

func TestV2BudgetExhaustedThroughRemoteWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("extended v2 pressure recovery matrix")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		var attempts atomic.Int64
		reg := agenthost.Registration{Name: "budget-loss/v1", Factory: func(agent.FactoryArgs) agent.Agent { return &slowRestoreAgent{} }}
		sys := startAgentHarness(t, store, agentHarnessSettings{v2: []agenthost.Registration{reg, reg}, remoteTarget: true, targetHandler: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/resume" {
					attempts.Add(1)
					privateapi.Error(w, mediaworker.ErrMoveBudget, mediaworker.RemoteErrors)
					return
				}
				next.ServeHTTP(w, r)
			})
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{})
		require.NoError(t, err)
		readV2Snapshot(t, store, call.SessionID(), true)
		require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
		require.Eventually(t, func() bool { status, err := sys.plane.Status(ctx); return err == nil && len(status.Takeovers) == 1 }, 4*time.Second, 10*time.Millisecond)
		status, err := sys.plane.Status(ctx)
		require.NoError(t, err)
		require.True(t, status.Takeovers[0].Lost)
		require.Equal(t, "move_budget_exhausted", status.Takeovers[0].LossCause)
		require.LessOrEqual(t, attempts.Load(), int64(8))
		require.Greater(t, attempts.Load(), int64(1))
		require.Eventually(t, func() bool {
			_, err := store.Get(ctx, call.SessionID())
			return errors.Is(err, sessionstore.ErrNotFound) || errors.Is(err, sessionstore.ErrEnded)
		}, 2*time.Second, 5*time.Millisecond)
	})
}
