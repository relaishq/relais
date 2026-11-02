package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayLossUnsupportedDeadlineWhileCutoffFails(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, a, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayCapable = false
		c.stateMu.Unlock()
		var unaffected []*call
		for _, spec := range []struct {
			slot    string
			capable bool
		}{{"r1", true}, {"r2", false}} {
			otherID, _, lease, err := a.CreateSessionAtRelay(context.Background(), "offer", spec.slot)
			require.NoError(t, err)
			other := &call{id: otherID, lease: lease, relaySlot: spec.slot, relayCapable: spec.capable}
			p.mu.Lock()
			p.calls[otherID] = other
			p.mu.Unlock()
			unaffected = append(unaffected, other)
		}
		p.mu.Lock()
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		expireObservedWorker(t, b) // death is detected, but recovery has not fenced it
		binding := p.relaySet["r1"]
		binding.lostAt = time.Now().Add(-relayRecoveryLimit + 2*time.Second)
		p.relaySet["r1"] = binding
		p.mu.Unlock()
		ctx := context.Background()
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		require.EqualValues(t, 1, journal.reads.Load(), "one shared journal snapshot per failed cut-off")
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode, "failed cut-off must not leave an unsupported call without a watchdog")
		require.Equal(t, binding.lostAt.Add(relayRecoveryLimit), episode.deadline)
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
		c.stateMu.Lock()
		reason := c.relayEndReason
		c.stateMu.Unlock()
		require.Equal(t, "relay_change_failed", reason)
		require.Eventually(t, func() bool { return !a.runs(id) }, time.Second, time.Millisecond)
		p.mu.Lock()
		losses := p.lossesByCause["relay_change_failed"]
		p.mu.Unlock()
		require.EqualValues(t, 1, losses)
		for _, other := range unaffected {
			require.True(t, a.runs(other.id))
			other.relayEpisodeMu.Lock()
			otherEpisode := other.relayEpisode
			other.relayEpisodeMu.Unlock()
			require.Nil(t, otherEpisode, "capable calls and other relay slots still wait for cut-off")
		}
	})
}

func TestRelayLossMixedCallsSharesJournal(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, a, _, _ := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		id, _, lease, err := a.CreateSessionAtRelay(context.Background(), "offer", "r1")
		require.NoError(t, err)
		p.mu.Lock()
		p.calls[id] = &call{id: id, lease: lease, relaySlot: "r1"}
		p.mu.Unlock()
		require.NoError(t, p.loseRelay(context.Background(), "r1"))
		require.EqualValues(t, 1, journal.reads.Load(), "successful cut-off must retain the single shared journal snapshot")
	})
}

func TestRelayLossFailedCutoffSkipsLoadedWork(t *testing.T) {
	for _, capable := range []bool{true, false} {
		name := "unsupported"
		if capable {
			name = "capable"
		}
		t.Run(name, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
				p, _, _, id := relayFailurePlane(t, journal)
				stopRelayRecoveries(t, p)
				authorizeTestControl(t, p)
				c, err := p.lookup(id)
				require.NoError(t, err)
				c.stateMu.Lock()
				c.relayCapable = capable
				c.stateMu.Unlock()
				p.mu.Lock()
				b := p.workers["b"]
				b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
				p.mu.Unlock()
				ctx := context.Background()
				require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
				reads, owners, puts := journal.reads.Load(), journal.owners.Load(), journal.puts.Load()
				if capable {
					require.Zero(t, reads)
					require.Zero(t, owners)
					require.Zero(t, puts)
				} else {
					require.EqualValues(t, 1, reads)
					require.EqualValues(t, 1, owners)
					require.EqualValues(t, 1, puts)
				}
				for range 3 {
					require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
				}
				require.Equal(t, reads, journal.reads.Load(), "loaded calls must not rescan the journal")
				require.Equal(t, owners, journal.owners.Load(), "loaded calls must not repeat assignment reads")
				require.Equal(t, puts, journal.puts.Load(), "loaded calls must not rewrite the intent")
				if !capable {
					guard := *p.restart.guard.Load()
					require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
					next, err := journal.ClaimControl(ctx, randomToken(), ControlLeaseTTL)
					require.NoError(t, err)
					p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(ControlLeaseTTL)})
					require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
					require.EqualValues(t, reads+1, journal.reads.Load(), "a new term must reload its journal intent")
				}
			})
		})
	}
}

func TestRelayLossFailedCutoffRetriesUnwrittenIntent(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, _, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayCapable = false
		c.stateMu.Unlock()
		p.mu.Lock()
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		p.mu.Unlock()
		ctx := context.Background()
		journal.putFail.Store(true)
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), sessionstore.ErrTransient)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode)
		journal.putFail.Store(false)
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		records, err := journal.ControlStore.ControlIntents(ctx)
		require.NoError(t, err)
		require.Len(t, records, 1, "a failed intent write must remain eligible for cut-off retries")
		require.Equal(t, episode.intent.Token, records[0].Token)
		require.True(t, episode.intent.Started.Equal(records[0].Started))
		puts := journal.puts.Load()
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		require.Equal(t, puts, journal.puts.Load(), "an acknowledged intent must not be rewritten")
	})
}

func TestRelayLossUnsupportedEpisodeSurvivesCutoffRetry(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayCapable = false
		c.stateMu.Unlock()
		p.mu.Lock()
		b := p.workers["b"]
		original := b.worker.(*relayProtocolWorker)
		b.worker = &unavailableRelayWorker{original}
		p.mu.Unlock()
		ctx := context.Background()
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode)
		func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			p.mu.Lock()
			b.worker = original
			p.mu.Unlock()
			require.NoError(t, p.loseRelay(ctx, "r1"))
			c.relayEpisodeMu.Lock()
			again := c.relayEpisode
			c.relayEpisodeMu.Unlock()
			require.Same(t, episode, again, "successful cut-off must reuse the pending episode and watchdog")
		}()
	})
}

type failedRelayOwner struct{ *unavailableRelayWorker }

func (w *failedRelayOwner) EndSession(string) error { return mediaworker.ErrClosed }

func TestRelayLossUnsupportedClearsPendingTakeover(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayCapable = false
		lease := c.lease
		c.stateMu.Unlock()
		p.mu.Lock()
		source := p.workers["a"]
		source.worker = &failedRelayOwner{&unavailableRelayWorker{a}}
		expireObservedWorker(t, source)
		p.recovery.pending[source] = map[string]*moveTransfer{id: {call: c, lease: lease, unrequested: true, deathCycle: source.observation.deathCycle}}
		binding := p.relaySet["r1"]
		binding.lostAt = time.Now().Add(-relayRecoveryLimit + 2*time.Second)
		p.relaySet["r1"] = binding
		p.mu.Unlock()
		ctx := context.Background()
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		require.Eventually(t, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return c.terminal
		}, 4*time.Second, time.Millisecond)
		require.False(t, p.recoverWorker(ctx, source, time.Now()), "an unreachable owner still needs fenced lease cleanup")
		p.mu.Lock()
		pending := p.recovery.hasWorkLocked(source, recoveryAll)
		p.mu.Unlock()
		require.False(t, pending, "terminal relay loss must clear pending takeover work")
		p.reconcileOwnership(ctx)
		require.True(t, p.recoverWorker(ctx, source, time.Now()), "fenced terminal cleanup must let the worker settle")
		p.mu.Lock()
		source.observation.recoveryFinished(true)
		p.mu.Unlock()
		require.NoError(t, p.loseRelay(ctx, "r1"), "the settled worker must no longer block cut-off")
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

func TestRelayLossFailedCutoffSkipsUnaffectedWork(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, a, _, _ := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		id, _, lease, err := a.CreateSessionAtRelay(context.Background(), "offer", "r2")
		require.NoError(t, err)
		p.mu.Lock()
		p.calls[id] = &call{id: id, lease: lease, relaySlot: "r2"}
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		p.mu.Unlock()
		ctx := context.Background()
		require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		require.EqualValues(t, 1, journal.owners.Load())
		for range 3 {
			require.ErrorIs(t, p.loseRelay(ctx, "r1"), mediaworker.ErrClosed)
		}
		require.EqualValues(t, 1, journal.owners.Load(), "unaffected calls must not repeat assignment reads")
		require.Zero(t, journal.reads.Load())
		require.Zero(t, journal.puts.Load())
	})
}
