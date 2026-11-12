package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Tests for the durable episode decision (ADR 0016 §7): one compare-and-set on
// the confirmation record decides ended or completed for an episode token.

// armEpisode opens an episode whose deadline is `in` from now and waits until
// its begin is definite.
func armEpisode(t *testing.T, p *Plane, c *call, in time.Duration) *relayEpisode {
	t.Helper()
	e, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(in-relayRecoveryLimit))
	require.NoError(t, err)
	select {
	case <-e.armed:
	case <-time.After(5 * time.Second):
		t.Fatal("episode never armed")
	}
	return e
}

func episodeOf(t *testing.T, store sessionstore.Store, id string) *sessionstore.RelayEpisode {
	t.Helper()
	_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
	require.NoError(t, err)
	return rec.Episode
}

func awaitDone(t *testing.T, e *relayEpisode) {
	t.Helper()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("episode never finished")
	}
}

func endReason(c *call) string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.relayEndReason
}

// In-flight switch adoption: full A was saved before the commit, then the
// source slot was lost. Begin binds the switch because its target is live,
// the worker commits afterwards, and the deadline promotes the proof.
func TestInFlightSwitchToLiveSlotJoinsEpisode(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		markLost(p, "r1")
		e := armEpisode(t, p, c, 400*time.Millisecond)
		require.Equal(t, e.proposal().Token, attemptOf(t, store, id).Attempt.Episode, "begin binds the in-flight switch to a live slot")
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		awaitDone(t, e)
		require.Equal(t, sessionstore.RelayEpisodeCompleted, episodeOf(t, store, id).Outcome, "valid proof is never ended")
		require.Empty(t, endReason(c))
	})
}

// Restart restores the detection-based watchdog of an unsupported call
// directly: no begin and no confirmation write.
func TestRestartRestoresUnsupportedWatchdog(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayCapable, c.relayMode = false, sessionstore.RelayConfirmNone
		lease := c.lease
		c.stateMu.Unlock()
		intent := sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKindUnsupported, Started: time.Now().Add(200*time.Millisecond - relayRecoveryLimit), Lease: lease}
		require.NoError(t, store.(sessionstore.ControlStore).PutControlIntent(context.Background(), intent))
		require.NoError(t, p.restoreRelayEpisode(context.Background(), c, intent))
		require.Eventually(t, func() bool { return endReason(c) == "relay_lost" }, 5*time.Second, 10*time.Millisecond)
		require.Eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return c.terminal }, 5*time.Second, 10*time.Millisecond)
	})
}

// A final outcome under the watcher's own token is closed and cleaned up, so a
// later recovery starts a new episode at the next seq.
func TestCompletedEpisodePermitsLaterRecovery(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		e := armEpisode(t, p, c, 300*time.Millisecond)
		a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		done := rec
		episode := *rec.Episode
		episode.Outcome, episode.Seq = sessionstore.RelayEpisodeCompleted, episode.Seq+1
		done.Episode = &episode
		require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, a, rec, done, time.Time{}, time.Time{}))
		awaitDone(t, e)
		require.Empty(t, endReason(c))
		require.NoError(t, p.startRelayRecovery(context.Background(), c, false), "completed permits a later recovery")
		c.relayEpisodeMu.Lock()
		next := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, next)
		require.NotEqual(t, e.proposal().Token, next.proposal().Token)
		require.Equal(t, episode.Seq+1, next.proposal().Seq)
	})
}

// A refused begin is re-proposed within the term while the call's current slot
// is still lost; otherwise the proposal is discarded.
func TestRefusedBeginReproposesWhileSlotLost(t *testing.T) {
	for _, lost := range []bool{true, false} {
		t.Run(map[bool]string{true: "lost", false: "live"}[lost], func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, _, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				c, err := p.lookup(id)
				require.NoError(t, err)
				if lost {
					markLost(p, "r1")
				}
				c.stateMu.Lock()
				lease := c.lease
				c.stateMu.Unlock()
				stale := sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now(), Lease: lease, Seq: 7}
				require.NoError(t, store.(sessionstore.ControlStore).PutControlIntent(context.Background(), stale))
				e := p.installRelayEpisode(c, stale)
				if lost {
					select {
					case <-e.armed:
					case <-time.After(5 * time.Second):
						t.Fatal("never re-proposed")
					}
					ep := episodeOf(t, store, id)
					require.True(t, ep.Open())
					require.NotEqual(t, stale.Token, ep.Token)
					require.Equal(t, uint64(1), ep.Seq)
					return
				}
				awaitDone(t, e)
				require.Nil(t, episodeOf(t, store, id), "a stale proposal on a live slot is discarded")
				// Cleanup closes done before it deletes the journal entry.
				require.Eventually(t, func() bool {
					records, err := store.(sessionstore.ControlStore).ControlIntents(context.Background())
					require.NoError(t, err)
					for _, r := range records {
						if r.ID == stale.ID {
							return false
						}
					}
					return true
				}, 5*time.Second, 10*time.Millisecond)
			})
		})
	}
}

// ChangeRelay refuses while an episode owns the call, and an episode that
// ends during its wait is returned directly, without abort or rollback.
func TestChangeRelayDefersToEpisode(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		result := make(chan error, 1)
		go func() { result <- p.ChangeRelay(context.Background(), id, "r2") }()
		_, pending := waitRelayPending(t, store, id)
		require.Eventually(t, func() bool {
			_, rec, e := confirmations(store).RelayConfirmation(context.Background(), id)
			return e == nil && rec.Attempt != nil
		}, 2*time.Second, time.Millisecond)
		e := armEpisode(t, p, c, 10*time.Second)
		require.Equal(t, relayEnded, p.endRelayEpisode(context.Background(), c, e.proposal().Token, "relay_change_failed", false))
		select {
		case err := <-result:
			require.ErrorIs(t, err, errRelayEpisodeEnded)
		case <-time.After(5 * time.Second):
			t.Fatal("ChangeRelay did not return")
		}
		_, got, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, pending, got, "no abort or rollback write under an ended episode")

		// While an episode is open, a planned change does not start.
		p2, _, _, id2 := relayFailurePlane(t, sessionstore.NewMemory())
		stopRelayRecoveries(t, p2)
		c2, err := p2.lookup(id2)
		require.NoError(t, err)
		armEpisode(t, p2, c2, 10*time.Second)
		require.ErrorIs(t, p2.ChangeRelay(context.Background(), id2, "r2"), ErrMoveInProgress)
		_, got, err = p2.store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id2)
		require.NoError(t, err)
		require.Empty(t, got.Pending)
	})
}

// The one end operation promotes proof owned by the episode, for the timeout
// and for immediate ends; a timeout also waits for the store deadline.
func TestEndPromotesProofAndWaitsForDeadline(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		e := armEpisode(t, p, c, 10*time.Second)
		require.Equal(t, relayUncertain, p.endRelayEpisode(context.Background(), c, e.proposal().Token, "relay_change_failed", true), "a timeout end waits for the store deadline")
		require.True(t, episodeOf(t, store, id).Open())
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		// A last: the acknowledgement itself completes the owning episode.
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		require.Equal(t, sessionstore.RelayEpisodeCompleted, episodeOf(t, store, id).Outcome)
		require.Equal(t, relayCompleted, p.endRelayEpisode(context.Background(), c, e.proposal().Token, "relay_change_failed", false), "an end after completion follows the stored outcome")
	})
}

// Commit last: proof that exists when the end decision runs is promoted.
func TestEndPromotesCommitLastProof(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		e := armEpisode(t, p, c, 10*time.Second)
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		require.Equal(t, relayCompleted, p.endRelayEpisode(context.Background(), c, e.proposal().Token, "confirmation_unsupported", false))
		require.Equal(t, sessionstore.RelayPhaseConfirmed, attemptOf(t, store, id).Attempt.Phase)
	})
}

// A success without an episode advances seq, so a delayed proposal from
// before it can never begin and end the recovered call.
func TestSuccessWithoutEpisodeFencesStaleProposal(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		stale := sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKind, Started: now, Lease: lease, Seq: 1}
		committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, committed))
		_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		require.NoError(t, p.confirmRelaySuccess(context.Background(), id, committed, rec), "a planned success without an episode")
		require.NoError(t, store.(sessionstore.ControlStore).PutControlIntent(context.Background(), stale))
		e := p.installRelayEpisode(c, stale)
		awaitDone(t, e)
		ep := episodeOf(t, store, id)
		require.False(t, ep.Open(), "the stale proposal never begins")
		require.Empty(t, ep.Token)
		require.Empty(t, endReason(c))
	})
}

// #323 item 1: a stale watchdog whose token was replaced by a successor's
// episode stands down: no end, no publication.
func TestStaleWatchdogStandsDown(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := watchEvents(t, c)
		e := armEpisode(t, p, c, 300*time.Millisecond)
		a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		successor := rec
		episode := sessionstore.RelayEpisode{Seq: rec.Episode.Seq + 1, Token: randomEpisodeToken(), StoreDeadline: rec.Episode.StoreDeadline.Add(time.Minute), Outcome: sessionstore.RelayEpisodeCompleted}
		successor.Episode = &episode
		require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, a, rec, successor, time.Time{}, time.Time{}))
		awaitEvent(t, events, "stood-down")
		awaitDone(t, e)
		require.Empty(t, endReason(c))
		_, err = store.Get(context.Background(), id)
		require.NoError(t, err, "the recovered call stays up")
	})
}

// After ended, no acknowledgement: not for a pending attempt and not for an
// already-confirmed record.
func TestEndedEpisodeRefusesAcknowledgement(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending", false: "confirmed"}[pending], func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
				stopRelayRecoveries(t, p)
				c, err := p.lookup(id)
				require.NoError(t, err)
				now := time.Now()
				attempt := sessionstore.RelayAttempt{Required: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)}
				if !pending {
					attempt.Decoded = []string{"0"}
				}
				lease, assigned := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, attempt)
				if !pending {
					committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
					require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, assigned, committed))
					_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
					require.NoError(t, err)
					require.NoError(t, p.confirmRelaySuccess(context.Background(), id, committed, rec)) // an earlier confirmation
				}
				markLost(p, "r2") // the attempt's target is lost: begin never binds it
				e := armEpisode(t, p, c, 10*time.Second)
				require.Empty(t, attemptOf(t, store, id).Attempt.Episode, "the attempt stays outside the episode")
				c.mu.Lock() // keep the session while the record is inspected
				defer c.mu.Unlock()
				require.Equal(t, relayEnded, p.endRelayEpisode(context.Background(), c, e.proposal().Token, "relay_change_failed", false))
				before := attemptOf(t, store, id).Attempt
				c.stateMu.Lock()
				c.relayEndReason = "" // isolate the store check from the in-memory one
				c.stateMu.Unlock()
				server := httptest.NewServer(p.Handler())
				defer server.Close()
				require.Equal(t, http.StatusGone, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
				require.Equal(t, before.Decoded, attemptOf(t, store, id).Attempt.Decoded, "no A is retained after ended")
			})
		})
	}
}

// ambiguousStore, while lose is set, applies a confirmation write and then
// reports an uncertain error, as a timeout after the write would. With drift
// set it applies a record whose episode differs from the requested one.
type ambiguousStore struct {
	relayStore
	lose, drift atomic.Bool
}

func (s *ambiguousStore) CASRelayConfirmation(ctx context.Context, id string, at sessionstore.RelayAssignment, from, to sessionstore.RelayConfirmation, notBefore, notAfter time.Time) error {
	if !s.lose.Load() {
		return s.relayStore.CASRelayConfirmation(ctx, id, at, from, to, notBefore, notAfter)
	}
	if s.drift.Load() && to.Episode != nil {
		episode := *to.Episode
		episode.Detail = "drifted"
		to.Episode = &episode
	}
	if err := s.relayStore.CASRelayConfirmation(ctx, id, at, from, to, notBefore, notAfter); err != nil {
		return err
	}
	return errors.New("store reply lost")
}

// decisionCAS proves an uncertain write by reading back the whole record,
// the episode included.
func TestReadBackComparesWholeEpisode(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "drift"}[drift], func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, backing sessionstore.Store) {
				store := &ambiguousStore{relayStore: backing.(relayStore)}
				p, _, _, id := relayFailurePlane(t, store)
				a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
				require.NoError(t, err)
				next := rec
				next.Episode = &sessionstore.RelayEpisode{Seq: 1, Token: "t", StoreDeadline: time.Now().Add(time.Minute), Outcome: sessionstore.RelayEpisodeOpen}
				store.lose.Store(true)
				store.drift.Store(drift)
				err = p.decisionCAS(context.Background(), id, a, rec, next, time.Time{}, time.Time{})
				if drift {
					require.Error(t, err, "an episode-only difference is a different record")
				} else {
					require.NoError(t, err, "the read-back proves an exact write")
				}
			})
		})
	}
}

// An end write that lands but loses its reply is settled by its read-back:
// the end is definite at once. A record that differs from the requested end
// stays uncertain.
func TestUncertainEndWriteIsReadBack(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "drift"}[drift], func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, backing sessionstore.Store) {
				store := &ambiguousStore{relayStore: backing.(relayStore)}
				p, _, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				c, err := p.lookup(id)
				require.NoError(t, err)
				a, rec, err := store.RelayConfirmation(context.Background(), id)
				require.NoError(t, err)
				open := rec
				open.Episode = &sessionstore.RelayEpisode{Seq: 1, Token: randomEpisodeToken(), StoreDeadline: time.Now().Add(time.Minute), Outcome: sessionstore.RelayEpisodeOpen}
				require.NoError(t, store.CASRelayConfirmation(context.Background(), id, a, rec, open, time.Time{}, time.Time{}))
				store.lose.Store(true)
				store.drift.Store(drift)
				outcome := p.endRelayEpisode(context.Background(), c, open.Episode.Token, "relay_change_failed", false)
				require.Equal(t, sessionstore.RelayEpisodeEnded, episodeOf(t, store, id).Outcome, "the end write landed")
				if drift {
					require.Equal(t, relayUncertain, outcome, "a different stored end is not this write")
				} else {
					require.Equal(t, relayEnded, outcome, "the read-back proves the uncertain end write")
				}
			})
		})
	}
}

// A removed session is gone at once, never waited on.
func TestEndOnRemovedSessionIsGone(t *testing.T) {
	store := sessionstore.NewMemory()
	p, _, _, id := relayFailurePlane(t, store)
	c, err := p.lookup(id)
	require.NoError(t, err)
	lease, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, store.Release(context.Background(), lease))
	require.Equal(t, relayGone, p.endRelayEpisode(context.Background(), c, "any", "relay_change_failed", true))
}

// Restart reconciles each journal proposal with the durable record.
func TestRestoreRelayEpisodeRows(t *testing.T) {
	type row struct {
		name   string
		seed   func(t *testing.T, p *Plane, store sessionstore.Store, id string, intent *sessionstore.ControlIntent)
		intent func(intent *sessionstore.ControlIntent)
		kept   bool // the journal still holds the proposal's token
	}
	setEpisode := func(t *testing.T, store sessionstore.Store, id string, ep sessionstore.RelayEpisode) {
		a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		next := rec
		next.Episode = &ep
		require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, a, rec, next, time.Time{}, time.Time{}))
	}
	rows := []row{
		{"unknown format", nil, func(i *sessionstore.ControlIntent) { i.Seq = 0 }, false},
		{"own completed", func(t *testing.T, p *Plane, store sessionstore.Store, id string, i *sessionstore.ControlIntent) {
			setEpisode(t, store, id, sessionstore.RelayEpisode{Seq: 2, Token: i.Token, Outcome: sessionstore.RelayEpisodeCompleted})
		}, nil, false},
		{"stale seq", func(t *testing.T, p *Plane, store sessionstore.Store, id string, i *sessionstore.ControlIntent) {
			setEpisode(t, store, id, sessionstore.RelayEpisode{Seq: 3})
		}, nil, false},
		{"own open", func(t *testing.T, p *Plane, store sessionstore.Store, id string, i *sessionstore.ControlIntent) {
			setEpisode(t, store, id, sessionstore.RelayEpisode{Seq: 1, Token: i.Token, StoreDeadline: time.Now().Add(time.Minute), Outcome: sessionstore.RelayEpisodeOpen})
		}, nil, true},
		{"next proposal", nil, nil, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			store := sessionstore.NewMemory()
			p, _, _, id := relayFailurePlane(t, store)
			stopRelayRecoveries(t, p)
			c, err := p.lookup(id)
			require.NoError(t, err)
			intent := sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now(), Seq: 1}
			if r.intent != nil {
				r.intent(&intent)
			}
			if r.seed != nil {
				r.seed(t, p, store, id, &intent)
			}
			require.NoError(t, store.PutControlIntent(context.Background(), intent))
			require.NoError(t, p.restoreRelayEpisode(context.Background(), c, intent))
			records, err := store.ControlIntents(context.Background())
			require.NoError(t, err)
			kept := false
			for _, rec := range records {
				kept = kept || rec.Token == intent.Token
			}
			require.Equal(t, r.kept, kept)
		})
	}

	// Another token open in the record replaces the proposal in the journal.
	store := sessionstore.NewMemory()
	p, _, _, id := relayFailurePlane(t, store)
	stopRelayRecoveries(t, p)
	c, err := p.lookup(id)
	require.NoError(t, err)
	successor := sessionstore.RelayEpisode{Seq: 2, Token: randomEpisodeToken(), StoreDeadline: time.Now().Add(time.Minute), Outcome: sessionstore.RelayEpisodeOpen}
	setEpisode(t, store, id, successor)
	intent := sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now(), Seq: 1}
	require.NoError(t, store.PutControlIntent(context.Background(), intent))
	require.NoError(t, p.restoreRelayEpisode(context.Background(), c, intent))
	records, err := store.ControlIntents(context.Background())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, successor.Token, records[0].Token)
	require.Equal(t, successor.Seq, records[0].Seq)
}

// A historical confirmation on a live slot (for example before a caller
// stall) is never adopted by a later episode, so it can never complete it.
func TestBeginNeverAdoptsHistoricalConfirmation(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, committed))
		_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		require.NoError(t, p.confirmRelaySuccess(context.Background(), id, committed, rec))
		e := armEpisode(t, p, c, 10*time.Second)
		require.Empty(t, attemptOf(t, store, id).Attempt.Episode)
		require.Equal(t, relayEnded, p.endRelayEpisode(context.Background(), c, e.proposal().Token, "relay_change_failed", false), "an earlier confirmation never completes a later episode")
	})
}

// gatedJournal holds the first journal read until an episode opens, or for
// 300 ms: an episode that begins during reconciliation shows up in the record.
type gatedJournal struct {
	*sessionstore.Memory
	gate atomic.Bool
	id   string
}

func (s *gatedJournal) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	if s.gate.CompareAndSwap(true, false) {
		for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, rec, err := s.RelayConfirmation(ctx, s.id); err == nil && rec.Episode.Open() {
				break
			}
		}
	}
	return s.Memory.ControlIntents(ctx)
}

// Code review 1, P1: no watcher runs before journal reconciliation picks the
// final proposal. The journaled proposal (first detection) is the one that
// begins; a temporary proposal never opens an episode.
func TestWatcherStartsAfterJournalReconciliation(t *testing.T) {
	store := &gatedJournal{Memory: sessionstore.NewMemory()}
	p, _, _, id := relayFailurePlane(t, store)
	stopRelayRecoveries(t, p)
	store.id = id
	c, err := p.lookup(id)
	require.NoError(t, err)
	c.stateMu.Lock()
	lease := c.lease
	c.stateMu.Unlock()
	saved := sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now().Add(-time.Second), Lease: lease, Seq: 1}
	require.NoError(t, store.PutControlIntent(context.Background(), saved))
	store.gate.Store(true)
	e, err := p.relayRecoveryEpisode(context.Background(), c, time.Now())
	require.NoError(t, err)
	require.Equal(t, saved.Token, e.proposal().Token)
	require.Eventually(t, func() bool { return episodeOf(t, store, id).Open() }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, saved.Token, episodeOf(t, store, id).Token, "only the reconciled proposal begins")
}

// Code review 1, P1: an open episode that no watcher owns (a replaced
// watcher's begin committed) is adopted, armed and enforced at its deadline.
func TestUnownedOpenEpisodeIsAdopted(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := watchEvents(t, c)
		a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		orphan := rec
		deadline, err := p.convertClock(context.Background(), id, time.Now().Add(time.Second), true)
		require.NoError(t, err)
		orphan.Episode = &sessionstore.RelayEpisode{Seq: 1, Token: randomEpisodeToken(), StoreDeadline: deadline, Outcome: sessionstore.RelayEpisodeOpen}
		require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, a, rec, orphan, time.Time{}, time.Time{}))
		_, err = p.relayRecoveryEpisode(context.Background(), c, time.Now())
		require.NoError(t, err)
		awaitEvent(t, events, "adopted")
		c.relayEpisodeMu.Lock()
		owner := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, owner)
		require.Equal(t, orphan.Episode.Token, owner.proposal().Token)
		require.Eventually(t, func() bool { return endReason(c) == "relay_change_failed" }, 5*time.Second, 5*time.Millisecond, "the adopted episode is enforced at its deadline")
	})
}

// cleanupFault fails journal deletes while fail is set.
type cleanupFault struct {
	*sessionstore.Memory
	fail atomic.Bool
}

func (s *cleanupFault) DeleteControlIntent(ctx context.Context, id, token string) error {
	if s.fail.Load() {
		return errors.New("journal unavailable")
	}
	return s.Memory.DeleteControlIntent(ctx, id, token)
}

// Code review 1, P1: a failed proposal cleanup is retried by the next
// recovery instead of refusing it forever.
func TestFailedProposalCleanupIsRetried(t *testing.T) {
	store := &cleanupFault{Memory: sessionstore.NewMemory()}
	p, _, _, id := relayFailurePlane(t, store)
	stopRelayRecoveries(t, p)
	c, err := p.lookup(id)
	require.NoError(t, err)
	first := armEpisode(t, p, c, 10*time.Second)
	// Cleanup follows a decided episode. A record left open under the first
	// token would be adopted by the next watcher, which journals that token
	// again (issue #436).
	a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
	require.NoError(t, err)
	done := rec
	episode := *rec.Episode
	episode.Seq, episode.Outcome = episode.Seq+1, sessionstore.RelayEpisodeCompleted
	done.Episode = &episode
	require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, a, rec, done, time.Time{}, time.Time{}))
	store.fail.Store(true)
	p.cleanupRelayEpisode(context.Background(), c, first)
	store.fail.Store(false)
	next, err := p.relayRecoveryEpisode(context.Background(), c, time.Now())
	require.NoError(t, err, "one failed delete must not block every later recovery")
	require.NotSame(t, first, next)
	require.NotEqual(t, first.proposal().Token, next.proposal().Token, "the old proposal is not reloaded")
	select { // the next watcher has made its begin decision
	case <-next.armed:
	case <-time.After(5 * time.Second):
		t.Fatal("next episode never armed")
	}
	records, err := store.ControlIntents(context.Background())
	require.NoError(t, err)
	require.Len(t, records, 1, "the old proposal is deleted")
	require.Equal(t, next.proposal().Token, records[0].Token)
}

// beginGate holds an episode's begin until release closes; passed closes once
// that write returned. With holdRead, the first record read made while begin
// is held releases it and returns only once the episode armed.
type beginGate struct {
	*sessionstore.Memory
	on, holdRead                 atomic.Bool
	held, release, passed, armed chan struct{}
	holdOnce, releaseOnce        sync.Once
}

func (s *beginGate) CASRelayConfirmation(ctx context.Context, id string, at sessionstore.RelayAssignment, from, to sessionstore.RelayConfirmation, notBefore, notAfter time.Time) error {
	if s.on.Load() && to.Episode.Open() && !from.Episode.Open() {
		held := false
		s.holdOnce.Do(func() { close(s.held); held = true })
		if held {
			<-s.release
			defer close(s.passed)
		}
	}
	return s.Memory.CASRelayConfirmation(ctx, id, at, from, to, notBefore, notAfter)
}

func (s *beginGate) RelayConfirmation(ctx context.Context, id string) (sessionstore.RelayAssignment, sessionstore.RelayConfirmation, error) {
	a, rec, err := s.Memory.RelayConfirmation(ctx, id)
	select {
	case <-s.held:
		if s.holdRead.Load() {
			s.releaseOnce.Do(func() {
				close(s.release)
				<-s.armed
			})
		}
	default:
	}
	return a, rec, err
}

// completionRace sets up R2 review of #436: a planned change's settlement
// confirms with no episode open (seq advances), then its recovery episode is
// proposed and its watcher's begin is held by the gate.
func completionRace(t *testing.T, holdRead bool) (*beginGate, *Plane, *call, *relayEpisode) {
	t.Helper()
	store := &beginGate{Memory: sessionstore.NewMemory(), held: make(chan struct{}), release: make(chan struct{}), passed: make(chan struct{}), armed: make(chan struct{})}
	store.holdRead.Store(holdRead)
	p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
	stopRelayRecoveries(t, p)
	c, err := p.lookup(id)
	require.NoError(t, err)
	var armOnce sync.Once
	hook := func(from *call, name string) {
		if from == c && name == "armed" {
			armOnce.Do(func() { close(store.armed) })
		}
	}
	relayWatchHooks.Store(&hook)
	t.Cleanup(func() { relayWatchHooks.Store(nil) })
	now := time.Now()
	lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
		Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
	committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
	require.NoError(t, store.CommitRelay(context.Background(), lease, pending, committed))
	_, rec, err := store.RelayConfirmation(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, p.confirmRelaySuccess(context.Background(), id, committed, rec))
	store.on.Store(true)
	e, err := p.relayRecoveryEpisode(context.Background(), c, now)
	require.NoError(t, err)
	<-store.held
	return store, p, c, e
}

// Completion reads the record before the episode begins; the episode then
// begins and arms before completion acts. Completion must not close an open
// episode: its watcher keeps it.
func TestCompletionNeverClosesEpisodeThatBeganMeanwhile(t *testing.T) {
	store, p, c, e := completionRace(t, true)
	err := p.completeRelayEpisode(context.Background(), c, e)
	require.ErrorIs(t, err, ErrMoveInProgress)
	require.True(t, episodeOf(t, store, c.id).Open())
	require.False(t, e.completed.Load(), "an open episode keeps its watcher")
}

// Completion that acts before the episode begins fences its proposal: the
// begin is refused, and no episode opens after the confirmed change.
func TestCompletionFencesUnbegunProposal(t *testing.T) {
	store, p, c, e := completionRace(t, false)
	require.NoError(t, p.completeRelayEpisode(context.Background(), c, e))
	close(store.release)
	<-store.passed
	require.False(t, episodeOf(t, store, c.id).Open(), "the fenced proposal never begins")
	records, err := store.ControlIntents(context.Background())
	require.NoError(t, err)
	require.Empty(t, records)
}

// seqFault fails confirmation-record reads while fail is set.
type seqFault struct {
	*sessionstore.Memory
	fail atomic.Bool
}

func (s *seqFault) RelayConfirmation(ctx context.Context, id string) (sessionstore.RelayAssignment, sessionstore.RelayConfirmation, error) {
	if s.fail.Load() {
		return sessionstore.RelayAssignment{}, sessionstore.RelayConfirmation{}, errors.New("store unavailable")
	}
	return s.Memory.RelayConfirmation(ctx, id)
}

// Code review 1, P1: a proposal is never created without its seq baseline,
// so it can never be rebased past a success that lands meanwhile and end the
// recovered call at an old deadline.
func TestUnreadableSeqCreatesNoProposal(t *testing.T) {
	store := &seqFault{Memory: sessionstore.NewMemory()}
	p, _, _, id := relayFailurePlane(t, store)
	stopRelayRecoveries(t, p)
	c, err := p.lookup(id)
	require.NoError(t, err)
	store.fail.Store(true)
	_, err = p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-10*time.Second))
	require.Error(t, err)
	store.fail.Store(false)
	detected := time.Now()
	e, err := p.relayRecoveryEpisode(context.Background(), c, detected)
	require.NoError(t, err)
	require.Equal(t, detected.Add(relayRecoveryLimit), e.deadline, "the later detection starts its own window")
}

// Code review 1, P1: once its episode completed, a driver starts no further
// attempt: the attempt write is fenced on its token still being open.
func TestCompletedEpisodeDriverStartsNoAttempt(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		e := armEpisode(t, p, c, 10*time.Second)
		a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		done := rec
		episode := *rec.Episode
		episode.Seq, episode.Outcome = episode.Seq+1, sessionstore.RelayEpisodeCompleted
		done.Episode = &episode
		require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, a, rec, done, time.Time{}, time.Time{}))
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		err = p.recoverRelay(ctx, c, false, map[string]bool{}, e)
		require.ErrorIs(t, err, errRelayEpisodeClosed)
		_, after, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		require.Nil(t, after.Attempt, "no attempt is created after completion")
	})
}

// Code review 1, P2: a watcher that stops for shutdown releases its driver.
func TestShutdownReleasesEpisodeDriver(t *testing.T) {
	store := &seqFault{Memory: sessionstore.NewMemory()}
	p, _, _, id := relayFailurePlane(t, store)
	stopRelayRecoveries(t, p)
	authorizeTestControl(t, p)
	c, err := p.lookup(id)
	require.NoError(t, err)
	_, rec, err := store.RelayConfirmation(context.Background(), id)
	require.NoError(t, err)
	require.Nil(t, rec.Episode)
	store.fail.Store(true) // begin stays uncertain; the driver waits for arming
	ctx := context.Background()
	c.stateMu.Lock()
	lease := c.lease
	c.stateMu.Unlock()
	c.relayEpisodeMu.Lock()
	c.relayEpisode = newRelayEpisode(sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now(), Lease: lease, Seq: 1})
	c.relayEpisodeMu.Unlock()
	require.NoError(t, p.startRelayRecoveryFrom(ctx, c, false, time.Now(), nil))
	c.relayEpisodeMu.Lock()
	e := c.relayEpisode
	running := e.running
	c.relayEpisodeMu.Unlock()
	require.True(t, running)
	lapsed := *p.restart.guard.Load()
	lapsed.deadline = time.Now().Add(-time.Millisecond)
	p.restart.guard.Store(&lapsed)
	p.restart.stopping.Store(true)
	require.Eventually(t, func() bool {
		c.relayEpisodeMu.Lock()
		defer c.relayEpisodeMu.Unlock()
		return !e.running
	}, 5*time.Second, 5*time.Millisecond, "the driver exits with its watcher")
}

// Code review 2: a watcher whose begin commits after its episode was closed
// hands the open token to a new owner instead of leaving it unwatched.
func TestClosedWatcherHandsOffCommittedBegin(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := make(chan string, 64)
		closed := make(chan *relayEpisode, 1)
		var once atomic.Bool
		hook := func(from *call, name string) {
			if from != c {
				return
			}
			if name == "begun" && once.CompareAndSwap(false, true) {
				// The begin CAS committed; the episode is replaced before the
				// watcher checks it.
				c.relayEpisodeMu.Lock()
				e := c.relayEpisode
				c.relayEpisode = nil
				c.relayEpisodeMu.Unlock()
				e.close()
				closed <- e
			}
			select {
			case events <- name:
			default:
			}
		}
		relayWatchHooks.Store(&hook)
		t.Cleanup(func() { relayWatchHooks.Store(nil) })
		_, err = p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(time.Second-relayRecoveryLimit))
		require.NoError(t, err)
		var orphan *relayEpisode
		select {
		case orphan = <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("begin never committed")
		}
		ep := episodeOf(t, store, id)
		require.True(t, ep.Open())
		require.Equal(t, orphan.proposal().Token, ep.Token, "the closed watcher's begin committed")
		awaitEvent(t, events, "adopted")
		c.relayEpisodeMu.Lock()
		owner := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, owner)
		require.NotSame(t, orphan, owner)
		require.Equal(t, ep.Token, owner.proposal().Token, "the new owner holds the committed token")
		require.Eventually(t, func() bool { return endReason(c) == "relay_change_failed" }, 5*time.Second, 5*time.Millisecond, "the adopted episode is enforced at its deadline")
	})
}

// completingAbortWorker is a worker without the C contract whose first abort
// lets the test complete the episode, as a concurrent completion would
// between the driver's begin and its immediate end.
type completingAbortWorker struct {
	*oldRelayWorker
	once     sync.Once
	complete func()
}

func (w *completingAbortWorker) AbortRelayRestart(ctx context.Context, id string, g uint64) error {
	w.once.Do(w.complete)
	return w.oldRelayWorker.AbortRelayRestart(ctx, id, g)
}

// Code review 2: an immediate end (a worker without the C contract) that
// finds its episode completed follows the stored outcome: the driver stops
// after one attempt, its proposal is cleaned up, and the call survives
// without an ended event. Proof for an earlier attempt cannot be promoted
// here: the driver's pending write or abort already moved the assignment.
func TestImmediateEndFollowsCompletedEpisode(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := watchEvents(t, c)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		e := armEpisode(t, p, c, 10*time.Second)
		require.Equal(t, e.proposal().Token, attemptOf(t, store, id).Attempt.Episode, "begin binds the attempt")
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		markLost(p, "r2") // the driver does not adopt; it tries another slot
		worker := &completingAbortWorker{oldRelayWorker: &oldRelayWorker{fakeWorker: a.fakeWorker, inner: a}}
		worker.complete = func() {
			at, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
			require.NoError(t, err)
			done := rec
			episode := *rec.Episode
			episode.Seq, episode.Outcome = episode.Seq+1, sessionstore.RelayEpisodeCompleted
			done.Episode = &episode
			require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, at, rec, done, time.Time{}, time.Time{}))
		}
		p.mu.Lock()
		p.workers["a"].worker = worker
		p.mu.Unlock()
		require.NoError(t, p.startRelayRecoveryFrom(context.Background(), c, false, time.Now(), nil))
		awaitDone(t, e)
		require.Eventually(t, func() bool {
			c.relayEpisodeMu.Lock()
			defer c.relayEpisodeMu.Unlock()
			return c.relayEpisode == nil
		}, 5*time.Second, 5*time.Millisecond, "the completed episode is cleaned up")
		require.Equal(t, sessionstore.RelayEpisodeCompleted, episodeOf(t, store, id).Outcome)
		records, err := store.(sessionstore.ControlStore).ControlIntents(context.Background())
		require.NoError(t, err)
		for _, r := range records {
			require.NotEqual(t, e.proposal().ID, r.ID, "the proposal is deleted")
		}
		time.Sleep(200 * time.Millisecond) // a driver that kept going would try again within 50 ms
		_, assigned, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 2}, assigned, "one aborted attempt, then the driver stopped")
		require.Empty(t, endReason(c))
		p.mu.Lock()
		terminal := c.terminal
		p.mu.Unlock()
		require.False(t, terminal, "the call survives")
		for drained := false; !drained; {
			select {
			case name := <-events:
				require.NotEqual(t, "published", name, "no ended event")
			default:
				drained = true
			}
		}
	})
}

// lostReplyJournal applies one journal Put and then reports an error, as a
// write whose reply was lost.
type lostReplyJournal struct {
	relayStore
	drop atomic.Bool
}

func (s *lostReplyJournal) PutControlIntent(ctx context.Context, intent sessionstore.ControlIntent) error {
	if err := s.relayStore.PutControlIntent(ctx, intent); err != nil {
		return err
	}
	if s.drop.CompareAndSwap(true, false) {
		return errors.New("store reply lost")
	}
	return nil
}

// Code review 2, P1: a replacement proposal whose Put landed but reported an
// error is retried with the same token, so the episode still arms and is
// decided at its deadline instead of minting tokens the journal refuses.
func TestUncertainReproposalWriteStillArms(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, backing sessionstore.Store) {
		store := &lostReplyJournal{relayStore: backing.(relayStore)}
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		markLost(p, "r1")
		events := watchEvents(t, c)
		c.stateMu.Lock()
		lease := c.lease
		c.stateMu.Unlock()
		stale := sessionstore.ControlIntent{ID: "relay-recovery:" + id, Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now().Add(time.Second - relayRecoveryLimit), Lease: lease, Seq: 7}
		require.NoError(t, store.PutControlIntent(context.Background(), stale))
		store.drop.Store(true) // the replacement's Put lands, its reply is lost
		e := p.installRelayEpisode(c, stale)
		awaitEvent(t, events, "reproposed")
		select {
		case <-e.armed:
		case <-time.After(5 * time.Second):
			t.Fatal("the episode never armed after an uncertain proposal write")
		}
		ep := episodeOf(t, store, id)
		require.True(t, ep.Open())
		require.Equal(t, e.proposal().Token, ep.Token)
		records, err := store.ControlIntents(context.Background())
		require.NoError(t, err)
		var journaled []string
		for _, r := range records {
			if r.ID == stale.ID {
				journaled = append(journaled, r.Token)
			}
		}
		require.Equal(t, []string{ep.Token}, journaled, "the journal holds the token that began")
		require.Eventually(t, func() bool { return endReason(c) == "relay_change_failed" }, 5*time.Second, 5*time.Millisecond, "the episode is decided at its deadline")
	})
}
