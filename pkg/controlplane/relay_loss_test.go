package controlplane

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayLossUnsupportedDeadlineWhileCutoffFails(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
		c.relayWake = make(chan struct{}, 1)
		c.stateMu.Unlock()
		var unaffected []*call
		for _, spec := range []struct {
			slot    string
			capable bool
		}{{"r1", true}, {"r2", false}} {
			otherID, _, lease, err := a.CreateSessionAtRelay(context.Background(), "offer", spec.slot)
			require.NoError(t, err)
			other := &call{id: otherID, lease: lease, relaySlot: spec.slot, relayCapable: spec.capable}
			p.calls[otherID] = other
			unaffected = append(unaffected, other)
		}
		p.mu.Lock()
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		expireObservedWorker(t, b) // Dead, but recovery has not fenced the worker.
		binding := p.relaySet["r1"]
		// Advance detection, not the watchdog clock, to keep the real deadline test short.
		binding.lostAt = time.Now().Add(-relayRecoveryLimit + 2*time.Second)
		p.relaySet["r1"] = binding
		p.mu.Unlock()
		ctx := context.Background()
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode, "failed cut-off must still install the unsupported call's watchdog")
		require.Equal(t, binding.lostAt.Add(relayRecoveryLimit), episode.deadline)
		require.Equal(t, "relay-recovery-unsupported", episode.proposal().Kind)
		require.True(t, a.runs(id), "failed cut-off must not end the call before its deadline")
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		c.relayEpisodeMu.Lock()
		again := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.Same(t, episode, again, "cut-off retries must retain the detection deadline")
		require.Eventually(t, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return c.terminal
		}, 4*time.Second, time.Millisecond)
		require.False(t, time.Now().Before(episode.deadline), "the watchdog must not end the call early")
		c.stateMu.Lock()
		reason := c.relayEndReason
		c.stateMu.Unlock()
		require.Equal(t, "relay_lost", reason)
		select {
		case <-c.relayWake:
		default:
			t.Fatal("deadline must wake the terminal event path")
		}
		require.Eventually(t, func() bool { return !a.runs(id) }, time.Second, time.Millisecond)
		require.Eventually(t, func() bool {
			c.relayEpisodeMu.Lock()
			defer c.relayEpisodeMu.Unlock()
			return c.relayEpisode == nil
		}, time.Second, time.Millisecond, "terminal episode must finish journal cleanup")
		// Further failed cut-offs must not start a second terminal episode.
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		p.mu.Lock()
		losses := p.lossesByCause["relay_lost"]
		replaceable := b.observation.canReplace(b.reserved)
		p.mu.Unlock()
		require.False(t, replaceable, "deadline must not depend on dead-worker recovery")
		require.EqualValues(t, 1, losses)
		for _, other := range unaffected {
			require.True(t, a.runs(other.id))
			other.relayEpisodeMu.Lock()
			otherEpisode := other.relayEpisode
			other.relayEpisodeMu.Unlock()
			require.Nil(t, otherEpisode, "capable calls and calls on other slots still wait for cut-off")
		}
		c.relayEpisodeMu.Lock()
		again = c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.Nil(t, again, "ended calls must not get another watchdog")
	})
}

func TestRelayLossUnsupportedEndsAfterCutoff(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "first-attempt"
		if retry {
			name = "after-failed-cutoff"
		}
		t.Run(name, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, a, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				c, err := p.lookup(id)
				require.NoError(t, err)
				c.stateMu.Lock()
				c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
				c.stateMu.Unlock()
				p.mu.Lock()
				binding := p.relaySet["r1"]
				binding.lostAt = time.Now().Add(-5 * time.Second)
				p.relaySet["r1"] = binding
				b := p.workers["b"]
				original := b.worker.(*relayProtocolWorker)
				p.mu.Unlock()
				var first *relayEpisode
				if retry {
					p.mu.Lock()
					b.worker = &unavailableRelayWorker{original}
					expireObservedWorker(t, b)
					p.mu.Unlock()
					require.ErrorIs(t, p.loseRelay(context.Background(), "r1"), mediaworker.ErrClosed)
					c.relayEpisodeMu.Lock()
					first = c.relayEpisode
					c.relayEpisodeMu.Unlock()
					require.NotNil(t, first)
					p.mu.Lock()
					b.worker = original
					p.mu.Unlock()
				}
				func() {
					c.mu.Lock() // Inspect initialization before the recovery driver can end the call.
					defer c.mu.Unlock()
					require.NoError(t, p.loseRelay(context.Background(), "r1"))
					c.relayEpisodeMu.Lock()
					episode := c.relayEpisode
					c.relayEpisodeMu.Unlock()
					require.NotNil(t, episode)
					require.Equal(t, binding.lostAt.Add(relayRecoveryLimit), episode.deadline)
					if retry {
						require.Same(t, first, episode, "successful cut-off must reuse the detection-based episode")
					}
					p.mu.Lock()
					cached := p.relaySet["r1"].lossUnaffected
					p.mu.Unlock()
					require.Nil(t, cached, "successful cut-off must release its retry cache")
				}()
				require.Eventually(t, func() bool {
					c.relayEpisodeMu.Lock()
					defer c.relayEpisodeMu.Unlock()
					return c.relayEpisode == nil
				}, time.Second, time.Millisecond)
				p.mu.Lock()
				terminal, lost, failed := c.terminal, p.lossesByCause["relay_lost"], p.lossesByCause["relay_change_failed"]
				p.mu.Unlock()
				require.True(t, terminal)
				require.EqualValues(t, 1, lost)
				require.Zero(t, failed)
				c.stateMu.Lock()
				reason := c.relayEndReason
				c.stateMu.Unlock()
				require.Equal(t, "relay_lost", reason, "successful cut-off must keep the existing immediate end path")
				require.False(t, a.runs(id))
			})
		})
	}
}

type relayLossCountingStore struct {
	sessionstore.Store
	sessionstore.RelayConfirmations
	sessionstore.ControlStore
	owners, lists, puts atomic.Int64
	missingID           string
	missingOwner        atomic.Bool
	failNextPut         atomic.Bool
}

func (s *relayLossCountingStore) RelayOwner(ctx context.Context, id string) (sessionstore.Lease, sessionstore.RelayAssignment, error) {
	s.owners.Add(1)
	if id == s.missingID && s.missingOwner.Load() {
		return sessionstore.Lease{}, sessionstore.RelayAssignment{}, sessionstore.ErrNotFound
	}
	return s.RelayConfirmations.RelayOwner(ctx, id)
}

func (s *relayLossCountingStore) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	s.lists.Add(1)
	return s.ControlStore.ControlIntents(ctx)
}

func (s *relayLossCountingStore) PutControlIntent(ctx context.Context, intent sessionstore.ControlIntent) error {
	s.puts.Add(1)
	if s.failNextPut.Swap(false) {
		return sessionstore.ErrTransient
	}
	return s.ControlStore.PutControlIntent(ctx, intent)
}

func TestRelayLossExpiredOwnerLeaseRetriesAfterTakeover(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		counted := &relayLossCountingStore{Store: store, RelayConfirmations: store.(sessionstore.RelayConfirmations), ControlStore: store.(sessionstore.ControlStore)}
		p, a, b, id := relayFailurePlane(t, counted)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
		ctx := context.Background()
		owned, err := store.Transfer(ctx, c.lease, b.addr, time.Minute)
		require.NoError(t, err)
		c.rememberLease(owned)
		a.mu.Lock()
		delete(a.running, id)
		a.mu.Unlock()
		b.mu.Lock()
		b.running[id] = true
		b.mu.Unlock()
		p.mu.Lock()
		source := p.workers["b"]
		source.worker = &unavailableRelayWorker{source.worker.(*relayProtocolWorker)}
		expireObservedWorker(t, source)
		p.mu.Unlock()
		// Expose the expired-lease lookup result while takeover is pending.
		counted.missingID = id
		counted.missingOwner.Store(true)
		counted.owners.Store(0)
		counted.lists.Store(0)
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		p.mu.Lock()
		detected := p.relaySet["r1"].lostAt
		p.mu.Unlock()
		// Takeover changes the owner, but preserves the call's relay assignment.
		resumed, err := store.Transfer(ctx, owned, a.addr, time.Minute)
		require.NoError(t, err)
		c.rememberLease(resumed)
		a.mu.Lock()
		a.running[id] = true
		a.mu.Unlock()
		b.mu.Lock()
		delete(b.running, id)
		b.mu.Unlock()
		counted.missingOwner.Store(false)
		_, assignment, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "r1", assignment.Slot)
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode, "temporary NotFound must not suppress the restored call's watchdog")
		require.Equal(t, detected.Add(relayRecoveryLimit), episode.deadline)
		require.Equal(t, a.addr, episode.proposal().Lease.Worker)
		require.EqualValues(t, 3, counted.owners.Load(), "retry missing ownership until takeover restores it")
		for range 3 {
			require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		}
		require.EqualValues(t, 3, counted.owners.Load(), "store work must stop once the restored call is armed")
		require.EqualValues(t, 3, counted.lists.Load())
		require.EqualValues(t, 1, counted.puts.Load())
	})
}

func TestRelayLossFailedIntentWriteRetriesOriginalDeadline(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		counted := &relayLossCountingStore{Store: store, RelayConfirmations: store.(sessionstore.RelayConfirmations), ControlStore: store.(sessionstore.ControlStore)}
		p, _, _, id := relayFailurePlane(t, counted)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
		p.mu.Lock()
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		expireObservedWorker(t, b)
		binding := p.relaySet["r1"]
		binding.lostAt = time.Now().Add(-5 * time.Second)
		p.relaySet["r1"] = binding
		p.mu.Unlock()
		ctx := context.Background()
		counted.owners.Store(0)
		counted.lists.Store(0)
		counted.puts.Store(0)
		counted.failNextPut.Store(true)
		err = p.loseRelay(ctx, "r1")
		require.ErrorIs(t, err, mediaworker.ErrClosed)
		require.ErrorIs(t, err, sessionstore.ErrTransient)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode, "a failed write must still leave a local watchdog")
		records, err := store.(sessionstore.ControlStore).ControlIntents(ctx)
		require.NoError(t, err)
		require.Empty(t, records)
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		records, err = store.(sessionstore.ControlStore).ControlIntents(ctx)
		require.NoError(t, err)
		require.Len(t, records, 1, "retry must persist the original episode before cut-off succeeds")
		require.Equal(t, episode.proposal().Token, records[0].Token)
		require.WithinDuration(t, binding.lostAt, records[0].Started, time.Millisecond)
		c.relayEpisodeMu.Lock()
		again := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.Same(t, episode, again)
		require.Equal(t, binding.lostAt.Add(relayRecoveryLimit), again.deadline)
		for range 3 {
			require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		}
		require.EqualValues(t, 2, counted.owners.Load())
		require.EqualValues(t, 2, counted.lists.Load())
		require.EqualValues(t, 2, counted.puts.Load(), "a successful intent write must stop further retry writes")
	})
}

func TestRelayLossFailedCutoffStopsStoreWorkAfterFirstPass(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		counted := &relayLossCountingStore{Store: store, RelayConfirmations: store.(sessionstore.RelayConfirmations), ControlStore: store.(sessionstore.ControlStore)}
		p, a, _, id := relayFailurePlane(t, counted)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
		for range 8 {
			for _, spec := range []struct {
				slot    string
				capable bool
			}{{"r1", false}, {"r2", false}, {"r1", true}} {
				otherID, _, lease, err := a.CreateSessionAtRelay(context.Background(), "offer", spec.slot)
				require.NoError(t, err)
				p.calls[otherID] = &call{id: otherID, lease: lease, relaySlot: spec.slot, relayCapable: spec.capable}
			}
		}
		p.mu.Lock()
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		expireObservedWorker(t, b)
		p.mu.Unlock()
		counted.owners.Store(0)
		counted.lists.Store(0)
		counted.puts.Store(0)
		require.ErrorIs(t, p.loseRelay(context.Background(), "r1"), mediaworker.ErrClosed)
		require.EqualValues(t, 17, counted.owners.Load(), "inspect only the 17 non-capable calls once")
		require.EqualValues(t, 1, counted.lists.Load(), "share the first journal snapshot")
		require.EqualValues(t, 9, counted.puts.Load(), "arm only the nine calls on the lost slot")
		for range 5 {
			require.ErrorIs(t, p.loseRelay(context.Background(), "r1"), mediaworker.ErrClosed)
		}
		require.EqualValues(t, 17, counted.owners.Load(), "armed and unaffected calls must not repeat assignment reads")
		require.EqualValues(t, 1, counted.lists.Load(), "steady-state retries must not list the journal")
		require.EqualValues(t, 9, counted.puts.Load(), "armed calls must not rewrite their intent")
	})
}

type deadRelayLossOwner struct{ *unavailableRelayWorker }

func (w *deadRelayLossOwner) EndSession(string) error { return mediaworker.ErrClosed }

func TestRelayLossUnsupportedDeadOwnerDeadline(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
		p.mu.Lock()
		owner := p.workers["a"]
		owner.worker = &deadRelayLossOwner{&unavailableRelayWorker{a}}
		expireObservedWorker(t, owner)
		binding := p.relaySet["r1"]
		binding.lostAt = time.Now().Add(-relayRecoveryLimit + 2*time.Second)
		p.relaySet["r1"] = binding
		p.mu.Unlock()
		require.ErrorIs(t, p.loseRelay(context.Background(), "r1"), mediaworker.ErrClosed)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode)
		require.Equal(t, binding.lostAt.Add(relayRecoveryLimit), episode.deadline)
		require.Eventually(t, func() bool {
			c.relayEpisodeMu.Lock()
			defer c.relayEpisodeMu.Unlock()
			return c.relayEpisode == nil
		}, 4*time.Second, time.Millisecond)
		p.mu.Lock()
		terminal, cleanup := c.terminal, c.cleanupPending
		losses, replaceable := p.lossesByCause["relay_lost"], owner.observation.canReplace(owner.reserved)
		p.mu.Unlock()
		require.True(t, terminal)
		require.True(t, cleanup, "unreachable owner must leave fenced cleanup pending")
		require.EqualValues(t, 1, losses)
		require.False(t, replaceable, "the owner's recovery must still be unfinished")
	})
}

func TestRelayLossUnsupportedDeadlineSurvivesSuccessorRebuild(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
		p.mu.Lock()
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		expireObservedWorker(t, b)
		binding := p.relaySet["r1"]
		binding.lostAt = time.Now().Add(-5 * time.Second)
		p.relaySet["r1"] = binding
		p.mu.Unlock()
		ctx := context.Background()
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode)
		// Stop the first process's watcher; the successor must load only the journal.
		episode.once.Do(func() { close(episode.done) })
		next := New(&fakeRelay{}, store)
		stopRelayRecoveries(t, next)
		authorizeTestControl(t, next)
		t.Cleanup(func() { require.NoError(t, next.ReleaseControl(ctx)) })
		next.SetRelayReady(true)
		require.NoError(t, next.rebuild(ctx, nil))
		restored, err := next.lookup(id)
		require.NoError(t, err)
		require.NotSame(t, c, restored)
		restored.relayEpisodeMu.Lock()
		saved := restored.relayEpisode
		restored.relayEpisodeMu.Unlock()
		require.NotNil(t, saved)
		require.False(t, restored.relayCapable)
		require.Equal(t, "relay-recovery-unsupported", saved.proposal().Kind)
		require.Equal(t, episode.proposal().Token, saved.proposal().Token)
		require.WithinDuration(t, binding.lostAt, saved.proposal().Started, time.Millisecond)
		require.WithinDuration(t, binding.lostAt.Add(relayRecoveryLimit), saved.deadline, time.Millisecond)
	})
}

func TestRelayLeaseLossStartsChange(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		// The standby mechanism owns each slot's lease. No renewal simulates both
		// processes lost; worker heartbeats continue independently.
		leases := store.(sessionstore.RelayLeases)
		identity := sessionstore.RelayProcess{Owner: "relay-test", PID: 42, Start: "start", Host: "host", Namespace: "namespace", Executable: "relay"}
		_, err := leases.ClaimRelay(context.Background(), "r1", identity, time.Minute)
		require.NoError(t, err)
		live, err := leases.ClaimRelay(context.Background(), "r2", identity, time.Minute)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- p.Run(ctx) }()
		defer func() { cancel(); require.NoError(t, <-done) }()
		go func() {
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					_ = p.Heartbeat(a.addr)
					live, _ = leases.RenewRelay(ctx, live, time.Minute)
				}
			}
		}()
		require.Eventually(t, func() bool {
			_, assignment, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
			return e == nil && assignment.Pending == "r2"
		}, 4*time.Second, 10*time.Millisecond, "reachable unchanged slot lease must trigger every affected call")
	})
}
