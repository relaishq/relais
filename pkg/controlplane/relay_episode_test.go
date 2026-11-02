package controlplane

import (
	"bufio"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/relaishq/relais/pkg/agent/v2"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayEpisodeDeadlineSurvivesBusyRetriesAndReload(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		detected := time.Now()
		first, err := p.relayRecoveryEpisode(context.Background(), c, detected)
		require.NoError(t, err)
		defer first.once.Do(func() { close(first.done) })
		c.mu.Lock()
		again, err := p.relayRecoveryEpisode(context.Background(), c, detected.Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, first.deadline, again.deadline)
		c.mu.Unlock()
		// Reload from the same store into a fresh call object, as reconstruction does.
		restored := &call{id: id, lease: c.lease, relayCapable: true}
		saved, err := p.relayRecoveryEpisode(context.Background(), restored, detected.Add(time.Hour))
		require.NoError(t, err)
		defer saved.once.Do(func() { close(saved.done) })
		require.WithinDuration(t, detected.Add(15*time.Second), saved.deadline, time.Millisecond)
		require.Equal(t, first.intent.Token, saved.intent.Token)
	})
}

func TestRelayEpisodeExhaustionPublishesEndedWhileBusy(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		response, err := server.Client().Get(server.URL + "/calls/" + id + "/events")
		require.NoError(t, err)
		defer response.Body.Close()
		event := make(chan string, 1)
		go func() {
			scan := bufio.NewScanner(response.Body)
			ended := false
			for scan.Scan() {
				line := scan.Text()
				if line == "event: ended" {
					ended = true
				}
				if ended && strings.HasPrefix(line, "data: ") {
					event <- line
					return
				}
			}
			event <- ""
		}()
		func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			episode, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-14900*time.Millisecond))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				c.stateMu.Lock()
				defer c.stateMu.Unlock()
				return c.relayEndReason == "relay_change_failed"
			}, time.Second, time.Millisecond)
			require.Less(t, time.Until(episode.deadline), time.Duration(0))
			select {
			case data := <-event:
				require.Contains(t, data, "relay_change_failed")
			case <-time.After(time.Second):
				t.Fatal("busy operation prevented ended event")
			}
		}()
		require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.terminal }, time.Second, time.Millisecond)
	})
}

type episodeReadFault struct {
	*sessionstore.Memory
	fail atomic.Bool
}

func (s *episodeReadFault) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	if s.fail.Load() {
		return nil, errors.New("journal unavailable")
	}
	return s.Memory.ControlIntents(ctx)
}
func TestRelayEpisodeFailedJournalReadKeepsDetectionTime(t *testing.T) {
	store := &episodeReadFault{Memory: sessionstore.NewMemory()}
	p, _, _, id := relayFailurePlane(t, store)
	c, err := p.lookup(id)
	require.NoError(t, err)
	detected := time.Now()
	store.fail.Store(true)
	_, err = p.relayRecoveryEpisode(context.Background(), c, detected)
	require.Error(t, err)
	store.fail.Store(false)
	episode, err := p.relayRecoveryEpisode(context.Background(), c, detected.Add(time.Hour))
	require.NoError(t, err)
	defer episode.once.Do(func() { close(episode.done) })
	require.Equal(t, detected.Add(15*time.Second), episode.deadline)
}

func TestRelayEpisodeFromEarlierControlEpochPreservesRecoveredCall(t *testing.T) {
	for _, name := range []string{"recovered", "complete", "unfinished"} {
		recovered := name != "unfinished"
		t.Run(name, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, a, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				authorizeTestControl(t, p)
				guard := *p.restart.guard.Load()
				c, err := p.lookup(id)
				require.NoError(t, err)
				// Expire local authority before the episode deadline. The old watchdog
				// must wait while another control recovers this call.
				expired := guard
				expired.deadline = time.Now().Add(-time.Millisecond)
				p.restart.guard.Store(&expired)
				ctx := context.Background()
				episode, err := p.relayRecoveryEpisode(ctx, c, time.Now().Add(-relayRecoveryLimit))
				require.NoError(t, err)
				deadline, token := episode.deadline, episode.intent.Token
				journal := store.(sessionstore.ControlStore)
				require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
				successor, err := journal.ClaimControl(ctx, randomToken(), ControlLeaseTTL)
				require.NoError(t, err)
				if name == "complete" {
					saved := episode.intent
					saved.Kind = "relay-recovery-complete"
					require.NoError(t, journal.PutControlIntent(ctx, saved))
				} else if recovered {
					require.NoError(t, journal.DeleteControlIntent(ctx, episode.intent.ID, episode.intent.Token))
				}
				require.NoError(t, journal.ReleaseControl(ctx, successor))
				returned, err := journal.ClaimControl(ctx, guard.lease.Holder, ControlLeaseTTL)
				require.NoError(t, err)
				require.Greater(t, returned.Epoch, successor.Epoch)
				require.Same(t, c, p.rebuildCall(c.lease), "rebuild retains the old call object")
				if !recovered {
					// This call stands in for rebuild's journaled-intent path in restart.go.
					p.restart.guard.Store(&controlGuard{lease: returned, deadline: expired.deadline})
					restored, err := p.relayRecoveryEpisode(ctx, c, time.Now())
					require.NoError(t, err)
					require.Same(t, episode, restored, "an old recovery driver's completion must still stop the guarded episode")
					require.True(t, deadline.Equal(restored.deadline), "rebuild must retain the detection deadline")
					require.Equal(t, token, restored.intent.Token)
				}
				p.restart.guard.Store(&controlGuard{lease: returned, deadline: time.Now().Add(ControlLeaseTTL)})
				ended := func() bool {
					c.stateMu.Lock()
					defer c.stateMu.Unlock()
					return c.relayEndReason != ""
				}
				if recovered {
					require.Never(t, ended, 500*time.Millisecond, time.Millisecond, "an earlier control epoch must not end the recovered call")
					require.True(t, a.runs(id))
					_, err = store.Get(ctx, id)
					require.NoError(t, err)
					records, err := journal.ControlIntents(ctx)
					require.NoError(t, err)
					if name == "complete" {
						require.Len(t, records, 1)
					} else {
						require.Empty(t, records)
					}
					detected := time.Now()
					fresh, err := p.relayRecoveryEpisode(ctx, c, detected)
					require.NoError(t, err)
					require.NotEqual(t, token, fresh.intent.Token, "the next relay event must start a fresh episode")
					require.True(t, episode.completed.Load(), "the old episode must be retired")
					require.WithinDuration(t, detected.Add(relayRecoveryLimit), fresh.deadline, time.Millisecond)
					require.Never(t, ended, 250*time.Millisecond, time.Millisecond)
				} else {
					require.Eventually(t, ended, time.Second, time.Millisecond, "the new control epoch must still enforce the unfinished deadline")
				}
			})
		})
	}
}

type relayJournalStore struct {
	sessionstore.Store
	sessionstore.ControlStore
	sessionstore.RelayAssignments
	fail    atomic.Bool
	putFail atomic.Bool
	reads   atomic.Int64
	owners  atomic.Int64
	puts    atomic.Int64
}

func (s *relayJournalStore) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	s.reads.Add(1)
	if s.fail.Load() {
		return nil, errors.New("journal unavailable")
	}
	return s.ControlStore.ControlIntents(ctx)
}

func (s *relayJournalStore) RelayOwner(ctx context.Context, id string) (sessionstore.Lease, sessionstore.RelayAssignment, error) {
	s.owners.Add(1)
	return s.RelayAssignments.RelayOwner(ctx, id)
}

func (s *relayJournalStore) PutControlIntent(ctx context.Context, intent sessionstore.ControlIntent) error {
	s.puts.Add(1)
	if s.putFail.Load() {
		return sessionstore.ErrTransient
	}
	return s.ControlStore.PutControlIntent(ctx, intent)
}

func TestRelayEpisodeEpochChangeWaitsForJournal(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, a, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		guard := *p.restart.guard.Load()
		c, err := p.lookup(id)
		require.NoError(t, err)
		expired := guard
		expired.deadline = time.Now().Add(-time.Millisecond)
		p.restart.guard.Store(&expired)
		ctx := context.Background()
		old, err := p.relayRecoveryEpisode(ctx, c, time.Now().Add(-relayRecoveryLimit))
		require.NoError(t, err)
		require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
		next, err := journal.ClaimControl(ctx, randomToken(), ControlLeaseTTL)
		require.NoError(t, err)
		require.NoError(t, journal.DeleteControlIntent(ctx, old.intent.ID, old.intent.Token))
		p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(ControlLeaseTTL)})
		journal.fail.Store(true)
		_, err = p.relayRecoveryEpisode(ctx, c, time.Now())
		require.EqualError(t, err, "journal unavailable")
		require.Never(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason != ""
		}, 250*time.Millisecond, time.Millisecond, "a failed read must not re-arm an unconfirmed old intent")
		journal.fail.Store(false)
		detected := time.Now()
		fresh, err := p.relayRecoveryEpisode(ctx, c, detected)
		require.NoError(t, err)
		require.NotEqual(t, old.intent.Token, fresh.intent.Token)
		require.WithinDuration(t, detected.Add(relayRecoveryLimit), fresh.deadline, time.Millisecond)
		require.True(t, a.runs(id))
	})
}

func TestRelayEpisodeEpochChangeWhileWaitingForCallLock(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		guard := *p.restart.guard.Load()
		lease, err := store.(sessionstore.ControlStore).RenewControl(context.Background(), guard.lease, 5*time.Second)
		require.NoError(t, err)
		guard.lease, guard.deadline = lease, time.Now().Add(5*time.Second)
		p.restart.guard.Store(&guard)
		c, err := p.lookup(id)
		require.NoError(t, err)
		var cancelled atomic.Bool
		c.controlMu.Lock()
		c.relayCancel = func() { cancelled.Store(true) }
		c.controlCancel = func() { cancelled.Store(true) }
		c.controlMu.Unlock()
		c.mu.Lock()
		locked := true
		defer func() {
			if locked {
				c.mu.Unlock()
			}
		}()
		ctx := context.Background()
		_, err = p.relayRecoveryEpisode(ctx, c, time.Now().Add(-relayRecoveryLimit))
		require.NoError(t, err)
		poisoned := func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason != ""
		}
		require.Eventually(t, poisoned, time.Second, time.Millisecond, "managed expiry must publish while the holder unwinds")
		require.True(t, cancelled.Load())
		journal := store.(sessionstore.ControlStore)
		require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
		next, err := journal.ClaimControl(ctx, randomToken(), ControlLeaseTTL)
		require.NoError(t, err)
		p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(ControlLeaseTTL)})
		require.Same(t, c, p.rebuildCall(c.lease))
		c.mu.Unlock()
		locked = false
		require.Never(t, poisoned, 500*time.Millisecond, time.Millisecond, "a stale waiter must leave the recovered call usable")
		require.True(t, a.runs(id))
		p.mu.Lock()
		terminal := c.terminal
		p.mu.Unlock()
		require.False(t, terminal)
	})
}

func TestRelayEpisodeSameTermAuthorityBlipWhileWaiting(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		guard := *p.restart.guard.Load()
		lease, err := store.(sessionstore.ControlStore).RenewControl(context.Background(), guard.lease, 5*time.Second)
		require.NoError(t, err)
		guard.lease, guard.deadline = lease, time.Now().Add(5*time.Second)
		p.restart.guard.Store(&guard)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.mu.Lock()
		locked := true
		defer func() {
			if locked {
				c.mu.Unlock()
			}
		}()
		_, err = p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit))
		require.NoError(t, err)
		ended := func() bool { c.stateMu.Lock(); defer c.stateMu.Unlock(); return c.relayEndReason != "" }
		require.Eventually(t, ended, time.Second, time.Millisecond)
		expired := guard
		expired.deadline = time.Now().Add(-time.Millisecond)
		p.restart.guard.Store(&expired)
		c.mu.Unlock()
		locked = false
		terminal := func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.terminal }
		require.Never(t, terminal, 250*time.Millisecond, time.Millisecond, "termination must wait through a same-term authority gap")
		guard.deadline = time.Now().Add(2 * time.Second)
		p.restart.guard.Store(&guard)
		require.Eventually(t, terminal, time.Second, time.Millisecond, "same-term renewal must still enforce the expired deadline")
	})
}

type episodeControlWorker struct{ *blockingControlWorker }

func (w *episodeControlWorker) AgentControl(ctx context.Context, _ string, m agent.ControlMessage) (agent.ControlReceipt, error) {
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.release:
		if err := ctx.Err(); err != nil {
			return agent.ControlReceipt{}, err
		}
		return agent.ControlReceipt{ID: m.ID, Sequence: m.Sequence}, nil
	case <-ctx.Done():
		<-w.release
		return agent.ControlReceipt{}, ctx.Err()
	}
}

func TestRelayEpisodeManagedExhaustionCancelsBusyHolder(t *testing.T) {
	for _, successor := range []bool{false, true} {
		name := "same-term"
		if successor {
			name = "successor"
		}
		t.Run(name, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, a, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				authorizeTestControl(t, p)
				ctx := context.Background()
				journal := store.(sessionstore.ControlStore)
				guard := *p.restart.guard.Load()
				lease, err := journal.RenewControl(ctx, guard.lease, 5*time.Second)
				require.NoError(t, err)
				guard.lease = lease
				guard.deadline = time.Now().Add(5 * time.Second)
				p.restart.guard.Store(&guard)
				w := &episodeControlWorker{&blockingControlWorker{fakeWorker: a.fakeWorker, entered: make(chan struct{}), release: make(chan struct{})}}
				p.mu.Lock()
				p.workers["a"].worker = w
				p.mu.Unlock()
				var release sync.Once
				unblock := func() { release.Do(func() { close(w.release) }) }
				server := httptest.NewServer(p.Handler())
				defer server.Close()
				response, err := server.Client().Get(server.URL + "/calls/" + id + "/events")
				require.NoError(t, err)
				defer response.Body.Close()
				event := make(chan string, 1)
				go func() {
					scan := bufio.NewScanner(response.Body)
					ended := false
					for scan.Scan() {
						line := scan.Text()
						if line == "event: ended" {
							ended = true
						}
						if ended && strings.HasPrefix(line, "data: ") {
							event <- line
							return
						}
					}
					event <- ""
				}()
				sent, done := make(chan error, 1), make(chan struct{})
				go func() {
					_, err := p.AgentControl(ctx, id, agent.ControlMessage{ID: "1", Sequence: 1, Name: "start"})
					sent <- err
					close(done)
				}()
				t.Cleanup(func() { unblock(); <-done })
				select {
				case <-w.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("Control did not acquire the call")
				}
				c, err := p.lookup(id)
				require.NoError(t, err)
				var cancelled atomic.Bool
				c.controlMu.Lock()
				cancel := c.controlCancel
				c.controlCancel = func() { cancel(); cancelled.Store(true) }
				c.controlMu.Unlock()
				episode, err := p.relayRecoveryEpisode(ctx, c, time.Now().Add(-relayRecoveryLimit))
				require.NoError(t, err)
				require.Eventually(t, cancelled.Load, time.Second, time.Millisecond, "deadline must cancel the busy holder before acquiring its lock")
				select {
				case data := <-event:
					require.Contains(t, data, "relay_change_failed")
				case <-time.After(time.Second):
					t.Fatal("busy holder prevented deadline event")
				}
				if successor {
					require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
					next, err := journal.ClaimControl(ctx, randomToken(), 5*time.Second)
					require.NoError(t, err)
					require.NoError(t, journal.DeleteControlIntent(ctx, episode.intent.ID, episode.intent.Token))
					p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(5 * time.Second)})
					require.Same(t, c, p.rebuildCall(c.lease))
				}
				unblock()
				require.ErrorIs(t, <-sent, context.Canceled)
				if successor {
					require.Never(t, func() bool { c.stateMu.Lock(); defer c.stateMu.Unlock(); return c.relayEndReason != "" }, 500*time.Millisecond, time.Millisecond)
					_, err = p.AgentControl(ctx, id, agent.ControlMessage{ID: "2", Sequence: 2, Name: "start"})
					require.NoError(t, err, "cancel-only stale expiry must leave the call usable")
				} else {
					require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.terminal }, time.Second, time.Millisecond)
				}
			})
		})
	}
}

func TestRelayEpisodeRetirementStopsDriver(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, _, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		guard := *p.restart.guard.Load()
		p.mu.Lock()
		for id, binding := range p.relaySet {
			binding.lost = true
			p.relaySet[id] = binding
		}
		p.mu.Unlock()
		ctx := context.Background()
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.startRelayRecovery(ctx, c, false))
		c.relayEpisodeMu.Lock()
		old := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		t.Cleanup(func() {
			c.hungUp.Store(true)
			require.Eventually(t, func() bool { c.relayEpisodeMu.Lock(); defer c.relayEpisodeMu.Unlock(); return !old.running }, time.Second, time.Millisecond)
		})
		require.Eventually(t, func() bool { return journal.owners.Load() > 0 }, time.Second, time.Millisecond)
		require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
		next, err := journal.ClaimControl(ctx, randomToken(), ControlLeaseTTL)
		require.NoError(t, err)
		require.NoError(t, journal.DeleteControlIntent(ctx, old.intent.ID, old.intent.Token))
		p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(ControlLeaseTTL)})
		fresh, err := p.relayRecoveryEpisode(ctx, c, time.Now())
		require.NoError(t, err)
		require.NotEqual(t, old.intent.Token, fresh.intent.Token)
		require.NoError(t, p.startRelayRecovery(ctx, c, false))
		require.Eventually(t, func() bool { c.relayEpisodeMu.Lock(); defer c.relayEpisodeMu.Unlock(); return !old.running }, time.Second, time.Millisecond, "retired driver must stop without waiting for its old deadline")
		c.relayEpisodeMu.Lock()
		cleaned := old.cleanupReady
		c.relayEpisodeMu.Unlock()
		require.False(t, cleaned, "retired driver must not complete the old journal intent")
	})
}

func TestRelayEpisodeRebuildSharesJournalSnapshot(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, a, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		otherID, _, lease, err := a.CreateSessionAtRelay(context.Background(), "offer", "r1")
		require.NoError(t, err)
		p.mu.Lock()
		p.calls[otherID] = &call{id: otherID, lease: lease, relayCapable: true}
		p.mu.Unlock()
		for _, callID := range []string{id, otherID} {
			c, err := p.lookup(callID)
			require.NoError(t, err)
			e, err := p.relayRecoveryEpisode(context.Background(), c, time.Now())
			require.NoError(t, err)
			c.relayEpisodeMu.Lock()
			e.completed.Store(true)
			e.once.Do(func() { close(e.done) })
			c.relayEpisode = nil
			c.relayEpisodeMu.Unlock()
		}
		journal.reads.Store(0)
		p.SetRelayReady(true)
		require.NoError(t, p.rebuild(context.Background(), nil))
		require.EqualValues(t, 1, journal.reads.Load(), "rebuild must share the journal snapshot it already read")
	})
}

func TestLostSlotPreservesRecoveryAlreadyTargetingLiveSlot(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
		_, assignment := waitRelayPending(t, store, id)
		require.Equal(t, "r2", assignment.Pending)
		var cancelled atomic.Bool
		c.controlMu.Lock()
		original := c.relayCancel
		c.relayCancel = func() { cancelled.Store(true); original() }
		c.controlMu.Unlock()
		defer func() {
			original()
			require.NoError(t, p.End(context.Background(), id))
			require.Eventually(t, func() bool {
				c.relayEpisodeMu.Lock()
				defer c.relayEpisodeMu.Unlock()
				return c.relayEpisode == nil || c.relayEpisode.completed.Load()
			}, time.Second, time.Millisecond)
		}()
		require.NoError(t, p.loseRelay(context.Background(), "r1"))
		require.False(t, cancelled.Load(), "loss and caller triggers must join the same live-target attempt")
	})
}

// Pause the first call lookup after rebuild has read its journal snapshot.
type episodeRebuildStore struct {
	*relayJournalStore
	block            atomic.Bool
	entered, release chan struct{}
}

func (s *episodeRebuildStore) Get(ctx context.Context, id string) (sessionstore.Lease, error) {
	if s.block.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return sessionstore.Lease{}, ctx.Err()
		}
	}
	return s.Store.Get(ctx, id)
}
func TestRelayEpisodeRebuildRejectsEarlierTermSnapshot(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &episodeRebuildStore{relayJournalStore: &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}, entered: make(chan struct{}), release: make(chan struct{})}
		p, a, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		ctx := context.Background()
		guard := *p.restart.guard.Load()
		expired := guard
		expired.deadline = time.Now().Add(-time.Second)
		p.restart.guard.Store(&expired)
		c, err := p.lookup(id)
		require.NoError(t, err)
		e, err := p.relayRecoveryEpisode(ctx, c, time.Now().Add(-relayRecoveryLimit))
		require.NoError(t, err)
		p.SetRelayReady(true)
		require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
		termB, err := journal.ClaimControl(ctx, guard.lease.Holder, 5*time.Second)
		require.NoError(t, err)
		guard.lease = termB
		p.restart.guard.Store(&controlGuard{lease: termB, deadline: time.Now().Add(5 * time.Second)})
		journal.block.Store(true)
		done := make(chan error, 1)
		go func() { done <- p.rebuild(ctx, nil) }()
		var once sync.Once
		unblock := func() { once.Do(func() { close(journal.release) }) }
		defer unblock()
		select {
		case <-journal.entered:
		case <-time.After(time.Second):
			t.Fatal("rebuild did not reach journal call lookup")
		}
		require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
		successor, err := journal.ClaimControl(ctx, randomToken(), 5*time.Second)
		require.NoError(t, err)
		require.NoError(t, journal.DeleteControlIntent(ctx, e.intent.ID, e.intent.Token))
		require.NoError(t, journal.ReleaseControl(ctx, successor))
		next, err := journal.ClaimControl(ctx, guard.lease.Holder, 5*time.Second)
		require.NoError(t, err)
		p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(5 * time.Second)})
		unblock()
		require.ErrorIs(t, <-done, ErrNotReady, "a rebuild snapshot must retain its reading term")
		records, err := journal.ControlIntents(ctx)
		require.NoError(t, err)
		require.Empty(t, records, "stale rebuild must not rewrite a deleted intent")
		require.NoError(t, p.rebuild(ctx, nil))
		require.Never(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.terminal }, 200*time.Millisecond, time.Millisecond)
		require.True(t, a.runs(id))
	})
}
func TestRelayEpisodeEarlierTermDriverStops(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, _, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		p.mu.Lock()
		for id, b := range p.relaySet {
			b.lost = true
			p.relaySet[id] = b
		}
		p.mu.Unlock()
		ctx := context.Background()
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.startRelayRecovery(ctx, c, false))
		c.relayEpisodeMu.Lock()
		e := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.Eventually(t, func() bool { return journal.owners.Load() > 0 }, time.Second, time.Millisecond)
		guard := *p.restart.guard.Load()
		require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
		next, err := journal.ClaimControl(ctx, randomToken(), 5*time.Second)
		require.NoError(t, err)
		p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(5 * time.Second)})
		require.Eventually(t, func() bool { c.relayEpisodeMu.Lock(); defer c.relayEpisodeMu.Unlock(); return !e.running }, time.Second, time.Millisecond, "old driver must stop on a term change without retirement")
		c.relayEpisodeMu.Lock()
		e.running = true
		c.relayEpisodeMu.Unlock() // model an old driver still unwinding
		rearmed, err := p.relayRecoveryEpisode(ctx, c, time.Now())
		require.NoError(t, err)
		require.Same(t, e, rearmed)
		c.relayEpisodeMu.Lock()
		running := e.running
		c.relayEpisodeMu.Unlock()
		require.False(t, running, "same-token re-arm must permit a new term driver")
		require.NoError(t, p.startRelayRecovery(ctx, c, false))
		require.Eventually(t, func() bool { c.relayEpisodeMu.Lock(); defer c.relayEpisodeMu.Unlock(); return e.running }, time.Second, time.Millisecond)
	})
}

func TestRelayEpisodeRearmWaitsForIntentWrite(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		journal := &relayJournalStore{Store: store, ControlStore: store.(sessionstore.ControlStore), RelayAssignments: store.(sessionstore.RelayAssignments)}
		p, _, _, id := relayFailurePlane(t, journal)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		ctx := context.Background()
		guard := *p.restart.guard.Load()
		expired := guard
		expired.deadline = time.Now().Add(-time.Second)
		p.restart.guard.Store(&expired)
		c, err := p.lookup(id)
		require.NoError(t, err)
		e, err := p.relayRecoveryEpisode(ctx, c, time.Now().Add(-relayRecoveryLimit))
		require.NoError(t, err)
		require.NoError(t, journal.ReleaseControl(ctx, guard.lease))
		next, err := journal.ClaimControl(ctx, randomToken(), 5*time.Second)
		require.NoError(t, err)
		p.restart.guard.Store(&controlGuard{lease: next, deadline: time.Now().Add(5 * time.Second)})
		journal.putFail.Store(true)
		_, err = p.relayRecoveryEpisode(ctx, c, time.Now())
		require.ErrorIs(t, err, sessionstore.ErrTransient)
		require.Never(t, func() bool { c.stateMu.Lock(); defer c.stateMu.Unlock(); return c.relayEndReason != "" }, 200*time.Millisecond, time.Millisecond, "failed write must not arm the new term watchdog")
		journal.putFail.Store(false)
		rearmed, err := p.relayRecoveryEpisode(ctx, c, time.Now())
		require.NoError(t, err)
		require.Same(t, e, rearmed)
		require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.terminal }, time.Second, time.Millisecond)
	})
}
