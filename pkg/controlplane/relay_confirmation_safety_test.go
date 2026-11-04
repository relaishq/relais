package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Regression tests for the slice-4 review findings (ADR 0016 sections 5, 7
// and 10). Each one fails on the code before its fix.

func markLost(p *Plane, slots ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, slot := range slots {
		b := p.relaySet[slot]
		b.lost = true
		p.relaySet[slot] = b
	}
}

// seedAttempt writes a pending attempt for generation 1 on target and returns
// the pending assignment.
func seedAttempt(t *testing.T, p *Plane, store sessionstore.Store, id, target, mode string, attempt sessionstore.RelayAttempt) (sessionstore.Lease, sessionstore.RelayAssignment) {
	t.Helper()
	ctx := context.Background()
	c, err := p.lookup(id)
	require.NoError(t, err)
	require.NoError(t, p.acceptRelayMode(ctx, c, mode))
	lease, a, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
	require.NoError(t, err)
	pending := a
	pending.Pending = target
	require.NoError(t, store.(sessionstore.RelayAssignments).CASRelay(ctx, lease, a, pending))
	_, rec, err := confirmations(store).RelayConfirmation(ctx, id)
	require.NoError(t, err)
	attempt.Generation, attempt.Slot, attempt.Phase = 1, target, sessionstore.RelayPhasePending
	require.NoError(t, confirmations(store).CASRelayConfirmation(ctx, id, pending, rec, sessionstore.RelayConfirmation{Mode: mode, Attempt: &attempt}, time.Time{}))
	return lease, pending
}

// Finding 1: an earlier episode's confirmation never completes a later one.
// A confirmed call loses its relay and no slot is eligible: the new episode
// must end the call at its own deadline.
func TestEarlierConfirmationDoesNotCompleteLaterEpisode(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		commitPending(t, store, id)
		require.NoError(t, <-done)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, attemptOf(t, store, id).Attempt.Phase)

		markLost(p, "r1", "r2")
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason == "relay_change_failed"
		}, relayRecoveryLimit+2*time.Second, 20*time.Millisecond, "a stalled call must not stay alive on an old confirmation")
	})
}

// Finding 2: R2 switched but unconfirmed, then the rollback to R1 fails before
// switching. The call is not on its original working path, so it must enter
// recovery under the first planned deadline.
func TestFailedRollbackAfterUnconfirmedSwitchRecovers(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.acceptRelayMode(context.Background(), c, sessionstore.RelayConfirmDecode))
		started := time.Now()
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		commitPending(t, store, id) // no A follows: unconfirmed after 3 s
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			if c.relayEvent == nil || c.relayEvent.Generation != 2 {
				return false
			}
			c.relayRejected = true // the rollback offer is refused before switching
			return true
		}, 6*time.Second, time.Millisecond)
		require.Error(t, <-done)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.NotNil(t, episode, "a failed rollback after an unconfirmed switch must enter recovery")
		require.False(t, episode.deadline.After(started.Add(relayPlannedLimit+relayRecoveryLimit+100*time.Millisecond)), "the episode keeps the 10 s + 15 s bound")
	})
}

// Finding 3: control acknowledges full A, the worker commits, and control
// stops before it observes the commit. After restart the same attempt
// completes from saved A plus the recorded commit; it is not superseded.
func TestRestartCompletesFullAThenCommit(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", Tried: []string{"r2"},
			PlannedDeadline: now.Add(5 * time.Second), StoreDeadline: now.Add(5 * time.Second)})
		committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, committed))
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayMode = ""
		c.stateMu.Unlock()
		require.Equal(t, sessionstore.RelayPhasePending, attemptOf(t, store, id).Attempt.Phase, "control never observed the commit")

		p.resumeRelayAttempts(context.Background())
		require.Eventually(t, func() bool {
			rec := attemptOf(t, store, id)
			return rec.Attempt.Generation == 1 && rec.Attempt.Phase == sessionstore.RelayPhaseConfirmed
		}, 3*time.Second, 5*time.Millisecond)
		_, got, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, committed, got, "no new generation")
	})
}

// Finding 4: a retired (aborted) generation never accepts A and never
// confirms, even when it retires onto the same slot.
func TestAbortedGenerationRefusesA(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r1", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Kind: "recovery", EpisodeDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		require.NoError(t, store.(sessionstore.RelayAssignments).CASRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r1", Generation: 1}))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		require.Equal(t, http.StatusConflict, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		rec := attemptOf(t, store, id)
		require.Empty(t, rec.Attempt.Decoded)
		require.True(t, rec.Aborted())
	})
}

// Finding 5: late evidence never completes an attempt. A commit that lands
// after the deadline does not confirm saved A, and delayed C is refused.
func TestLateEvidenceNeverCompletes(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", Tried: []string{"r2"},
			PlannedDeadline: now.Add(30 * time.Millisecond), StoreDeadline: now.Add(30 * time.Millisecond)})
		time.Sleep(80 * time.Millisecond)
		committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, committed))
		a2, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		require.False(t, rec.Confirmed(a2), "the commit arrived last, after the deadline")

		// Delayed C for a switched attempt whose bound has passed is refused.
		c, err := p.lookup(id)
		require.NoError(t, err)
		switched := *rec.Attempt
		switched.Decoded = nil
		switched.Phase, switched.CommitAt, switched.AttemptBound = sessionstore.RelayPhaseSwitched, now, now.Add(50*time.Millisecond)
		switched.StoreDeadline = time.Time{}
		require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, a2, rec, sessionstore.RelayConfirmation{Mode: rec.Mode, Attempt: &switched, Commit: rec.Commit}, time.Time{}))
		a2, rec, err = confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		p.retainDelivery(context.Background(), c, a, lease, a2, rec, sessionstore.RelayConfirmDecode)
		require.Empty(t, attemptOf(t, store, id).Attempt.Delivered, "C after the attempt bound is never retained")

		// Delayed A after the bound is refused too.
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		require.Equal(t, http.StatusConflict, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
	})
}

type flakyConfirmations struct {
	sessionstore.Store
	sessionstore.RelayConfirmations
	sessionstore.ControlStore
	fail atomic.Bool
}

func (s *flakyConfirmations) RelayConfirmation(ctx context.Context, id string) (sessionstore.RelayAssignment, sessionstore.RelayConfirmation, error) {
	if s.fail.Load() {
		return sessionstore.RelayAssignment{}, sessionstore.RelayConfirmation{}, sessionstore.ErrTransient
	}
	return s.RelayConfirmations.RelayConfirmation(ctx, id)
}

// Finding 6: unavailable store state is uncertain, never expiry: the
// watchdog does not end the call until it can decide.
func TestStoreUncertaintyNeverEndsCall(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		flaky := &flakyConfirmations{Store: store, RelayConfirmations: store.(sessionstore.RelayConfirmations), ControlStore: store.(sessionstore.ControlStore)}
		flaky.fail.Store(true)
		p.store = flaky
		outcome, _ := p.expireRelayAttempt(context.Background(), c, time.Now())
		require.Equal(t, relayExpiryUncertain, outcome)

		_, err = p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+50*time.Millisecond))
		require.NoError(t, err)
		time.Sleep(400 * time.Millisecond)
		c.stateMu.Lock()
		reason := c.relayEndReason
		c.stateMu.Unlock()
		require.Empty(t, reason, "an unreadable store must not end the call")
		flaky.fail.Store(false) // the store answers: no confirmed attempt, so the call ends
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason == "relay_change_failed"
		}, 3*time.Second, 10*time.Millisecond)
	})
}

// Finding 8: A is retained only under mutation authority.
func TestMediaProgressNeedsAuthority(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		now := time.Now()
		seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		guard := *p.restart.guard.Load()
		lapsed := guard
		lapsed.deadline = time.Now().Add(-time.Millisecond)
		p.restart.guard.Store(&lapsed)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		url := server.URL + "/calls/" + id + "/media-progress"
		require.Equal(t, http.StatusServiceUnavailable, postProgress(t, url, "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		require.Empty(t, attemptOf(t, store, id).Attempt.Decoded, "nothing is retained without authority")
		p.restart.guard.Store(&guard)
		require.Equal(t, http.StatusNoContent, postProgress(t, url, "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
	})
}

// Finding 9: create accepts A only when the selected worker has C, and a
// topology without a relay set accepts nothing and writes nothing.
func TestCapabilityAcceptanceChecksWorkerAndTopology(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, _ := relayFailurePlane(t, store)
		p.workers["a"].worker = &oldRelayWorker{fakeWorker: a.fakeWorker, inner: a}
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		create := func(url string, progress bool) *http.Response {
			req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("offer"))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/sdp")
			req.Header.Set("Relais-Relay-Change", "true")
			if progress {
				req.Header.Set("Relais-Media-Progress", "true")
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			require.NoError(t, resp.Body.Close())
			if resp.StatusCode == http.StatusConflict {
				var failure struct{ Code string }
				require.NoError(t, json.Unmarshal(body, &failure))
				require.Equal(t, "relay_unsupported", failure.Code)
			}
			return resp
		}
		resp := create(server.URL+"/calls?worker=a", true)
		require.Equal(t, http.StatusConflict, resp.StatusCode, "a new control with an old worker refuses A at create")
		resp = create(server.URL+"/calls?worker=a", false)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "C-only calls are accepted; a change then reports confirmation_unsupported")
		require.Equal(t, "true", resp.Header.Get("Relais-Relay-Change"))

		// Default profile: no relay set. Nothing is echoed or written, so the
		// default writer stays on store contract 1 and rollback preflight holds.
		target := sessionstore.NewMemory()
		q := New(&fakeRelay{}, target)
		w := &relayProtocolWorker{fakeWorker: &fakeWorker{store: target, addr: netip.MustParseAddrPort("127.0.0.1:17011"), running: map[string]bool{}}}
		require.NoError(t, q.Register("a", w.addr, w))
		server2 := httptest.NewServer(q.Handler())
		defer server2.Close()
		resp = create(server2.URL+"/calls", true)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		require.Empty(t, resp.Header.Get("Relais-Relay-Change"))
		require.Empty(t, resp.Header.Get("Relais-Media-Progress"))
		id := strings.TrimPrefix(resp.Header.Get("Location"), "/calls/")
		_, rec, err := confirmations(target).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayConfirmation{}, rec, "no contract-3 field is written by a default profile")
	})
}

func waitUntil(start time.Time, offset time.Duration) { time.Sleep(time.Until(start.Add(offset))) }

// Adoption never extends an attempt (scaled 8/9/10/11 s case): the planned
// deadline is D, the worker commits before D, control restarts before it
// observes the commit, and A arrives after D. A must be refused.
func TestAdoptionNeverExtendsPlannedBound(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		start := time.Now()
		deadline := start.Add(time.Second)
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Kind: "planned", Tried: []string{"r2"}, PlannedDeadline: deadline, StoreDeadline: deadline})
		waitUntil(start, 800*time.Millisecond)
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		waitUntil(start, 900*time.Millisecond)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayMode = ""
		c.stateMu.Unlock()
		p.resumeRelayAttempts(context.Background())
		waitUntil(start, 1100*time.Millisecond)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		require.Equal(t, http.StatusConflict, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode, "A after the planned deadline is refused")
		rec := attemptOf(t, store, id)
		require.False(t, rec.Attempt.Generation == 1 && rec.Attempt.Phase == sessionstore.RelayPhaseConfirmed)
	})
}

// Evidence writes use the saved store-clock bound, never a reconversion of a
// local deadline written by another control (here 10 s ahead).
func TestEvidenceUsesSavedStoreBound(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		now := time.Now()
		skewed := now.Add(10 * time.Second) // the previous control's local clock
		storeBound := now.Add(50 * time.Millisecond)
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Kind: "planned", PlannedDeadline: skewed, StoreDeadline: storeBound})
		committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, committed))
		_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		switched := *rec.Attempt
		switched.Phase, switched.CommitAt, switched.AttemptBound, switched.StoreBound = sessionstore.RelayPhaseSwitched, now, skewed, storeBound
		require.NoError(t, confirmations(store).CASRelayConfirmation(context.Background(), id, committed, rec, sessionstore.RelayConfirmation{Mode: rec.Mode, Attempt: &switched, Commit: rec.Commit}, time.Time{}))
		time.Sleep(100 * time.Millisecond)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		require.Equal(t, http.StatusConflict, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		require.Empty(t, attemptOf(t, store, id).Attempt.Decoded, "the saved store bound is authoritative")
	})
}

// A rollback whose worker switched but whose commit never becomes visible
// cannot hold the call past the first planned deadline + 15 s.
func TestSwitchedRollbackEndsWithinPlannedBound(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.acceptRelayMode(context.Background(), c, sessionstore.RelayConfirmDecode))
		start := time.Now()
		go func() { _ = p.ChangeRelay(context.Background(), id, "r2") }()
		commitPending(t, store, id) // unconfirmed after 3 s; rollback follows
		waitRelayPendingAt(t, store, id, 1)
		a.abortErr = mediaworker.ErrRelaySwitched // the worker switched; its commit never lands
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason == "relay_change_failed"
		}, relayPlannedLimit+relayRecoveryLimit+2*time.Second, 20*time.Millisecond)
		require.Less(t, time.Since(start), relayPlannedLimit+relayRecoveryLimit+1500*time.Millisecond)
	})
}

// Authority lost between an uncertain read and the definitive retry: the
// stale control must not end the call.
func TestWatchdogRechecksAuthorityBeforeEnding(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		flaky := &flakyConfirmations{Store: store, RelayConfirmations: store.(sessionstore.RelayConfirmations), ControlStore: store.(sessionstore.ControlStore)}
		flaky.fail.Store(true)
		p.store = flaky
		_, err = p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+50*time.Millisecond))
		require.NoError(t, err)
		time.Sleep(300 * time.Millisecond) // uncertain retries under authority
		guard := *p.restart.guard.Load()
		lapsed := guard
		lapsed.deadline = time.Now().Add(-time.Millisecond)
		p.restart.guard.Store(&lapsed)
		flaky.fail.Store(false)
		time.Sleep(time.Second)
		c.stateMu.Lock()
		reason := c.relayEndReason
		c.stateMu.Unlock()
		require.Empty(t, reason, "a control without authority must not end the call")
		require.False(t, c.terminal)
		guard.deadline = time.Now().Add(ControlLeaseTTL)
		p.restart.guard.Store(&guard)
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason == "relay_change_failed"
		}, 3*time.Second, 10*time.Millisecond)
	})
}

// A hangup while the watchdog retries an unreadable store stops the watcher.
func TestHangupStopsUncertainWatchdog(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		flaky := &flakyConfirmations{Store: store, RelayConfirmations: store.(sessionstore.RelayConfirmations), ControlStore: store.(sessionstore.ControlStore)}
		flaky.fail.Store(true)
		p.store = flaky
		e, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+50*time.Millisecond))
		require.NoError(t, err)
		time.Sleep(300 * time.Millisecond)
		require.NoError(t, p.End(context.Background(), id))
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			t.Fatal("the watchdog kept retrying for an ended call")
		}
		c.stateMu.Lock()
		defer c.stateMu.Unlock()
		require.Empty(t, c.relayEndReason, "a hangup is a terminal end, not a relay failure")
	})
}

// A definitively removed session is gone, not uncertain.
func TestExpiryTreatsRemovedSessionAsGone(t *testing.T) {
	store := sessionstore.NewMemory()
	p, _, _, id := relayFailurePlane(t, store)
	c, err := p.lookup(id)
	require.NoError(t, err)
	lease, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.NoError(t, store.Release(context.Background(), lease))
	outcome, _ := p.expireRelayAttempt(context.Background(), c, time.Now())
	require.Equal(t, relayExpiryGone, outcome)
}

// An explicitly configured one-slot set accepts A and recovers on the same
// slot (ADR 0016 sections 5 and 10).
func TestOneSlotSetAcceptsAAndRecoversSameSlot(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p := New(&fakeRelay{}, store)
		w := &relayProtocolWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:17021"), running: map[string]bool{}}}
		require.NoError(t, p.Register("a", w.addr, w))
		addr := netip.MustParseAddrPort("127.0.0.1:18021")
		require.NoError(t, p.ConfigureRelaySet(context.Background(), []RelayBinding{{Slot: mediaworker.RelaySlot{ID: "r1", Leg: addr, Public: addr}, Relay: &fakeRelay{}}}))
		stopRelayRecoveries(t, p)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/calls", strings.NewReader("offer"))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/sdp")
		req.Header.Set("Relais-Relay-Change", "true")
		req.Header.Set("Relais-Media-Progress", "true")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		require.Equal(t, "true", resp.Header.Get("Relais-Relay-Change"))
		require.Equal(t, "true", resp.Header.Get("Relais-Media-Progress"))
		id := strings.TrimPrefix(resp.Header.Get("Location"), "/calls/")
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := make(chan error, 1)
		go func() { _, err := p.callerRelayRestart(context.Background(), c); events <- err }()
		_, pending := waitRelayPending(t, store, id)
		require.Equal(t, "r1", pending.Pending, "a same-slot restart")
		require.Eventually(t, func() bool {
			_, rec, e := confirmations(store).RelayConfirmation(context.Background(), id)
			return e == nil && rec.Attempt != nil
		}, time.Second, time.Millisecond)
		commitPending(t, store, id)
		require.NoError(t, <-events)
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		require.Eventually(t, func() bool {
			rec := attemptOf(t, store, id)
			return rec.Attempt.Phase == sessionstore.RelayPhaseConfirmed && rec.Attempt.Slot == "r1"
		}, 3*time.Second, 5*time.Millisecond)
	})
}

// watchEvents delivers this call's watcher transitions to the test.
func watchEvents(t *testing.T, c *call) <-chan string {
	t.Helper()
	events := make(chan string, 16)
	hook := func(from *call, name string) {
		if from == c {
			events <- name
		}
	}
	relayWatchHooks.Store(&hook)
	t.Cleanup(func() { relayWatchHooks.Store(nil) })
	return events
}

// awaitEvent waits for the watcher to reach want, in order; earlier
// transitions are passed over.
func awaitEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-events:
			if got == want {
				return
			}
		case <-timeout:
			t.Fatalf("watcher never reached %q", want)
		}
	}
}

// Losing authority while the watcher waits for the call lock never leaves the
// episode without a watcher: once authority returns the call really ends and
// the episode completes.
func TestWatcherSurvivesAuthorityLossUnderCallLock(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		authorizeTestControl(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := watchEvents(t, c)
		c.mu.Lock() // an operation still holds the call
		e, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+50*time.Millisecond))
		require.NoError(t, err)
		awaitEvent(t, events, "deciding")
		awaitEvent(t, events, "awaiting-call-lock") // barrier: the watcher decided and waits for c.mu
		guard := *p.restart.guard.Load()
		lapsed := guard
		lapsed.deadline = time.Now().Add(-time.Millisecond)
		p.restart.guard.Store(&lapsed)
		c.mu.Unlock()
		awaitEvent(t, events, "suspended")
		require.False(t, c.terminal)
		guard.deadline = time.Now().Add(ControlLeaseTTL)
		p.restart.guard.Store(&guard)
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the episode never completed after authority returned")
		}
		_, err = store.Get(context.Background(), id)
		require.Error(t, err, "the session really ended")
		p.mu.Lock()
		defer p.mu.Unlock()
		require.True(t, c.terminal)
	})
}

// Confirmation wins before expiry while the driver's completion is delayed:
// the armed episode binds the existing attempt, so expiry sees its own
// confirmation; the call stays up and no failure is published.
func TestArmedEpisodeBindsExistingAttempt(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		e, err := p.relayRecoveryEpisode(context.Background(), c, now.Add(-relayRecoveryLimit+time.Second))
		require.NoError(t, err)
		// The driver arms and re-enters its wait; nothing completes the episode.
		go func() {
			_ = p.waitRelayChange(context.Background(), c, pending, time.Minute, relayAttemptOptions{kind: "planned", outer: now.Add(10 * time.Second), episode: e.deadline})
		}()
		require.Eventually(t, func() bool { // barrier: the binding is durable
			return attemptOf(t, store, id).Attempt.EpisodeDeadline.Equal(e.deadline)
		}, 2*time.Second, time.Millisecond)
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		select { // the watchdog itself completes the episode at its deadline
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Fatal("episode not completed")
		}
		c.stateMu.Lock()
		reason := c.relayEndReason
		c.stateMu.Unlock()
		require.Empty(t, reason, "no relay_change_failed after a durable confirmation")
		_, err = store.Get(context.Background(), id)
		require.NoError(t, err, "the call stays up")
	})
}

// A driver that completes the episode just before the watchdog's terminal
// decision wins: nothing is published on the event stream the caller reads,
// and the call stays up.
func TestCompletionBeforeDecisionPublishesNothing(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := make(chan string, 16)
		hook := func(from *call, name string) {
			if from != c {
				return
			}
			if name == "deciding" {
				c.relayEpisodeMu.Lock()
				e := c.relayEpisode
				c.relayEpisodeMu.Unlock()
				_ = p.completeRelayEpisode(context.Background(), c, e) // the slow driver completes now; asserted below
			}
			events <- name
		}
		relayWatchHooks.Store(&hook)
		t.Cleanup(func() { relayWatchHooks.Store(nil) })
		_, err = p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+100*time.Millisecond))
		require.NoError(t, err)
		awaitEvent(t, events, "deciding")
		awaitEvent(t, events, "completed-before-decision")
		c.stateMu.Lock()
		reason := c.relayEndReason
		c.stateMu.Unlock()
		require.Empty(t, reason, "no terminal event once a driver completed")
		_, err = store.Get(context.Background(), id)
		require.NoError(t, err, "the call is not ended")
		requireNoEndedEvent(t, p, id)
	})
}

// After the terminal decision no completion is recorded: the caller was told
// ended, and the call really ends, even if a driver finishes meanwhile.
func TestCompletionAfterDecisionIsRefused(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := watchEvents(t, c)
		c.mu.Lock()
		e, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+50*time.Millisecond))
		require.NoError(t, err)
		awaitEvent(t, events, "deciding")
		awaitEvent(t, events, "awaiting-call-lock")
		require.ErrorIs(t, p.completeRelayEpisode(context.Background(), c, e), errRelayEpisodeEnded, "the slow driver finishes after the decision")
		awaitEvent(t, events, "completion-refused")
		require.False(t, e.completed.Load())
		c.stateMu.Lock()
		require.Equal(t, "relay_change_failed", c.relayEndReason, "ended was published at the deadline")
		c.stateMu.Unlock()
		c.mu.Unlock()
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Fatal("episode not finished")
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		require.True(t, c.terminal, "a call told ended really ends")
	})
}

// requireNoEndedEvent opens the call's event stream, as the caller does, and
// checks it stays open without an ended event over several polling cycles.
func requireNoEndedEvent(t *testing.T, p *Plane, id string) {
	t.Helper()
	c, err := p.lookup(id)
	require.NoError(t, err)
	c.stateMu.Lock()
	c.relayCapable = true
	c.stateMu.Unlock()
	server := httptest.NewServer(p.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/calls/"+id+"/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	data, _ := io.ReadAll(resp.Body) // ends at the context deadline unless the stream closes
	require.NotContains(t, string(data), "event: ended", "the caller must not be told the call ended")
	require.Error(t, ctx.Err(), "the stream stayed open for the whole observation")
}

// Binding a record with no store-clock fields keeps its own deadline: the
// store values come from the original Outer and Bound, not the episode.
func TestBindingLegacyRecordKeepsOriginalDeadline(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		start := time.Now()
		deadline := start.Add(300 * time.Millisecond)
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: deadline}) // no store fields
		a, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		episode := start.Add(5 * time.Second)
		bound, err := p.bindRelayAttempt(context.Background(), id, a, rec, episode)
		require.NoError(t, err)
		require.Equal(t, episode, bound.EpisodeDeadline)
		require.False(t, bound.StoreDeadline.After(deadline.Add(50*time.Millisecond)), "the store deadline comes from the original planned deadline")
		waitUntil(start, 400*time.Millisecond)
		committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, committed))
		a, rec, err = confirmations(store).RelayConfirmation(context.Background(), id)
		require.NoError(t, err)
		require.False(t, rec.Confirmed(a), "saved A plus a commit after the original deadline never confirms")
	})
}

// A hangup while authority stays absent closes a suspended watcher.
func TestSuspendedWatcherObservesHangup(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		authorizeTestControl(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		events := watchEvents(t, c)
		c.mu.Lock()
		e, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+50*time.Millisecond))
		require.NoError(t, err)
		awaitEvent(t, events, "deciding")
		awaitEvent(t, events, "awaiting-call-lock")
		lapsed := *p.restart.guard.Load()
		lapsed.deadline = time.Now().Add(-time.Millisecond)
		p.restart.guard.Store(&lapsed)
		c.mu.Unlock()
		awaitEvent(t, events, "suspended")
		c.hungUp.Store(true) // a hangup admitted before authority lapsed completes
		awaitEvent(t, events, "gone-while-suspended")
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the suspended watcher was retained after the hangup")
		}
	})
}

// After the terminal decision the real success paths are fenced: a delayed
// driver cannot save `confirmed` or report success (its wait returns the
// episode-ended error), A is not acknowledged, and ended is published.
func TestEndDecisionFencesDriverConfirmation(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		// Full A was acknowledged and the worker committed, but the attempt
		// is not bound to the episode: expiry treats it as not its own.
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		events := watchEvents(t, c)
		c.mu.Lock() // a busy operation holds the call; publication must not wait for it
		e, err := p.relayRecoveryEpisode(context.Background(), c, now.Add(-relayRecoveryLimit+100*time.Millisecond))
		require.NoError(t, err)
		awaitEvent(t, events, "deciding")
		awaitEvent(t, events, "awaiting-call-lock") // the decision is made and ended published
		c.stateMu.Lock()
		require.Equal(t, "relay_change_failed", c.relayEndReason)
		c.stateMu.Unlock()

		// The delayed driver now observes the durable confirmation.
		err = p.waitRelayChange(context.Background(), c, pending, time.Second, relayAttemptOptions{kind: "planned", outer: now.Add(10 * time.Second)})
		require.ErrorIs(t, err, errRelayEpisodeEnded, "no success after the terminal decision")
		require.NotEqual(t, sessionstore.RelayPhaseConfirmed, attemptOf(t, store, id).Attempt.Phase, "no confirmed record is saved")
		require.Equal(t, http.StatusGone, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode, "A is not acknowledged")
		require.ErrorIs(t, p.completeRelayEpisode(context.Background(), c, e), errRelayEpisodeEnded, "completion is refused, not swallowed")
		c.mu.Unlock()
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Fatal("episode not finished")
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		require.True(t, c.terminal, "the call told ended really ends")
	})
}

// ChangeRelay reports the ended episode, not a kept call, when the episode's
// terminal decision lands between a confirmed rollback and its completion:
// the completion refusal reaches the caller instead of being swallowed.
func TestChangeRelayReportsEndedEpisode(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.acceptRelayMode(context.Background(), c, sessionstore.RelayConfirmDecode))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		hook := func(from *call, name string) {
			if from == c && name == "rollback-confirmed" {
				c.relayEpisodeMu.Lock()
				e := c.relayEpisode
				c.relayEpisodeMu.Unlock()
				require.True(t, e.decideEnd(), "the watchdog decides first")
			}
		}
		relayWatchHooks.Store(&hook)
		t.Cleanup(func() { relayWatchHooks.Store(nil) })
		result := make(chan error, 1)
		go func() { result <- p.ChangeRelay(context.Background(), id, "r2") }()
		commitPending(t, store, id) // no A: unconfirmed after 3 s, then a rollback in an armed episode
		lease, rollback := waitRelayPendingAt(t, store, id, 1)
		require.Eventually(t, func() bool {
			rec := attemptOf(t, store, id)
			return rec.Attempt.Generation == 2
		}, 2*time.Second, time.Millisecond)
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, rollback, sessionstore.RelayAssignment{Slot: "r1", Generation: 2}))
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":2,"decoded_mids":["0"]}`).StatusCode)
		select {
		case err := <-result:
			require.ErrorIs(t, err, errRelayEpisodeEnded, "ChangeRelay must report the ended episode")
		case <-time.After(10 * time.Second):
			t.Fatal("ChangeRelay did not return")
		}
	})
}

// A reported in the window after the terminal decision but before ended is
// published is refused by the decision itself, not only by the published end.
func TestEndDecisionFencesAcknowledgement(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		c, err := p.lookup(id)
		require.NoError(t, err)
		now := time.Now()
		// Full A saved and committed, unbound: the decision is not fenced by a
		// durable expiry here, only by the decision itself.
		lease, pending := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDecode, sessionstore.RelayAttempt{
			Required: []string{"0"}, Decoded: []string{"0"}, Kind: "planned", PlannedDeadline: now.Add(10 * time.Second), StoreDeadline: now.Add(10 * time.Second)})
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		status := make(chan int, 1)
		hook := func(from *call, name string) {
			if from == c && name == "decided" { // decided, ended not yet published
				status <- postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode
			}
		}
		relayWatchHooks.Store(&hook)
		t.Cleanup(func() { relayWatchHooks.Store(nil) })
		c.mu.Lock() // keep the session until the record is inspected
		_, err = p.relayRecoveryEpisode(context.Background(), c, now.Add(-relayRecoveryLimit+100*time.Millisecond))
		require.NoError(t, err)
		select {
		case got := <-status:
			require.Equal(t, http.StatusGone, got, "A is not acknowledged after the decision")
		case <-time.After(5 * time.Second):
			t.Fatal("watchdog never decided")
		}
		rec := attemptOf(t, store, id)
		require.NotEqual(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase, "no confirmed record is saved")
		c.mu.Unlock()
	})
}
