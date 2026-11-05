package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// oldRelayWorker supports restart but not the C contract: a worker from
// before the A+C amendment.
type oldRelayWorker struct {
	*fakeWorker
	inner *relayProtocolWorker
}

func (w *oldRelayWorker) ConfigureRelays(ctx context.Context, slots []mediaworker.RelaySlot) error {
	return w.inner.ConfigureRelays(ctx, slots)
}
func (w *oldRelayWorker) CreateSessionAtRelay(ctx context.Context, offer, slot string) (string, string, sessionstore.Lease, error) {
	return w.inner.CreateSessionAtRelay(ctx, offer, slot)
}
func (w *oldRelayWorker) AbortRelayRestart(ctx context.Context, id string, g uint64) error {
	return w.inner.AbortRelayRestart(ctx, id, g)
}
func (w *oldRelayWorker) RestartSession(ctx context.Context, id string, req mediaworker.RestartRequest) (string, error) {
	return w.inner.RestartSession(ctx, id, req)
}

func confirmations(store sessionstore.Store) sessionstore.RelayConfirmations {
	return store.(sessionstore.RelayConfirmations)
}

// commitPending plays the owning worker's switch commit for the pending slot.
func commitPending(t *testing.T, store sessionstore.Store, id string) sessionstore.RelayAssignment {
	t.Helper()
	lease, pending := waitRelayPending(t, store, id)
	committed := sessionstore.RelayAssignment{Slot: pending.Pending, Generation: pending.Generation + 1}
	require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, pending, committed))
	return committed
}

func attemptOf(t *testing.T, store sessionstore.Store, id string) sessionstore.RelayConfirmation {
	t.Helper()
	_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, rec.Attempt)
	return rec
}

func postProgress(t *testing.T, url, contentType, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, contentType, strings.NewReader(body))
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp
}

func TestRelayCommitAloneNeverCompletes(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		a.noDelivery.Store(true)
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		commitPending(t, store, id)
		select {
		case err := <-done:
			t.Fatalf("a switch commit alone completed the change: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		rec := attemptOf(t, store, id)
		require.Equal(t, sessionstore.RelayConfirmDelivery, rec.Mode)
		require.Equal(t, sessionstore.RelayPhaseSwitched, rec.Attempt.Phase)
		require.Equal(t, []string{"0"}, rec.Attempt.Required)
		require.False(t, rec.Attempt.CommitAt.IsZero())
		a.noDelivery.Store(false) // fresh receiver reports arrive
		require.NoError(t, <-done)
		rec = attemptOf(t, store, id)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
		require.Equal(t, []string{"0"}, rec.Attempt.Delivered)
	})
}

func TestMediaProgressConfirmsDecodeMode(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.acceptRelayMode(context.Background(), c, sessionstore.RelayConfirmDecode))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		url := server.URL + "/calls/" + id + "/media-progress"
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		waitRelayPending(t, store, id)
		require.Eventually(t, func() bool {
			_, rec, e := confirmations(store).RelayConfirmation(context.Background(), id)
			return e == nil && rec.Attempt != nil
		}, time.Second, time.Millisecond)

		// Refusals have no effect; the rate limit is one per second, burst 2.
		require.Equal(t, http.StatusBadRequest, postProgress(t, url, "text/plain", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		require.Equal(t, http.StatusConflict, postProgress(t, url, "application/json", `{"generation":2,"decoded_mids":["0"]}`).StatusCode, "a future generation is refused")
		time.Sleep(2 * time.Second)
		require.Equal(t, http.StatusConflict, postProgress(t, url, "application/json", `{"generation":1,"decoded_mids":["7"]}`).StatusCode, "a MID outside the required set is refused")
		require.Equal(t, http.StatusRequestEntityTooLarge, postProgress(t, url, "application/json", `{"generation":1,"decoded_mids":["0"],"pad":"`+strings.Repeat("x", 5000)+`"}`).StatusCode)
		resp := postProgress(t, url, "application/json", `{"generation":1,"decoded_mids":["0"]}`)
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		require.Equal(t, "1", resp.Header.Get("Retry-After"))
		require.Empty(t, attemptOf(t, store, id).Attempt.Decoded)

		// A full set saved before the switch is consumed by the commit.
		time.Sleep(1100 * time.Millisecond)
		require.Equal(t, http.StatusNoContent, postProgress(t, url, "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
		require.Equal(t, []string{"0"}, attemptOf(t, store, id).Attempt.Decoded)
		select {
		case err := <-done:
			t.Fatalf("A without the commit completed the change: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		commitPending(t, store, id)
		require.NoError(t, <-done)
		rec := attemptOf(t, store, id)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
		time.Sleep(1100 * time.Millisecond)
		require.Equal(t, http.StatusNoContent, postProgress(t, url, "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode, "a lost reply is retried idempotently")

		// A delivery-mode call never accepts A.
		p2, _, _, id2 := relayFailurePlane(t, sessionstore.NewMemory())
		server2 := httptest.NewServer(p2.Handler())
		defer server2.Close()
		require.Equal(t, http.StatusBadRequest, postProgress(t, server2.URL+"/calls/"+id2+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode)
	})
}

func TestDecodeModeCompleteDeliveryWithoutARollsBack(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.acceptRelayMode(context.Background(), c, sessionstore.RelayConfirmDecode))
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		commitPending(t, store, id)
		committed := time.Now()
		// C completes at once; A never arrives. The 3 s attempt bound fails it.
		lease, rollback := waitRelayPendingAt(t, store, id, 1)
		elapsed := time.Since(committed)
		require.GreaterOrEqual(t, elapsed, relayAttemptBound-100*time.Millisecond, "A attempt bound runs from the observed commit")
		require.Less(t, elapsed, relayAttemptBound+2*time.Second, "the attempt bound is enforced")
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1, Pending: "r1"}, rollback, "a planned failure rolls back to the original slot with a higher generation")
		var rec sessionstore.RelayConfirmation
		require.Eventually(t, func() bool {
			_, rec, err = confirmations(store).RelayConfirmation(context.Background(), id)
			return err == nil && rec.Attempt != nil && rec.Attempt.Generation == 2
		}, time.Second, time.Millisecond)
		require.Equal(t, "r1", rec.Attempt.Slot)
		require.Empty(t, rec.Attempt.Delivered, "evidence never moves to a higher generation")
		require.Equal(t, "delivered_decode_unconfirmed", relayFailureDetail(sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDecode, Attempt: &sessionstore.RelayAttempt{Generation: 1, Slot: "r2", Required: []string{"0"}, Delivered: []string{"0"}}}).Detail)
		// The rollback is confirmed under the same mode.
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, rollback, sessionstore.RelayAssignment{Slot: "r1", Generation: 2}))
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":2,"decoded_mids":["0"]}`).StatusCode)
		err = <-done
		require.ErrorIs(t, err, ErrRelayRolledBack)
		require.Equal(t, http.StatusConflict, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":1,"decoded_mids":["0"]}`).StatusCode, "an expired generation is never revived")
	})
}

func waitRelayPendingAt(t *testing.T, store sessionstore.Store, id string, generation uint64) (sessionstore.Lease, sessionstore.RelayAssignment) {
	t.Helper()
	var lease sessionstore.Lease
	var a sessionstore.RelayAssignment
	require.Eventually(t, func() bool {
		var err error
		lease, a, err = store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		return err == nil && a.Pending != "" && a.Generation == generation
	}, 5*time.Second, time.Millisecond)
	return lease, a
}

// ADR 0016 section 10: an owning worker without the C contract is never
// completed on commit. A planned change leaves the call; recovery ends it.
func TestOldWorkerWithoutConfirmationContract(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, _, id := relayFailurePlane(t, store)
		p.workers["a"].worker = &oldRelayWorker{fakeWorker: a.fakeWorker, inner: a}
		err := p.ChangeRelay(context.Background(), id, "r2")
		require.ErrorIs(t, err, ErrConfirmationUnsupported)
		_, got, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 1}, got, "the attempted generation is consumed and the call stays")
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.False(t, c.terminal)

		stopRelayRecoveries(t, p)
		require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason == "relay_change_failed" && c.relayEndDetail.Detail == "confirmation_unsupported"
		}, 2*time.Second, time.Millisecond)
	})
}

func TestRemoteWorkerWithoutDeliveryRoute(t *testing.T) {
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	_, err := (&RemoteWorker{URL: old.URL}).RelayDelivery(context.Background(), "id", 1)
	require.ErrorIs(t, err, mediaworker.ErrRelayUnsupported)
	unknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Error(w, mediaworker.ErrUnknownSession, mediaworker.RemoteErrors)
	}))
	defer unknown.Close()
	_, err = (&RemoteWorker{URL: unknown.URL}).RelayDelivery(context.Background(), "id", 1)
	require.False(t, errors.Is(err, mediaworker.ErrRelayUnsupported), "an unknown session is not an old worker")
}

func TestRelayModeAcceptance(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, _ := relayFailurePlane(t, store)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		for _, tc := range []struct {
			relay, progress string
			mode            string
		}{
			{"", "", ""},
			{"", "true", ""},
			{"true", "", sessionstore.RelayConfirmDelivery},
			{"true", "true", sessionstore.RelayConfirmDecode},
		} {
			req, err := http.NewRequest(http.MethodPost, server.URL+"/calls", strings.NewReader("offer"))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/sdp")
			if tc.relay != "" {
				req.Header.Set("Relais-Relay-Change", tc.relay)
			}
			if tc.progress != "" {
				req.Header.Set("Relais-Media-Progress", tc.progress)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			require.Equal(t, tc.mode != "", resp.Header.Get("Relais-Relay-Change") == "true")
			require.Equal(t, tc.mode == sessionstore.RelayConfirmDecode, resp.Header.Get("Relais-Media-Progress") == "true", "media_progress requires relay_change")
			id := strings.TrimPrefix(resp.Header.Get("Location"), "/calls/")
			_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
			require.NoError(t, err)
			require.Equal(t, tc.mode, rec.Mode, "the accepted mode is durable before the echo")
		}

		// A call without a saved mode never accepted relay change: none.
		q := New(&fakeRelay{}, sessionstore.NewMemory())
		c := &call{id: "never-accepted"}
		mode, err := q.relayMode(context.Background(), c)
		require.Error(t, err, "no session record")
		require.Empty(t, mode)
	})
}

func TestRelayFailureDetail(t *testing.T) {
	required := []string{"0", "1"}
	detail := relayFailureDetail(sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDecode, Attempt: &sessionstore.RelayAttempt{Generation: 3, Slot: "r2", Required: required, Delivered: required, Decoded: []string{"1"}}})
	require.Equal(t, RelayEvent{Generation: 3, Slot: "r2", Confirmation: "decode", Detail: "delivered_decode_unconfirmed", MissingMIDs: []string{"0"}}, detail)
	detail = relayFailureDetail(sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDecode, Attempt: &sessionstore.RelayAttempt{Generation: 3, Slot: "r2", Required: required, Delivered: []string{"0"}}})
	require.Equal(t, "delivery_unconfirmed", detail.Detail, "incomplete C names delivery, never decode")
	detail = relayFailureDetail(sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDelivery, Attempt: &sessionstore.RelayAttempt{Generation: 3, Slot: "r2", Required: required, Delivered: []string{"0"}}})
	require.Equal(t, RelayEvent{Generation: 3, Slot: "r2", Confirmation: "delivery", Detail: "delivery_unconfirmed", MissingMIDs: []string{"1"}}, detail)
}

// Row 6 (unplanned): the episode retries untried slots, never a frozen
// connected call, and ends with relay_change_failed by its fixed deadline.
func TestUnplannedEpisodeEndsWithDeliveryDetail(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.acceptRelayMode(context.Background(), c, sessionstore.RelayConfirmDecode))
		p.mu.Lock()
		binding := p.relaySet["r1"]
		binding.lost = true
		p.relaySet["r1"] = binding
		p.mu.Unlock()
		stop := make(chan struct{})
		defer close(stop)
		go func() { // the worker commits every attempt; C arrives, A never does
			for {
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
				}
				lease, a, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
				if err == nil && a.Pending != "" {
					_ = store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, a, sessionstore.RelayAssignment{Slot: a.Pending, Generation: a.Generation + 1})
				}
			}
		}()
		started := time.Now()
		require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
		c.relayEpisodeMu.Lock()
		deadline := c.relayEpisode.deadline
		c.relayEpisodeMu.Unlock()
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason == "relay_change_failed"
		}, relayRecoveryLimit+2*time.Second, 10*time.Millisecond)
		ended := time.Since(started)
		require.Less(t, ended, relayRecoveryLimit+time.Second)
		require.False(t, time.Now().Before(deadline.Add(-50*time.Millisecond)), "the end waits for the fixed episode deadline")
		c.stateMu.Lock()
		detail := c.relayEndDetail
		c.stateMu.Unlock()
		require.Equal(t, "delivered_decode_unconfirmed", detail.Detail)
		require.Equal(t, "r2", detail.Slot, "only the untried live slot was attempted")
		require.Equal(t, []string{"0"}, detail.MissingMIDs)
		t.Logf("ROW6_UNPLANNED ended_after=%s detail=%s generation=%d", ended.Round(time.Millisecond), detail.Detail, detail.Generation)
	})
}

// ADR 0016 section 10, rule 4: after a control restart, a committed attempt
// still awaiting confirmation is adopted with its saved bound and evidence.
// Saved full A plus the commit completes it; A is not requested again.
func TestControlRestartAdoptsUnconfirmedAttempt(t *testing.T) {
	for _, mode := range []string{sessionstore.RelayConfirmDelivery, sessionstore.RelayConfirmDecode} {
		t.Run(mode, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, _, _, id := relayFailurePlane(t, store, mode)
				stopRelayRecoveries(t, p)
				c, err := p.lookup(id)
				require.NoError(t, err)
				require.NoError(t, p.acceptRelayMode(context.Background(), c, mode))
				ctx := context.Background()
				lease, a, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
				require.NoError(t, err)
				pending := a
				pending.Pending = "r2"
				require.NoError(t, store.(sessionstore.RelayAssignments).CASRelay(ctx, lease, a, pending))
				committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
				require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(ctx, lease, pending, committed))
				now := time.Now()
				attempt := sessionstore.RelayAttempt{Generation: 1, Slot: "r2", Phase: sessionstore.RelayPhaseSwitched, Required: []string{"0"}, Kind: "planned", Tried: []string{"r2"},
					CommitAt: now, AttemptBound: now.Add(5 * time.Second), PlannedDeadline: now.Add(5 * time.Second)}
				if mode == sessionstore.RelayConfirmDecode {
					attempt.Decoded = []string{"0"} // a saved full set from before the restart
				}
				_, rec, err := confirmations(store).RelayConfirmation(ctx, id)
				require.NoError(t, err)
				require.NoError(t, confirmations(store).CASRelayConfirmation(ctx, id, committed, rec, sessionstore.RelayConfirmation{Mode: mode, Attempt: &attempt}, time.Time{}, time.Time{}))
				c.stateMu.Lock()
				c.relayMode = "" // a restarted control reloads the mode
				c.stateMu.Unlock()

				p.resumeRelayAttempts(ctx)
				require.Eventually(t, func() bool {
					_, rec, err := confirmations(store).RelayConfirmation(ctx, id)
					return err == nil && rec.Attempt.Generation == 1 && rec.Attempt.Phase == sessionstore.RelayPhaseConfirmed
				}, 3*time.Second, 5*time.Millisecond, "the same attempt completes; no new generation")
				_, got, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
				require.NoError(t, err)
				require.Equal(t, committed, got)
				c.stateMu.Lock()
				defer c.stateMu.Unlock()
				require.Empty(t, c.relayEndReason)
				require.Equal(t, mode, c.relayMode)
			})
		})
	}
}
