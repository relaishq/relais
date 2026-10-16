package controlplane

import (
	"context"
	"crypto/rand"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/redisendpoint"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type budgetRetryWorker struct {
	*fakeWorker
	attempts atomic.Int64
	refuse   atomic.Bool
}

func (w *budgetRetryWorker) ResumeSession(data []byte, opts mediaworker.ResumeOptions) (string, error) {
	w.attempts.Add(1)
	if w.refuse.Load() {
		return "", mediaworker.ErrMoveBudget
	}
	return w.fakeWorker.ResumeSession(data, opts)
}
func forBudgetRetryStores(t *testing.T, run func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, sessionstore.NewMemory()) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("dedicated Redis required")
			}
			t.Skip("dedicated Redis not configured")
		}
		require.NoError(t, redisendpoint.ValidateDev(addr))
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: "budget-retry:" + rand.Text() + ":"}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		run(t, store)
	})
}

// Based on the reviewer's retained planned-move probe. Budget refusals must
// not enter the unlimited, pinned confirmation path for uncertain adoption.
func TestMoveBudgetRetryBounded(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, b, _ := setup(t)
		p.store = store
		a.store = store
		b.store = store
		p.relay = &trackedHoldRelay{fakeRelay: &fakeRelay{}}
		ctx := context.Background()
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		bw := &budgetRetryWorker{fakeWorker: b}
		bw.refuse.Store(true)
		p.workers["b"].worker = bw
		p.store = &retainedMoveStore{Store: store}
		_, err = p.Move(ctx, id, "b")
		require.Error(t, err)
		source := p.workers["a"]
		start := time.Now()
		for time.Since(start) < 3*time.Second {
			p.mu.Lock()
			pending := source.pending[id]
			p.mu.Unlock()
			if pending == nil {
				break
			}
			p.takeover(ctx, source, pending.lease, time.Now())
			time.Sleep(10 * time.Millisecond)
		}
		require.Less(t, time.Since(start), 2500*time.Millisecond)
		require.Greater(t, bw.attempts.Load(), int64(1))
		require.LessOrEqual(t, bw.attempts.Load(), int64(8), "retries require backoff across detector ticks")
		status, err := p.Status(ctx)
		require.NoError(t, err)
		require.Len(t, status.Takeovers, 1)
		require.True(t, status.Takeovers[0].Lost)
		require.Equal(t, "move_budget_exhausted", status.Takeovers[0].LossCause)
		require.Contains(t, status.Takeovers[0].Error, ErrMoveBudgetExhausted.Error())
		_, err = store.Get(ctx, id)
		require.Error(t, err, "terminal pressure loss must release the transferred lease")
	})
}

func TestMoveBudgetRetryAlternateAndRollback(t *testing.T) {
	for _, retained := range []bool{true, false} {
		t.Run(map[bool]string{true: "retained", false: "rollback"}[retained], func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, a, b, _ := setup(t)
				p.store = store
				a.store = store
				b.store = store
				hold := &trackedHoldRelay{fakeRelay: &fakeRelay{}}
				p.relay = hold
				ctx := context.Background()
				id, _, err := p.Create(ctx, "offer", "a")
				require.NoError(t, err)
				bw := &budgetRetryWorker{fakeWorker: b}
				bw.refuse.Store(true)
				p.workers["b"].worker = bw
				if retained {
					p.store = &retainedMoveStore{Store: store}
				} else {
					aw := &budgetRetryWorker{fakeWorker: a}
					aw.refuse.Store(true)
					p.workers["a"].worker = aw
				}
				_, err = p.Move(ctx, id, "b")
				require.Error(t, err)
				source := p.workers["a"]
				if retained {
					p.takeover(ctx, source, source.pending[id].lease, time.Now())
					p.store = store
				}
				c := &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:3"), running: map[string]bool{}}
				require.NoError(t, p.Register("c", c.addr, c))
				require.Eventually(t, func() bool {
					p.mu.Lock()
					pending := source.pending[id]
					p.mu.Unlock()
					if pending == nil {
						return true
					}
					p.takeover(ctx, source, pending.lease, time.Now())
					return c.runs(id)
				}, time.Second, 10*time.Millisecond)
				require.True(t, c.runs(id), "a budget refusal must not pin the unavailable target")
				lease, err := store.Get(ctx, id)
				require.NoError(t, err)
				require.Equal(t, c.addr, lease.Worker)
				require.Contains(t, hold.releases, c.addr)
				status, err := p.Status(ctx)
				require.NoError(t, err)
				require.Len(t, status.Calls, 1)
				require.EqualValues(t, 1, status.Calls[0].MoveCount)
				require.NotNil(t, status.Calls[0].LastMove)
				require.Equal(t, "move", status.Calls[0].LastMoveKind)
				require.Contains(t, planeMetricsBody(p), "relais_control_moves_total 1\n")
				for _, result := range status.Takeovers {
					require.False(t, result.Lost)
				}
				require.NoError(t, p.End(ctx, id))
			})
		})
	}
}

func TestMoveBudgetRetryPreservesCommittedHangup(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, b, _ := setup(t)
		p.store = store
		a.store = store
		b.store = store
		p.relay = &trackedHoldRelay{fakeRelay: &fakeRelay{}}
		ctx := context.Background()
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		for name, w := range map[string]*fakeWorker{"a": a, "b": b} {
			limited := &budgetRetryWorker{fakeWorker: w}
			limited.refuse.Store(true)
			p.workers[name].worker = limited
		}
		_, err = p.Move(ctx, id, "b")
		require.Error(t, err)
		source := p.workers["a"]
		pending := source.pending[id]
		require.NotNil(t, pending)
		require.NoError(t, store.Release(ctx, pending.lease))
		pending.budgetStarted = time.Now().Add(-takeoverBudget - time.Millisecond)
		p.takeover(ctx, source, pending.lease, time.Now())
		status, err := p.Status(ctx)
		require.NoError(t, err)
		require.Zero(t, status.LostCount, "a committed hangup is not a capacity loss")
		require.Empty(t, source.pending)
	})
}

func TestMoveBudgetRetryRollbackReturnsToSource(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, b, _ := setup(t)
		p.store = store
		a.store = store
		b.store = store
		p.relay = &trackedHoldRelay{fakeRelay: &fakeRelay{}}
		ctx := context.Background()
		id, _, err := p.Create(ctx, "offer", "a")
		require.NoError(t, err)
		aw, bw := &budgetRetryWorker{fakeWorker: a}, &budgetRetryWorker{fakeWorker: b}
		aw.refuse.Store(true)
		bw.refuse.Store(true)
		p.workers["a"].worker = aw
		p.workers["b"].worker = bw
		_, err = p.Move(ctx, id, "b")
		require.ErrorIs(t, err, mediaworker.ErrMoveBudget)
		source := p.workers["a"]
		require.NotNil(t, source.pending[id])
		aw.refuse.Store(false)
		require.Eventually(t, func() bool {
			pending := source.pending[id]
			if pending == nil {
				return a.runs(id)
			}
			p.takeover(ctx, source, pending.lease, time.Now())
			return a.runs(id)
		}, time.Second, 10*time.Millisecond)
		require.Empty(t, source.pending)
		require.False(t, b.runs(id))
		status, err := p.Status(ctx)
		require.NoError(t, err)
		require.Zero(t, status.LostCount)
		require.Len(t, status.Calls, 1)
		require.Zero(t, status.Calls[0].MoveCount, "delayed rollback must preserve successful move counters")
		require.Nil(t, status.Calls[0].LastMove)
		require.Empty(t, status.Calls[0].LastMoveKind)
		require.Contains(t, planeMetricsBody(p), "relais_control_moves_total 0\n", "budget rollback is not a successful move")
		require.NoError(t, p.End(ctx, id))
	})
}
