package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		require.Eventually(t, func() bool { return endReason(c) == "relay_change_failed" }, 5*time.Second, 10*time.Millisecond)
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
				records, err := store.(sessionstore.ControlStore).ControlIntents(context.Background())
				require.NoError(t, err)
				for _, r := range records {
					require.NotEqual(t, stale.ID, r.ID)
				}
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
		require.NoError(t, p.confirmRelaySuccess(context.Background(), id, committed, rec)) // an earlier confirmation
		markLost(p, "r2")
		e := armEpisode(t, p, c, 10*time.Second)
		require.Empty(t, attemptOf(t, store, id).Attempt.Episode, "a historical confirmation is never adopted")
		c.mu.Lock() // keep the session while the record is inspected
		defer c.mu.Unlock()
		require.Equal(t, relayEnded, p.endRelayEpisode(context.Background(), c, e.proposal().Token, "relay_change_failed", false))
		c.stateMu.Lock()
		c.relayEndReason = "" // isolate the store check from the in-memory one
		c.stateMu.Unlock()
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		require.Equal(t, http.StatusGone, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
	})
}

func TestReadBackComparesWholeEpisode(t *testing.T) {
	ep := sessionstore.RelayEpisode{Seq: 2, Token: "t", StoreDeadline: time.Now(), Outcome: sessionstore.RelayEpisodeEnded, Detail: "delivery_unconfirmed"}
	other := ep
	other.Detail = "delivered_decode_unconfirmed"
	a := sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDecode, Episode: &ep}
	b := sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDecode, Episode: &other}
	require.True(t, sameRelayRecord(a, a))
	require.False(t, sameRelayRecord(a, b), "an episode-only difference is a different record")
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
