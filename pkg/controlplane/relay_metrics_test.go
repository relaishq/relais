package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// relayMetric reads one series from the plane's private /metrics: a counter
// value, or a histogram's sample count. A missing series reads as -1.
func relayMetric(t *testing.T, p *Plane, name string, labels map[string]string) float64 {
	t.Helper()
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(planeMetricsBody(p)))
	require.NoError(t, err)
	family := families[name]
	if family == nil {
		return -1
	}
	for _, m := range family.Metric {
		if labelsMatch(m, labels) {
			if m.Histogram != nil {
				return float64(m.Histogram.GetSampleCount())
			}
			return m.Counter.GetValue()
		}
	}
	return -1
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	if len(m.Label) != len(want) {
		return false
	}
	for _, l := range m.Label {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

// relayEvents reads every relay change event counter for a slot.
func relayEvents(t *testing.T, p *Plane, slot string) map[string]float64 {
	t.Helper()
	got := map[string]float64{}
	for _, event := range relayChangeEvents {
		got[event] = relayMetric(t, p, "relais_control_relay_changes_total", map[string]string{"slot": slot, "event": event})
	}
	return got
}

func relayEventCounts(started, switched, delivered, decoded, confirmed, rolledBack, failed float64) map[string]float64 {
	return map[string]float64{relayStarted: started, relaySwitched: switched, relayDelivered: delivered, relayDecodeConfirmed: decoded,
		relayConfirmed: confirmed, relayRolledBack: rolledBack, relayFailed: failed}
}

// waitAttempt waits until control saved the attempt for generation, so a
// test commit or A report never races its creation.
func waitAttempt(t *testing.T, store sessionstore.Store, id string, generation uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, rec, err := confirmations(store).RelayConfirmation(context.Background(), id)
		return err == nil && rec.Attempt != nil && rec.Attempt.Generation == generation
	}, 5*time.Second, time.Millisecond)
}

func switchTimes(t *testing.T, p *Plane, slot string) float64 {
	return relayMetric(t, p, "relais_control_relay_switch_seconds", map[string]string{"slot": slot})
}

func confirmationTimes(t *testing.T, p *Plane, slot, mode string) float64 {
	return relayMetric(t, p, "relais_control_relay_confirmation_seconds", map[string]string{"slot": slot, "confirmation": mode})
}

// The recorded events follow the durable transitions: only the write that
// makes a transition records it, so writes that find it already saved add
// nothing.
func TestRecordRelayWriteTransitions(t *testing.T) {
	committed := sessionstore.RelayAssignment{Slot: "r2", Generation: 1}
	attempt := func(phase string, delivered, decoded []string) *sessionstore.RelayAttempt {
		return &sessionstore.RelayAttempt{Generation: 1, Slot: "r2", Phase: phase, Kind: "planned", Required: []string{"0", "1"}, Delivered: delivered, Decoded: decoded, Episode: "e"}
	}
	rec := func(a *sessionstore.RelayAttempt, outcome string) sessionstore.RelayConfirmation {
		r := sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDecode, Attempt: a, Commit: sessionstore.RelayCommit{Switched: 1}}
		if outcome != "" {
			r.Episode = &sessionstore.RelayEpisode{Seq: 1, Token: "e", Outcome: outcome}
		}
		return r
	}
	full := []string{"0", "1"}
	for _, tc := range []struct {
		name     string
		from, to sessionstore.RelayConfirmation
		want     map[string]float64
	}{
		{"new attempt", rec(nil, ""), rec(attempt("pending", nil, nil), ""), relayEventCounts(1, 0, 0, 0, 0, 0, 0)},
		{"supersession starts the next generation", rec(attempt("expired", full, nil), ""), func() sessionstore.RelayConfirmation {
			next := attempt("pending", nil, nil)
			next.Generation = 2
			return rec(next, "")
		}(), relayEventCounts(1, 0, 0, 0, 0, 0, 0)},
		{"switch observed", rec(attempt("pending", nil, nil), ""), rec(attempt("switched", nil, nil), ""), relayEventCounts(0, 1, 0, 0, 0, 0, 0)},
		{"partial C", rec(attempt("switched", nil, nil), ""), rec(attempt("switched", []string{"0"}, nil), ""), relayEventCounts(0, 0, 0, 0, 0, 0, 0)},
		{"C completes", rec(attempt("switched", []string{"0"}, nil), ""), rec(attempt("switched", full, nil), ""), relayEventCounts(0, 0, 1, 0, 0, 0, 0)},
		{"A completes and confirms", rec(attempt("switched", full, []string{"1"}), ""), rec(attempt("confirmed", full, full), ""), relayEventCounts(0, 0, 0, 1, 1, 0, 0)},
		{"A before the observed commit: one write switches and confirms", rec(attempt("pending", nil, nil), ""), rec(attempt("confirmed", nil, full), ""), relayEventCounts(0, 1, 0, 1, 1, 0, 0)},
		{"already confirmed: an episode completion adds nothing", rec(attempt("confirmed", nil, full), sessionstore.RelayEpisodeOpen), rec(attempt("confirmed", nil, full), sessionstore.RelayEpisodeCompleted), relayEventCounts(0, 0, 0, 0, 0, 0, 0)},
		{"expiry", rec(attempt("switched", full, nil), sessionstore.RelayEpisodeOpen), rec(attempt("expired", full, nil), ""), relayEventCounts(0, 0, 0, 0, 0, 0, 0)},
		{"episode ends on its attempt's slot", rec(attempt("switched", nil, nil), sessionstore.RelayEpisodeOpen), rec(attempt("expired", nil, nil), sessionstore.RelayEpisodeEnded), relayEventCounts(0, 0, 0, 0, 0, 0, 1)},
		{"an ended episode stays ended", rec(attempt("expired", nil, nil), sessionstore.RelayEpisodeEnded), rec(attempt("expired", nil, nil), sessionstore.RelayEpisodeEnded), relayEventCounts(0, 0, 0, 0, 0, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(&fakeRelay{}, sessionstore.NewMemory())
			p.relayMetrics.exposeSlot("r2")
			p.recordRelayWrite("unknown-call", committed, tc.from, tc.to)
			require.Equal(t, tc.want, relayEvents(t, p, "r2"))
		})
	}
	// An episode that ends without an attempt it owns counts on the call's slot.
	p := New(&fakeRelay{}, sessionstore.NewMemory())
	p.relayMetrics.exposeSlot("r1")
	open := sessionstore.RelayConfirmation{Mode: "delivery", Episode: &sessionstore.RelayEpisode{Seq: 1, Token: "e", Outcome: sessionstore.RelayEpisodeOpen}}
	ended := open
	ended.Episode = &sessionstore.RelayEpisode{Seq: 1, Token: "e", Outcome: sessionstore.RelayEpisodeEnded}
	p.recordRelayWrite("unknown-call", sessionstore.RelayAssignment{Slot: "r1"}, open, ended)
	require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 0, 1), relayEvents(t, p, "r1"))
}

// A delivery-mode change: one event of each kind on the target slot, with
// one switch time and one confirmation time.
func TestRelayMetricsCountDeliveryChange(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 0, 0), relayEvents(t, p, "r2"), "configured slots start at zero")
		require.Zero(t, switchTimes(t, p, "r2"))
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		waitAttempt(t, store, id, 1)
		commitPending(t, store, id)
		require.NoError(t, <-done)
		require.Equal(t, relayEventCounts(1, 1, 1, 0, 1, 0, 0), relayEvents(t, p, "r2"))
		require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 0, 0), relayEvents(t, p, "r1"))
		require.Equal(t, float64(1), switchTimes(t, p, "r2"))
		require.Equal(t, float64(1), confirmationTimes(t, p, "r2", sessionstore.RelayConfirmDelivery))
		require.Zero(t, confirmationTimes(t, p, "r2", sessionstore.RelayConfirmDecode))
	})
}

// Rollback path (ADR 0016 section 7): the change to r2 switches and delivers
// but never decodes; its rollback to r1 is a new attempt confirmed by A.
func TestRelayMetricsCountRollback(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, worker, _, id := relayFailurePlane(t, store, sessionstore.RelayConfirmDecode)
		server := httptest.NewServer(p.Handler())
		defer server.Close()
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		waitAttempt(t, store, id, 1)
		commitPending(t, store, id)
		lease, rollback := waitRelayPendingAt(t, store, id, 1)
		worker.noDelivery.Store(true) // the rollback is confirmed by A alone
		waitAttempt(t, store, id, 2)
		require.NoError(t, store.(sessionstore.RelayAssignments).CommitRelay(context.Background(), lease, rollback, sessionstore.RelayAssignment{Slot: "r1", Generation: 2}))
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":2,"decoded_mids":["0"]}`).StatusCode)
		require.ErrorIs(t, <-done, ErrRelayRolledBack)
		// A repeated acknowledgement finds the decision saved.
		require.Equal(t, http.StatusNoContent, postProgress(t, server.URL+"/calls/"+id+"/media-progress", "application/json", `{"generation":2,"decoded_mids":["0"]}`).StatusCode)

		require.Equal(t, relayEventCounts(1, 1, 1, 0, 0, 1, 0), relayEvents(t, p, "r2"))
		require.Equal(t, relayEventCounts(1, 1, 0, 1, 1, 0, 0), relayEvents(t, p, "r1"))
		require.Equal(t, float64(1), switchTimes(t, p, "r2"))
		require.Equal(t, float64(1), switchTimes(t, p, "r1"))
		require.Zero(t, confirmationTimes(t, p, "r2", sessionstore.RelayConfirmDecode))
		require.Equal(t, float64(1), confirmationTimes(t, p, "r1", sessionstore.RelayConfirmDecode))
	})
}

// Failure path: the episode's end is counted once by the control whose write
// decided it, including a write whose reply was lost but whose read-back
// proves it. A retry, and a successor control, find it saved and add nothing.
func TestRelayMetricsCountFailedEndOnce(t *testing.T) {
	for _, lose := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "reply lost"}[lose], func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, backing sessionstore.Store) {
				store := &ambiguousStore{relayStore: backing.(relayStore)}
				p, a, b, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				c, err := p.lookup(id)
				require.NoError(t, err)
				token := randomEpisodeToken()
				now := time.Now()
				lease, _ := seedAttempt(t, p, store, id, "r2", sessionstore.RelayConfirmDelivery, sessionstore.RelayAttempt{
					Required: []string{"0"}, Kind: "recovery", Episode: token, Detected: now, EpisodeDeadline: now.Add(time.Minute), StoreDeadline: now.Add(time.Minute)})
				at, rec, err := store.RelayConfirmation(context.Background(), id)
				require.NoError(t, err)
				open := rec
				open.Episode = &sessionstore.RelayEpisode{Seq: 1, Token: token, StoreDeadline: now.Add(time.Minute), Outcome: sessionstore.RelayEpisodeOpen}
				require.NoError(t, store.CASRelayConfirmation(context.Background(), id, at, rec, open, time.Time{}, time.Time{}))

				store.lose.Store(lose)
				require.Equal(t, relayEnded, p.endRelayEpisode(context.Background(), c, token, "relay_change_failed", false))
				store.lose.Store(false)
				require.Equal(t, relayEnded, p.endRelayEpisode(context.Background(), c, token, "relay_change_failed", false), "a retry follows the saved end")
				require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 0, 1), relayEvents(t, p, "r2"), "the end names the episode's attempt slot")
				require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 0, 0), relayEvents(t, p, "r1"))

				next := successorPlane(t, store, a.fakeWorker, b)
				restored := next.rebuildCall(lease)
				require.Equal(t, relayEnded, next.endRelayEpisode(context.Background(), restored, token, "relay_change_failed", false))
				require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 0, 0), relayEvents(t, next, "r2"), "a successor replays no decision")
			})
		})
	}
}

// successorPlane is a restarted control on the same store: the same workers
// and relay set, with no calls until it rebuilds them.
func successorPlane(t *testing.T, store sessionstore.Store, a, b *fakeWorker) *Plane {
	t.Helper()
	next := New(&fakeRelay{}, store)
	require.NoError(t, next.Register("a", a.addr, &relayProtocolWorker{fakeWorker: a}))
	require.NoError(t, next.Register("b", b.addr, &relayProtocolWorker{fakeWorker: b}))
	require.NoError(t, next.ConfigureRelaySet(context.Background(), relayTestSlots()))
	return next
}

// Control restart: the first control records the start and the observed
// switch, then a successor resumes the attempt. The successor never records
// what was already saved, and it never times an attempt it did not see
// switch (that would compare two hosts' clocks). Each event is recorded once
// across both controls.
func TestRelayMetricsNoDoubleCountAcrossRestart(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, worker, b, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		worker.noDelivery.Store(true)
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(context.Background(), id, "r2") }()
		waitAttempt(t, store, id, 1)
		commitPending(t, store, id)
		require.Eventually(t, func() bool {
			return attemptOf(t, store, id).Attempt.Phase == sessionstore.RelayPhaseSwitched
		}, 2*time.Second, 5*time.Millisecond)
		require.Equal(t, relayEventCounts(1, 1, 0, 0, 0, 0, 0), relayEvents(t, p, "r2"))

		next := successorPlane(t, store, worker.fakeWorker, b)
		stopRelayRecoveries(t, next)
		lease, err := store.Get(context.Background(), id)
		require.NoError(t, err)
		next.rebuildCall(lease)
		next.resumeRelayAttempts(context.Background())
		require.Eventually(t, func() bool { return attemptOf(t, store, id).Attempt.Episode != "" }, 2*time.Second, 5*time.Millisecond, "the successor adopts the attempt")
		worker.noDelivery.Store(false)
		require.Eventually(t, func() bool {
			return attemptOf(t, store, id).Attempt.Phase == sessionstore.RelayPhaseConfirmed
		}, 5*time.Second, 5*time.Millisecond)
		<-done

		first, second := relayEvents(t, p, "r2"), relayEvents(t, next, "r2")
		require.Zero(t, second[relayStarted], "resuming saves no new attempt")
		require.Zero(t, second[relaySwitched], "the switch was already observed")
		for _, event := range []string{relayStarted, relaySwitched, relayDelivered, relayConfirmed} {
			require.Equal(t, float64(1), first[event]+second[event], event)
		}
		require.Equal(t, float64(1), switchTimes(t, p, "r2"))
		require.Zero(t, switchTimes(t, next, "r2"))
		require.Equal(t, first[relayConfirmed], confirmationTimes(t, p, "r2", sessionstore.RelayConfirmDelivery), "only the control that saw the switch times the confirmation")
		require.Zero(t, confirmationTimes(t, next, "r2", sessionstore.RelayConfirmDelivery))

		// A third control finds everything decided.
		third := successorPlane(t, store, worker.fakeWorker, b)
		stopRelayRecoveries(t, third)
		third.rebuildCall(lease)
		third.resumeRelayAttempts(context.Background())
		require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 0, 0), relayEvents(t, third, "r2"))
	})
}

// A relay-loss decision is counted once per slot; retries keep the decision.
func TestRelayMetricsCountLossDecisionOnce(t *testing.T) {
	p, _, _, _ := relayFailurePlane(t, sessionstore.NewMemory())
	stopRelayRecoveries(t, p)
	losses := func(slot string) float64 {
		return relayMetric(t, p, "relais_control_relay_loss_decisions_total", map[string]string{"slot": slot})
	}
	require.Zero(t, losses("r1"))
	require.NoError(t, p.loseRelay(context.Background(), "r1"))
	require.NoError(t, p.loseRelay(context.Background(), "r1"))
	require.Equal(t, float64(1), losses("r1"))
	require.Zero(t, losses("r2"))
}

// Profiles without a relay set expose no relay series.
func TestRelayMetricsAbsentWithoutRelaySet(t *testing.T) {
	p := New(&fakeRelay{}, sessionstore.NewMemory())
	body := planeMetricsBody(p)
	require.NotContains(t, body, "relais_control_relay_")
}
