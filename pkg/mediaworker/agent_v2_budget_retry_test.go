package mediaworker

import (
	"context"
	"crypto/rand"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/agenthost"
	"github.com/relaishq/relais/internal/redistestenv"
	legacy "github.com/relaishq/relais/pkg/agent"
	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func forV2Stores(t *testing.T, run func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, sessionstore.NewMemory()) })
	t.Run("redis", func(t *testing.T) {
		addr := redistestenv.Address(t)
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("dedicated Redis required")
			}
			t.Skip("dedicated Redis not configured")
		}
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "v2-budget:" + rand.Text() + ":"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		run(t, store)
	})
}

// Unflushed refusal check: asserts
// queue preservation and subsequent consumption through the same actor seam.
func TestV2UnflushedRefusalKeepsQueuedInput(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}, payload: []byte("m")}
	w, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.worker.cfg.Agent.CallbackTimeout = time.Second
	s.agent.queue = make(chan agentInput, 8)
	for range 8 {
		s.agent.queue <- agentInput{}
	}
	pressure := int64(v2MoveWorkerBytes) - w.moveBytes.Load()
	require.True(t, w.reserveMoveBytes(pressure))
	release := sync.OnceFunc(func() { w.moveBytes.Add(-pressure) })
	defer release()
	request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(1500 * time.Millisecond), moveOnly: make(chan agentMoveExport, 1)}
	require.True(t, s.finishAgentFlush(request))
	require.ErrorIs(t, <-request.done, ErrExportNotFlushed)
	require.Len(t, s.agent.queue, 8)
	require.Zero(t, w.AgentStats().InputDrops)
	require.Zero(t, a.handles)
	require.False(t, s.agent.paused)
	release()
	request.deadline = time.Now().Add(10 * time.Second)
	require.True(t, s.finishAgentFlush(request))
	require.NoError(t, <-request.done)
	require.Empty(t, s.agent.queue)
	require.Equal(t, 8, a.handles)
	require.EqualValues(t, 8, s.agent.latest.Progress.Consumed)
	require.Zero(t, w.AgentStats().InputDrops)
	s.close()
	require.Zero(t, w.moveBytes.Load())
}

func TestV2AbandonedExportScratchReleased(t *testing.T) {
	for _, claim := range []bool{false, true} {
		t.Run(map[bool]string{false: "abandoned", true: "claimed"}[claim], func(t *testing.T) {
			a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}, payload: []byte("m")}
			w, s := v2UnitSession(t, a)
			require.NoError(t, s.initAgent(false, ResumeOptions{}))
			request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(time.Second), moveOnly: make(chan agentMoveExport, 1)}
			require.True(t, s.finishAgentFlush(request))
			require.NoError(t, <-request.done)
			require.EqualValues(t, 9, w.moveBytes.Load())
			var release func()
			if claim {
				var err error
				release, err = s.claimV2ExportScratch()
				require.NoError(t, err)
			}
			// No reader receives the successful buffered move reply in the abandoned
			// case: this is the flushAgent ctx.Done/actor-success race.
			s.close()
			if claim {
				require.EqualValues(t, 8, w.moveBytes.Load())
				release()
				release()
			}
			require.Zero(t, w.moveBytes.Load())
		})
	}
}

type renewCounter struct {
	sessionstore.Store
	renews atomic.Int64
}

func (s *renewCounter) Renew(ctx context.Context, lease sessionstore.Lease, ttl time.Duration) (sessionstore.Lease, error) {
	s.renews.Add(1)
	return s.Store.Renew(ctx, lease, ttl)
}

func TestV2BudgetRefusalBeforeLeaseRenew(t *testing.T) {
	for _, growing := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "resumed-save-growth"}[growing], func(t *testing.T) {
			forV2Stores(t, func(t *testing.T, store sessionstore.Store) {
				reg := agenthost.Registration{Name: "pre-renew/v1", Factory: func(agent.FactoryArgs) agent.Agent {
					return &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true, MoveOnlyLength: 1}, payload: []byte("m")}
				}}
				source, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}})
				require.NoError(t, err)
				defer source.Close()
				call, client := dialDTLSCaller(t, source)
				defer client.Close()
				data, err := source.ExportSession(call.id)
				require.NoError(t, err)
				if growing {
					reg.Factory = func(agent.FactoryArgs) agent.Agent {
						a := &v2UnitAgent{}
						a.callback = func(context.Context) { a.state.MoveOnlyLength = 2 }
						return a
					}
				}
				owners := &renewCounter{Store: store}
				target, err := New(Config{Agent: AgentConfig{Factory: agenthost.Factory(reg)}, Relay: &RelayConfig{Owners: owners, Addr: netip.MustParseAddrPort("127.0.0.1:12345"), PublicAddr: netip.MustParseAddrPort("127.0.0.1:12346")}})
				require.NoError(t, err)
				defer target.Close()
				lease, err := store.Claim(context.Background(), call.id, target.LocalAddr(), 200*time.Millisecond)
				require.NoError(t, err)
				pressure := int64(v2MoveWorkerBytes)
				if growing {
					pressure = v2MoveWorkerBytes - v2ExportScratchBytes - 2
				}
				require.True(t, target.reserveMoveBytes(pressure))
				defer target.moveBytes.Add(-pressure)
				for range 5 {
					_, err = target.ResumeSession(data, ResumeOptions{Kind: legacy.PlannedMove, Lease: lease})
					require.ErrorIs(t, err, ErrMoveBudget)
				}
				require.Zero(t, owners.renews.Load(), "refused exact restores must not extend ownership, including Resumed.Save growth")
				current, err := store.Get(context.Background(), call.id)
				require.NoError(t, err, "failed target retains the transferred lease for control-plane cleanup")
				require.True(t, lease.ExpiresAt.Equal(current.ExpiresAt), "refused restores leave the original expiry unchanged")
				require.Eventually(t, func() bool { _, err := store.Get(context.Background(), call.id); return err != nil }, time.Second, 10*time.Millisecond)
				require.EqualValues(t, pressure, target.moveBytes.Load(), "failed preflight cannot leak partial reservations")
			})
		})
	}
}

// Legacy keeps one callback reserve. Only a v2 exact export needs the second
// deadline for SaveMoveOnly; stock Echo bypasses flush entirely.
func TestV2LegacyFlushKeepsOneCallbackReserve(t *testing.T) {
	a := &countingAgent{}
	w, err := New(Config{Agent: AgentConfig{Factory: func(string) legacy.Agent { return a }}})
	require.NoError(t, err)
	defer w.Close()
	s := sessionFromState(w, sessionState{Version: 7, ID: "legacy-reserve"})
	defer s.close()
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	require.Nil(t, s.agent.v2)
	w.cfg.Agent.CallbackTimeout = time.Second
	s.agent.queue = make(chan agentInput, 1)
	s.agent.queue <- agentInput{}
	request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(1500 * time.Millisecond)}
	require.True(t, s.finishAgentFlush(request))
	require.NoError(t, <-request.done)
	require.EqualValues(t, 1, s.agent.latest.Progress.Consumed)
	// Echo has no actor queue and returns before pause or budget calculation.
	s.agent.instance = &legacy.Echo{}
	require.NoError(t, s.flushAgent())
	require.False(t, s.agent.paused)
}

func TestV2PostPauseErrorIsNotDefinitive(t *testing.T) {
	a := &v2UnitAgent{state: agent.State{Version: 1, HasMoveOnly: true}, moveCallback: func(context.Context) error { return ErrExportNotFlushed }}
	w, s := v2UnitSession(t, a)
	require.NoError(t, s.initAgent(false, ResumeOptions{}))
	s.worker.cfg.Agent.CallbackTimeout = time.Second
	s.agent.queue = make(chan agentInput, 8)
	for range 8 {
		s.agent.queue <- agentInput{}
	}
	request := agentFlush{done: make(chan error, 1), deadline: time.Now().Add(1500 * time.Millisecond), moveOnly: make(chan agentMoveExport, 1)}
	require.False(t, s.finishAgentFlush(request))
	err := <-request.done
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrExportNotFlushed)
	require.EqualValues(t, 8, w.AgentStats().InputDrops)
	require.True(t, s.agent.failed.Load())
	require.Zero(t, w.moveBytes.Load())
}
