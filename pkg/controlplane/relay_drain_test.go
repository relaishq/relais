package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// drainWorker supplies C and plays the worker's switch commit after control
// saves the attempt. Failures occur before switching; rollback uses real code.
type drainWorker struct {
	*relayProtocolWorker
	failures  atomic.Int64
	attempts  atomic.Int64
	active    atomic.Int64
	peak      atomic.Int64
	gate      chan struct{}
	gateAfter int64
	failureID string
	failure   error
	committed chan sessionstore.RelayAssignment
	muTimes   sync.Mutex
	times     []time.Time
	targets   []string

	missingSlots []string // withhold delivery evidence on these relays
}

func (w *drainWorker) RelayDelivery(ctx context.Context, id string, g uint64) (mediaworker.RelayDeliveryReport, error) {
	lease, a, err := w.store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
	if err != nil {
		return mediaworker.RelayDeliveryReport{}, err
	}
	if a.Pending != "" && a.Pending != "r1" {
		w.attempts.Add(1)
		w.muTimes.Lock()
		w.times = append(w.times, time.Now())
		w.targets = append(w.targets, a.Pending)
		w.muTimes.Unlock()
		n := w.active.Add(1)
		for old := w.peak.Load(); n > old; old = w.peak.Load() {
			if w.peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer w.active.Add(-1)
		if w.gate != nil && w.attempts.Load() > w.gateAfter {
			select {
			case <-w.gate:
			case <-ctx.Done():
				return mediaworker.RelayDeliveryReport{}, ctx.Err()
			}
		}
		if (w.failureID == "" || w.failureID == id) && w.failures.Add(-1) >= 0 {
			return mediaworker.RelayDeliveryReport{}, w.failure
		}
	}
	if a.Pending != "" {
		go func() {
			for {
				_, rec, e := confirmations(w.store).RelayConfirmation(ctx, id)
				if e != nil {
					return
				}
				if rec.Attempt != nil && rec.Attempt.Generation == g {
					to := sessionstore.RelayAssignment{Slot: a.Pending, Generation: g}
					if err := w.store.(sessionstore.RelayAssignments).CommitRelay(ctx, lease, a, to); err == nil && w.committed != nil {
						w.committed <- to
					}
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Millisecond):
				}
			}
		}()
	}
	report, err := w.relayProtocolWorker.RelayDelivery(ctx, id, g)
	if slices.Contains(w.missingSlots, report.Slot) {
		report.Delivered = nil
	}
	return report, err
}

func drainPlane(t *testing.T, count int) (*Plane, *drainWorker, []string) {
	t.Helper()
	p, a, _, id := relayFailurePlane(t, sessionstore.NewMemory())
	w := &drainWorker{relayProtocolWorker: a, failure: errors.New("temporary delivery failure")}
	p.workers["a"].worker = w
	ids := []string{id}
	binding := p.relaySet["r2"]
	binding.draining = true
	p.relaySet["r2"] = binding
	for i := 1; i < count; i++ {
		id, _, err := p.Create(context.Background(), "offer", "a")
		require.NoError(t, err)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.acceptRelayMode(context.Background(), c, sessionstore.RelayConfirmDelivery))
		ids = append(ids, id)
	}
	binding.draining = false
	p.relaySet["r2"] = binding
	return p, w, ids
}

func TestRelayDrainAllMoved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 3)
		require.NoError(t, p.DrainRelay(context.Background(), "r1"))
		for _, id := range ids {
			require.Equal(t, "r2", relayStatusOf(t, p, id).Slot)
		}
		require.EqualValues(t, 3, w.attempts.Load())
	})
}

func TestRelayDrainRetriesTransientFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		w.failures.Store(1)
		start := time.Now()
		require.NoError(t, p.DrainRelay(context.Background(), "r1"))
		require.Equal(t, "r2", relayStatusOf(t, p, ids[0]).Slot)
		require.EqualValues(t, 2, w.attempts.Load())
		require.InDelta(t, 1, time.Since(start).Seconds(), 0.1)
	})
}

func TestRelayDrainExcludesNewCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, _ := drainPlane(t, 1)
		w.gate = make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- p.DrainRelay(context.Background(), "r1") }()
		synctest.Wait()
		require.EqualValues(t, 1, w.active.Load())
		id, _, err := p.Create(context.Background(), "offer", "a")
		require.NoError(t, err)
		require.Equal(t, "r2", relayStatusOf(t, p, id).Slot)
		close(w.gate)
		require.NoError(t, <-done)
	})
}

func TestRelayDrainConcurrencyEight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, _ := drainPlane(t, 17)
		w.gate = make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- p.DrainRelay(context.Background(), "r1") }()
		synctest.Wait()
		require.EqualValues(t, 8, w.active.Load())
		close(w.gate)
		require.NoError(t, <-done)
		require.EqualValues(t, 8, w.peak.Load())
		require.EqualValues(t, 17, w.attempts.Load())
	})
}

func drainHTTP(t *testing.T, p *Plane, ctx context.Context) RelayDrainResult {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/relays/r1/drain", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var result RelayDrainResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	return result
}

func TestRelayDrainBackoffExhaustionReportsNotMovable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		w.failures.Store(100)
		result := drainHTTP(t, p, context.Background())
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", NotMovable: true, Error: "controlplane: relay_change_rolled_back temporary delivery failure"}}, result.Moves)
		require.Equal(t, 1, result.NotMovable)
		status := relayStatusOf(t, p, ids[0])
		require.Equal(t, "r1", status.Slot)
		require.Equal(t, result.Moves[0].Error, status.NotMovable)
		require.EqualValues(t, 3, w.attempts.Load())
		require.Len(t, w.times, 3)
		for i, want := range []float64{1, 2} {
			require.InDelta(t, want, w.times[i+1].Sub(w.times[i]).Seconds(), 0.1)
		}
		// Failed attempts do not become successes; only rollback attempts complete.
		require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 3, 0), relayEvents(t, p, "r2"))
		require.Equal(t, relayEventCounts(3, 3, 3, 0, 3, 0, 0), relayEvents(t, p, "r1"))
		// The next successful drain clears the previous report on the old slot.
		w.failures.Store(0)
		moved := drainHTTP(t, p, context.Background())
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2"}}, moved.Moves)
		require.Empty(t, relayStatusOf(t, p, ids[0]).NotMovable)
	})
}

// A delivery-only caller switches to r2 but never confirms audio there. The
// original r1 path still works, even though draining excludes new placement.
// Exhaust the planned retries without ending a call that can roll back.
func TestRelayDrainUnconfirmedSwitchKeepsCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		stopRelayRecoveries(t, p)
		w.missingSlots = []string{"r2"}
		c, err := p.lookup(ids[0])
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		result, err := p.drainRelay(ctx, "r1")
		t.Logf("drain elapsed=%s error=%v end_reason=%q attempts=%d", time.Since(start), err, endReason(c), w.attempts.Load())
		require.Empty(t, endReason(c), "missing delivery on r2 must not end a call with a working r1 rollback")
		_, getErr := p.store.Get(ctx, ids[0])
		require.NoError(t, getErr, "the session must survive the failed drain")
		require.ErrorIs(t, err, ErrRelayRolledBack)
		require.ErrorIs(t, err, errRelayUnconfirmed)
		require.Equal(t, 1, result.NotMovable)
		require.Equal(t, ids[0], result.Moves[0].ID)
		require.True(t, result.Moves[0].NotMovable)
		require.Empty(t, result.Moves[0].To)
		require.EqualValues(t, 3, w.attempts.Load())
		a, rec, err := confirmations(p.store).RelayConfirmation(ctx, ids[0])
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 6}, a)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
		require.False(t, rec.Episode.Open())
	})
}

func TestRelayChangeRollbackKeepsPlannedKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _, _, id := relayFailurePlane(t, sessionstore.NewMemory(), sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- p.ChangeRelay(ctx, id, "r2") }()
		commitPending(t, p.store, id) // C arrives; missing A starts rollback at 3 s.
		lease, rollback := waitRelayPendingAt(t, p.store, id, 1)
		waitAttempt(t, p.store, id, 2)
		status := relayStatusOf(t, p, id)
		require.Equal(t, "r1", status.Pending)
		require.Equal(t, sessionstore.RelayPhasePending, status.Attempt.Phase)
		require.Equal(t, "planned", status.Attempt.Kind)
		_, rec, err := confirmations(p.store).RelayConfirmation(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "planned", rec.Attempt.Kind, "kind is persisted, not rewritten for status")
		require.True(t, rec.Episode.Open())
		require.Equal(t, rec.Episode.Token, rec.Attempt.Episode)
		require.False(t, rec.Attempt.PlannedDeadline.IsZero())
		require.Equal(t, rec.Attempt.Outer(), rec.Attempt.PlannedDeadline)
		require.NoError(t, p.store.(sessionstore.RelayAssignments).CommitRelay(ctx, lease, rollback,
			sessionstore.RelayAssignment{Slot: "r1", Generation: 2}))
		req := httptest.NewRequest(http.MethodPost, "/calls/"+id+"/media-progress",
			bytes.NewBufferString(`{"generation":2,"decoded_mids":["0"]}`))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		p.Handler().ServeHTTP(response, req)
		require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
		require.ErrorIs(t, <-done, ErrRelayRolledBack)
		require.Equal(t, "planned", relayStatusOf(t, p, id).Attempt.Kind)
	})
}

func TestRelayDrainFailedRollbackRecoversOnThirdRelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		stopRelayRecoveries(t, p)
		w.missingSlots = []string{"r1", "r2"}
		w.committed = make(chan sessionstore.RelayAssignment, 4)
		third := relayTestSlots()[1]
		third.Slot.ID, third.draining = "r3", true
		p.relaySet["r3"] = third
		c, err := p.lookup(ids[0])
		require.NoError(t, err)
		start := time.Now()
		done := make(chan RelayDrainResult, 1)
		go func() { done <- drainHTTP(t, p, context.Background()) }()
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, <-w.committed)
		// Make r3 eligible only after the initial planned target is fixed.
		p.mu.Lock()
		third.draining = false
		p.relaySet["r3"] = third
		p.mu.Unlock()
		result := <-done
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r3"}}, result.Moves)
		require.Zero(t, result.NotMovable)
		require.Empty(t, endReason(c))
		require.Less(t, time.Since(start), relayPlannedLimit+relayRecoveryLimit)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 2}, <-w.committed)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r3", Generation: 3}, <-w.committed)
		a, rec, err := confirmations(p.store).RelayConfirmation(context.Background(), ids[0])
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r3", Generation: 3}, a)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
		require.Equal(t, "recovery", rec.Attempt.Kind)
		require.ElementsMatch(t, []string{"r1", "r2", "r3"}, rec.Attempt.Tried)
		require.Equal(t, sessionstore.RelayEpisodeCompleted, rec.Episode.Outcome)
	})
}

func TestRelayDrainTwoRelaysKeepsLateRollbackEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		stopRelayRecoveries(t, p)
		w.missingSlots = []string{"r2"}
		w.noDelivery.Store(true)
		w.committed = make(chan sessionstore.RelayAssignment, 6)
		c, err := p.lookup(ids[0])
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		responderDone := make(chan struct{})
		defer func() { cancel(); <-responderDone }()
		go func() {
			defer close(responderDone)
			for {
				select {
				case a := <-w.committed:
					if a.Slot == "r1" {
						// Receiver evidence arrives 4.5 s after each rollback commit.
						select {
						case <-time.After(4500 * time.Millisecond):
							w.noDelivery.Store(false)
						case <-ctx.Done():
							return
						}
					} else {
						w.noDelivery.Store(true)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
		start := time.Now()
		result, err := p.drainRelay(ctx, "r1")
		t.Logf("late evidence drain elapsed=%s error=%v end_reason=%q", time.Since(start), err, endReason(c))
		require.Empty(t, endReason(c), "a working rollback must survive receiver evidence later than 4 s")
		require.ErrorIs(t, err, ErrRelayRolledBack)
		require.Equal(t, 1, result.NotMovable)
		require.EqualValues(t, 3, w.attempts.Load())
		require.GreaterOrEqual(t, time.Since(start), 46500*time.Millisecond)
		a, rec, err := confirmations(p.store).RelayConfirmation(ctx, ids[0])
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 6}, a)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
		require.False(t, rec.Episode.Open())
	})
}

func TestRelayDrainDecodeFailureExhaustsAllowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, base, _, id := relayFailurePlane(t, sessionstore.NewMemory(), sessionstore.RelayConfirmDecode)
		stopRelayRecoveries(t, p)
		w := &drainWorker{relayProtocolWorker: base, committed: make(chan sessionstore.RelayAssignment, 6)}
		p.workers["a"].worker = w
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- p.DrainRelay(ctx, "r1") }()
		for generation := uint64(1); generation <= 6; generation++ {
			a := <-w.committed
			require.Equal(t, generation, a.Generation)
			if a.Slot != "r1" {
				continue // C arrives on r2, but no A: fail at its 3 s bound.
			}
			body, err := json.Marshal(mediaProgress{Generation: generation, DecodedMIDs: []string{"0"}})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/calls/"+id+"/media-progress", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			p.Handler().ServeHTTP(response, req)
			require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
		}
		err := <-done
		require.ErrorIs(t, err, ErrRelayRolledBack)
		require.ErrorContains(t, err, "delivered_decode_unconfirmed")
		require.EqualValues(t, 3, w.attempts.Load())
		require.GreaterOrEqual(t, time.Since(start), 12*time.Second)
		require.Less(t, time.Since(start), 15*time.Second)
		status := relayStatusOf(t, p, id)
		require.Equal(t, "r1", status.Slot)
		require.EqualValues(t, 6, status.Generation)
		require.NotEmpty(t, status.NotMovable)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, status.Attempt.Phase)
	})
}

// Giving rollback the remaining episode budget must not extend the hard end
// when neither relay can supply confirmation.
func TestRelayDrainUnconfirmedRollbackEndsWithinEpisodeBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		stopRelayRecoveries(t, p)
		w.noDelivery.Store(true)
		c, err := p.lookup(ids[0])
		require.NoError(t, err)
		published := make(chan time.Time, 1)
		hook := func(observed *call, event string) {
			if observed == c && event == "published" {
				published <- time.Now()
			}
		}
		relayWatchHooks.Store(&hook)
		defer relayWatchHooks.Store(nil)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		result, err := p.drainRelay(ctx, "r1")
		require.Error(t, err)
		require.Equal(t, "relay_change_failed", endReason(c))
		require.InDelta(t, (relayPlannedLimit + relayRecoveryLimit).Seconds(), (<-published).Sub(start).Seconds(), 0.1)
		require.Zero(t, result.NotMovable, "an ended call is not a live not-movable call")
		require.Empty(t, result.Moves[0].To)
	})
}

func TestRelayDrainCancellationReportsRemaining(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 17)
		w.failures.Store(100)
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		start := time.Now()
		result := drainHTTP(t, p, ctx)
		require.Equal(t, 250*time.Millisecond, time.Since(start), "cancel interrupts the backoff")
		require.Len(t, result.Moves, len(ids))
		require.Zero(t, result.NotMovable)
		for _, move := range result.Moves {
			require.False(t, move.NotMovable)
			require.Empty(t, relayStatusOf(t, p, move.ID).NotMovable)
			require.Equal(t, context.DeadlineExceeded.Error(), move.Error)
			require.Empty(t, move.To)
			require.Equal(t, "r1", relayStatusOf(t, p, move.ID).Slot)
		}
		require.EqualValues(t, 17, w.attempts.Load(), "sleeping calls release capacity for queued calls")
	})
}

// Expiring the drain request must still let ChangeRelay confirm its rollback.
func TestRelayDrainCancellationSettlesRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		w.gate = make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		result := drainHTTP(t, p, ctx)
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", Error: context.DeadlineExceeded.Error()}}, result.Moves)
		a, rec, err := confirmations(p.store).RelayConfirmation(context.Background(), ids[0])
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 2}, a)
		require.True(t, rec.Confirmed(a), "rollback survives request cancellation")
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
		require.False(t, rec.Episode.Open())
		require.EqualValues(t, 1, w.attempts.Load())
		_, err = p.lookup(ids[0])
		require.NoError(t, err, "call remains live on the old path")
	})
}

func TestRelayDrainUnsupportedIsImmediate(t *testing.T) {
	for _, kind := range []string{"worker lacks C", "caller cannot restart"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				if kind == "worker lacks C" {
					w.failure = mediaworker.ErrRelayUnsupported
					w.failures.Store(1)
				} else {
					p2, a, _, id := relayFailurePlane(t, sessionstore.NewMemory(), "")
					p, w, ids = p2, &drainWorker{relayProtocolWorker: a}, []string{id}
				}
				start := time.Now()
				result := drainHTTP(t, p, context.Background())
				require.Less(t, time.Since(start), time.Second)
				require.True(t, result.Moves[0].NotMovable)
				require.Contains(t, result.Moves[0].Error, "unsupported")
				require.Equal(t, "r1", relayStatusOf(t, p, ids[0]).Slot)
				require.Equal(t, result.Moves[0].Error, relayStatusOf(t, p, ids[0]).NotMovable)
				require.LessOrEqual(t, w.attempts.Load(), int64(1))
			})
		})
	}
}

func TestRelayDrainRepicksTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, _ := drainPlane(t, 1)
		w.failures.Store(1)
		b := relayTestSlots()[1]
		b.Slot.ID = "r3"
		p.relaySet["r3"] = b
		go func() { time.Sleep(500 * time.Millisecond); markLost(p, "r2") }()
		result := drainHTTP(t, p, context.Background())
		require.Equal(t, "r3", result.Moves[0].To)
		require.Equal(t, []string{"r2", "r3"}, w.targets)
	})
}

func TestRelayDrainRequestFailures(t *testing.T) {
	p, _, _ := drainPlane(t, 1)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/relays/unknown/drain", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, p.EnableRestart())
	rec = httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/relays/r1/drain", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestRelayDrainMixedHTTPOutcomes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 2)
		w.failureID = ids[0]
		w.failures.Store(100)
		result := drainHTTP(t, p, context.Background())
		require.Equal(t, []RelayMoveResult{
			{ID: ids[0], From: "r1", NotMovable: true, Error: "controlplane: relay_change_rolled_back temporary delivery failure"},
			{ID: ids[1], From: "r1", To: "r2"},
		}, result.Moves)
		require.Empty(t, relayStatusOf(t, p, ids[1]).NotMovable)
		require.Equal(t, 1, result.NotMovable)
	})
}

func TestRelayDrainEmptyHTTPResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _, _ := drainPlane(t, 1)
		drainHTTP(t, p, context.Background())
		result := drainHTTP(t, p, context.Background())
		require.NotNil(t, result.Moves)
		require.Empty(t, result.Moves)
		require.Zero(t, result.NotMovable)
	})
}

func TestRelayDrainEndedCallIsNotNotMovable(t *testing.T) {
	for _, failure := range []error{ErrUnknownCall, sessionstore.ErrNotFound, mediaworker.ErrUnknownSession, sessionstore.ErrEnded, errRelayEpisodeEnded} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, _ := drainPlane(t, 1)
				w.failures.Store(100)
				w.failure = failure
				start := time.Now()
				result := drainHTTP(t, p, context.Background())
				require.False(t, result.Moves[0].NotMovable)
				require.Contains(t, result.Moves[0].Error, failure.Error())
				require.EqualValues(t, 1, w.attempts.Load())
				require.Less(t, time.Since(start), time.Second)
			})
		})
	}
}

func TestRelayDrainCallEndsBetweenAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		w.failures.Store(1)
		go func() { time.Sleep(500 * time.Millisecond); require.NoError(t, p.End(context.Background(), ids[0])) }()
		start := time.Now()
		result := drainHTTP(t, p, context.Background())
		require.False(t, result.Moves[0].NotMovable)
		require.Contains(t, result.Moves[0].Error, "not found")
		require.EqualValues(t, 1, w.attempts.Load())
		require.Less(t, time.Since(start), 2*time.Second)
	})
}

func TestRelayDrainRecoveryLeavesSlot(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "between attempts", true: "open past retry budget"}[blocked], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				b := relayTestSlots()[1]
				b.Slot.ID = "r3"
				b.draining = true
				p.relaySet["r3"] = b
				stopRelayRecoveries(t, p)
				w.failures.Store(1)
				w.gate = make(chan struct{})
				w.gateAfter = 1
				c, err := p.lookup(ids[0])
				require.NoError(t, err)
				actorDone := make(chan struct{})
				go func() {
					defer close(actorDone)
					time.Sleep(500 * time.Millisecond)
					require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
					c.relayEpisodeMu.Lock()
					e := c.relayEpisode
					c.relayEpisodeMu.Unlock()
					// r3 becomes eligible only after recovery selected r2.
					// A redundant planned retry would then choose r3.
					_, pending := waitRelayPending(t, p.store, ids[0])
					require.Equal(t, "r2", pending.Pending)
					p.mu.Lock()
					b := p.relaySet["r3"]
					b.draining = false
					p.relaySet["r3"] = b
					p.mu.Unlock()
					if blocked {
						time.Sleep(8 * time.Second)
					}
					close(w.gate)
					<-e.done
				}()
				result := drainHTTP(t, p, context.Background())
				<-actorDone
				require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2"}}, result.Moves)
				require.EqualValues(t, 2, w.attempts.Load(), "one failed planned attempt and one recovery attempt")
				require.Empty(t, relayStatusOf(t, p, ids[0]).NotMovable)
			})
		})
	}
}

func TestRelayDrainRecoveryCommitNeedsConfirmation(t *testing.T) {
	for _, confirm := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed", false: "ended"}[confirm], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				stopRelayRecoveries(t, p)
				store := &drainObservationStore{Memory: p.store.(*sessionstore.Memory), confirmed: make(chan sessionstore.RelayAssignment, 4)}
				p.store, w.store = store, store
				w.failures.Store(1)
				w.committed = make(chan sessionstore.RelayAssignment, 4)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan RelayDrainResult, 1)
				go func() { done <- drainHTTP(t, p, ctx) }()
				synctest.Wait()
				require.Equal(t, "r1", (<-w.committed).Slot)
				require.Equal(t, "r1", (<-store.confirmed).Slot)
				synctest.Wait()
				_, rollback, err := confirmations(p.store).RelayConfirmation(context.Background(), ids[0])
				require.NoError(t, err)
				require.Equal(t, sessionstore.RelayPhaseConfirmed, rollback.Attempt.Phase)
				w.noDelivery.Store(true)
				c, err := p.lookup(ids[0])
				require.NoError(t, err)
				require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
				require.Equal(t, "r2", (<-w.committed).Slot)
				synctest.Wait()
				a, rec, err := confirmations(p.store).RelayConfirmation(context.Background(), ids[0])
				require.NoError(t, err)
				require.Empty(t, a.Pending, "worker has committed before the test holds confirmation")
				require.True(t, rec.Episode.Open())
				require.False(t, rec.Confirmed(a))
				time.Sleep(2 * time.Second) // cross the drain's next observation
				synctest.Wait()
				var result RelayDrainResult
				returned := false
				select {
				case result = <-done:
					returned = true
				default:
				}
				if confirm {
					w.noDelivery.Store(false)
				}
				if !returned {
					result = <-done
				}
				require.False(t, returned, "commit alone must not finish the drain")
				if confirm {
					require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2"}}, result.Moves)
					a, rec, err = confirmations(p.store).RelayConfirmation(context.Background(), ids[0])
					require.NoError(t, err)
					require.True(t, rec.Confirmed(a))
					require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)
					require.Equal(t, sessionstore.RelayEpisodeCompleted, rec.Episode.Outcome)
				} else {
					require.Empty(t, result.Moves[0].To)
					require.NotEmpty(t, result.Moves[0].Error)
					require.False(t, result.Moves[0].NotMovable)
					require.Equal(t, "relay_change_failed", endReason(c))
				}
				require.EqualValues(t, 2, w.attempts.Load(), "recovery must not be followed by another media move")
			})
		})
	}
}

// Pause the first observation after its store read, so recovery can commit
// before target selection fails and the drain reads the post-attempt state.
type drainObservationStore struct {
	*sessionstore.Memory
	mu        sync.Mutex
	afterRead func()
	confirmed chan sessionstore.RelayAssignment
}

func (s *drainObservationStore) RelayConfirmation(ctx context.Context, id string) (sessionstore.RelayAssignment, sessionstore.RelayConfirmation, error) {
	a, rec, err := s.Memory.RelayConfirmation(ctx, id)
	s.mu.Lock()
	hook := s.afterRead
	s.afterRead = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return a, rec, err
}

func (s *drainObservationStore) CASRelayConfirmation(ctx context.Context, id string, at sessionstore.RelayAssignment, from, to sessionstore.RelayConfirmation, notBefore, notAfter time.Time) error {
	err := s.Memory.CASRelayConfirmation(ctx, id, at, from, to, notBefore, notAfter)
	if err == nil && s.confirmed != nil && to.Attempt != nil && to.Attempt.Phase == sessionstore.RelayPhaseConfirmed &&
		(from.Attempt == nil || from.Attempt.Phase != sessionstore.RelayPhaseConfirmed) {
		s.confirmed <- at
	}
	return err
}

func TestRelayDrainRecoveryCommitsDuringObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		stopRelayRecoveries(t, p)
		store := &drainObservationStore{Memory: p.store.(*sessionstore.Memory)}
		p.store, w.store = store, store
		w.noDelivery.Store(true)
		w.committed = make(chan sessionstore.RelayAssignment, 1)
		c, err := p.lookup(ids[0])
		require.NoError(t, err)
		observed := make(chan struct{})
		var recoveryErr error
		var committed sessionstore.RelayAssignment
		store.afterRead = func() {
			defer close(observed)
			recoveryErr = p.startRelayRecovery(context.Background(), c, false)
			if recoveryErr != nil {
				return
			}
			committed = <-w.committed
			p.mu.Lock()
			b := p.relaySet["r2"]
			b.draining = true // no planned target remains after recovery's commit
			p.relaySet["r2"] = b
			p.mu.Unlock()
		}
		done := make(chan RelayDrainResult, 1)
		go func() { done <- drainHTTP(t, p, context.Background()) }()
		<-observed
		require.NoError(t, recoveryErr)
		require.Equal(t, "r2", committed.Slot)
		synctest.Wait()
		a, rec, err := store.RelayConfirmation(context.Background(), ids[0])
		require.NoError(t, err)
		require.Equal(t, "r2", a.Slot)
		require.Empty(t, a.Pending)
		require.True(t, rec.Episode.Open())
		require.False(t, rec.Confirmed(a))
		var result RelayDrainResult
		returned := false
		select {
		case result = <-done:
			returned = true
		default:
		}
		w.noDelivery.Store(false)
		if !returned {
			result = <-done
		}
		require.False(t, returned, "post-attempt observation must not accept commit alone")
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2"}}, result.Moves)
		require.Equal(t, sessionstore.RelayEpisodeCompleted, episodeOf(t, store, ids[0]).Outcome)
		require.EqualValues(t, 1, w.attempts.Load())
	})
}

func TestRelayDrainConfirmedRecoveryDiscardsPickError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		stopRelayRecoveries(t, p)
		store := &drainObservationStore{Memory: p.store.(*sessionstore.Memory), confirmed: make(chan sessionstore.RelayAssignment, 4)}
		p.store, w.store = store, store
		w.committed = make(chan sessionstore.RelayAssignment, 1)
		c, err := p.lookup(ids[0])
		require.NoError(t, err)
		var recoveryErr error
		var committed, confirmed sessionstore.RelayAssignment
		store.afterRead = func() {
			recoveryErr = p.startRelayRecovery(context.Background(), c, false)
			if recoveryErr != nil {
				return
			}
			committed, confirmed = <-w.committed, <-store.confirmed
			synctest.Wait()
			p.mu.Lock()
			b := p.relaySet["r2"]
			b.draining = true // target selection fails after recovery has confirmed
			p.relaySet["r2"] = b
			p.mu.Unlock()
		}
		result, drainErr := p.drainRelay(context.Background(), "r1")
		require.NoError(t, recoveryErr)
		require.Equal(t, "r2", committed.Slot)
		require.Equal(t, committed, confirmed)
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2"}}, result.Moves)
		require.NoError(t, drainErr, "a confirmed recovery supersedes target selection failure")
		require.EqualValues(t, 1, w.attempts.Load())
	})
}

func TestRelayDrainOverallDeadline(t *testing.T) {
	for _, leaseLoss := range []bool{false, true} {
		t.Run(map[bool]string{false: "no target", true: "lease loss"}[leaseLoss], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				lease, err := p.store.Get(context.Background(), ids[0])
				require.NoError(t, err)
				_, err = p.store.Renew(context.Background(), lease, 5*time.Minute)
				require.NoError(t, err)
				if leaseLoss {
					store := &drainCompetingStore{Memory: p.store.(*sessionstore.Memory)}
					store.failures.Store(1000)
					p.store, w.store = store, store
				} else {
					b := p.relaySet["r2"]
					b.draining = true
					p.relaySet["r2"] = b
				}
				// No caller deadline: cancellation is only a cleanup for broken code.
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan RelayDrainResult, 1)
				start := time.Now()
				go func() { done <- drainHTTP(t, p, ctx) }()
				synctest.Wait()
				time.Sleep(2*time.Minute - time.Nanosecond)
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("drain expired before its overall deadline")
				default:
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				var result RelayDrainResult
				returned := false
				select {
				case result = <-done:
					returned = true
				default:
					cancel()
					result = <-done
				}
				require.True(t, returned, "drain must finish at its own deadline")
				require.Equal(t, 2*time.Minute, time.Since(start))
				require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", Error: context.DeadlineExceeded.Error()}}, result.Moves)
				require.Zero(t, result.NotMovable)
				require.Zero(t, w.attempts.Load(), "waiting does not start or exhaust planned attempts")
				require.Equal(t, "r1", relayStatusOf(t, p, ids[0]).Slot)
				_, err = p.lookup(ids[0])
				require.NoError(t, err, "deadline must preserve the working call")
			})
		})
	}
}

func TestRelayDrainReportsCleanupFailure(t *testing.T) {
	for _, http := range []bool{false, true} {
		t.Run(map[bool]string{false: "error API", true: "HTTP report"}[http], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				cleanup := &retryTenureRelay{fakeRelay: &fakeRelay{}, failures: 1}
				b := p.relaySet["r1"]
				b.Relay = cleanup
				p.relaySet["r1"] = b
				if http {
					result := drainHTTP(t, p, context.Background())
					require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2", Error: sessionstore.ErrTransient.Error()}}, result.Moves)
					require.Zero(t, result.NotMovable)
				} else {
					require.ErrorIs(t, p.DrainRelay(context.Background(), "r1"), sessionstore.ErrTransient)
				}
				require.EqualValues(t, 1, w.attempts.Load(), "cleanup failure must not repeat a confirmed media move")
				require.Equal(t, 1, cleanup.attempts)
				a, rec, err := confirmations(p.store).RelayConfirmation(context.Background(), ids[0])
				require.NoError(t, err)
				require.Equal(t, "r2", a.Slot)
				require.True(t, rec.Confirmed(a))
				require.False(t, rec.Episode.Open())
			})
		})
	}
}

type drainDeadlineCleanupRelay struct {
	*fakeRelay
	failure error
	calls   int
}

func (r *drainDeadlineCleanupRelay) ForgetSessionTenure(ctx context.Context, _ sessionstore.Lease) error {
	r.calls++
	<-ctx.Done()
	if r.failure != nil {
		return r.failure
	}
	return ctx.Err()
}

func TestRelayDrainDeadlinePreservesConfirmedCleanupFailure(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, errors.New("old relay cleanup failed")} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				cleanup := &drainDeadlineCleanupRelay{fakeRelay: &fakeRelay{}, failure: failure}
				b := p.relaySet["r1"]
				b.Relay = cleanup
				p.relaySet["r1"] = b
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				result, drainErr := p.drainRelay(ctx, "r1")
				require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2", Error: failure.Error()}}, result.Moves)
				require.ErrorIs(t, drainErr, failure)
				require.Zero(t, result.NotMovable)
				a, rec, err := confirmations(p.store).RelayConfirmation(context.Background(), ids[0])
				require.NoError(t, err)
				require.Equal(t, "r2", a.Slot)
				require.True(t, rec.Confirmed(a))
				require.False(t, rec.Episode.Open())
				require.EqualValues(t, 1, w.attempts.Load(), "deadline must not repeat the media move")
				require.Equal(t, 1, cleanup.calls)
			})
		})
	}
}

// Cleanup errors keep their cause for both a confirmed move and a rollback.
// A rollback must also retain the original planned failure.
func TestRelayDrainChangeRelayCleanupOutcomes(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "move", true: "rollback"}[rollback], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				cleanupSlot, destination := "r1", "r2"
				if rollback {
					w.failures.Store(1)
					cleanupSlot, destination = "r2", "r1"
				}
				cleanup := &retryTenureRelay{fakeRelay: &fakeRelay{}, failures: 1}
				b := p.relaySet[cleanupSlot]
				b.Relay = cleanup
				p.relaySet[cleanupSlot] = b
				err := p.ChangeRelay(context.Background(), ids[0], "r2")
				require.ErrorIs(t, err, sessionstore.ErrTransient)
				var cleanupErr *relayCleanupFailure
				require.Equal(t, !rollback, errors.As(err, &cleanupErr), "only a confirmed move classifies its cleanup failure")
				if rollback {
					require.ErrorIs(t, err, ErrRelayRolledBack)
					require.ErrorIs(t, err, w.failure)
				} else {
					require.EqualError(t, err, sessionstore.ErrTransient.Error())
				}
				require.Equal(t, destination, relayStatusOf(t, p, ids[0]).Slot)
				require.EqualValues(t, 1, w.attempts.Load())
				require.Equal(t, 1, cleanup.attempts)
			})
		})
	}
}

func TestRelayDrainDeadlineKeepsUnmovedCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		w.gate = make(chan struct{})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result, err := p.drainRelay(ctx, "r1")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", Error: context.DeadlineExceeded.Error()}}, result.Moves)
		require.Zero(t, result.NotMovable)
		require.Equal(t, "r1", relayStatusOf(t, p, ids[0]).Slot)
		require.EqualValues(t, 1, w.attempts.Load())
	})
}

func TestRelayDrainBackoffReleasesCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, _ := drainPlane(t, 17)
		w.failures.Store(100)
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		drainHTTP(t, p, ctx)
		require.EqualValues(t, 17, w.attempts.Load(), "all first attempts run before any retry is due")
		require.LessOrEqual(t, w.peak.Load(), int64(8))
	})
}

func TestRelayDrainReasonClearedByLaterChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		w.failures.Store(100)
		result := drainHTTP(t, p, context.Background())
		require.True(t, result.Moves[0].NotMovable)
		w.failures.Store(0)
		require.NoError(t, p.ChangeRelay(context.Background(), ids[0], "r2"))
		p.mu.Lock()
		b := p.relaySet["r1"]
		b.draining = false
		p.relaySet["r1"] = b
		p.mu.Unlock()
		require.NoError(t, p.ChangeRelay(context.Background(), ids[0], "r1"))
		require.Empty(t, relayStatusOf(t, p, ids[0]).NotMovable)
	})
}

// Another transition can consume a generation before our pending CAS is
// acknowledged. That is not evidence that this drain ran a planned attempt.
type drainCompetingStore struct {
	*sessionstore.Memory
	failures atomic.Int64
}

func (s *drainCompetingStore) CASRelay(ctx context.Context, lease sessionstore.Lease, from, to sessionstore.RelayAssignment) error {
	if from.Pending == "" && to.Pending != "" && s.failures.Add(-1) >= 0 {
		if err := s.Memory.CASRelay(ctx, lease, from, to); err != nil {
			return err
		}
		retired := from
		retired.Generation++
		if err := s.Memory.CASRelay(ctx, lease, to, retired); err != nil {
			return err
		}
		return sessionstore.ErrLeaseLost
	}
	return s.Memory.CASRelay(ctx, lease, from, to)
}

func TestRelayDrainDoesNotCountAnotherTransition(t *testing.T) {
	for _, expires := range []bool{false, true} {
		t.Run(map[bool]string{false: "eventually starts", true: "context expires"}[expires], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				store := &drainCompetingStore{Memory: p.store.(*sessionstore.Memory)}
				store.failures.Store(4)
				p.store = store
				w.store = store
				ctx := context.Background()
				if expires {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 1500*time.Millisecond)
					defer cancel()
					store.failures.Store(100)
				}
				start := time.Now()
				result := drainHTTP(t, p, ctx)
				require.False(t, result.Moves[0].NotMovable)
				require.Zero(t, result.NotMovable)
				require.Empty(t, relayStatusOf(t, p, ids[0]).NotMovable)
				if expires {
					require.Equal(t, context.DeadlineExceeded.Error(), result.Moves[0].Error)
					require.Zero(t, w.attempts.Load())
					require.Equal(t, 1500*time.Millisecond, time.Since(start))
				} else {
					require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2"}}, result.Moves)
					require.EqualValues(t, 1, w.attempts.Load(), "lease-loss observations do not consume planned attempts")
					require.InDelta(t, 11, time.Since(start).Seconds(), 0.1)
				}
			})
		})
	}
}

func TestRelayDrainTargetLostBeforeAttemptRepicks(t *testing.T) {
	for _, lost := range []bool{true, false} {
		t.Run(map[bool]string{true: "lost", false: "draining"}[lost], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, w, ids := drainPlane(t, 1)
				extra := relayTestSlots()[1]
				extra.Slot.ID = "r3"
				extra.draining = true
				p.relaySet["r3"] = extra
				c, err := p.lookup(ids[0])
				require.NoError(t, err)
				require.NoError(t, c.lockControl(context.Background()))
				held := true
				defer func() {
					if held {
						c.unlockControl()
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				rec := httptest.NewRecorder()
				done := make(chan struct{})
				start := time.Now()
				go func() {
					defer close(done)
					p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/relays/r1/drain", nil).WithContext(ctx))
				}()
				// ChangeRelay has picked r2 and now waits at the operation lock.
				synctest.Wait()
				c.controlMu.Lock()
				waiters := c.controlWaiters
				c.controlMu.Unlock()
				require.Equal(t, 1, waiters)
				p.mu.Lock()
				b := p.relaySet["r2"]
				b.lost = lost
				b.draining = !lost
				p.relaySet["r2"] = b
				extra.draining = false
				p.relaySet["r3"] = extra
				p.mu.Unlock()
				c.unlockControl()
				held = false
				<-done
				require.Equal(t, http.StatusOK, rec.Code)
				var result RelayDrainResult
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
				require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r3"}}, result.Moves)
				require.Zero(t, result.NotMovable)
				require.Equal(t, []string{"r3"}, w.targets, "rejected target never started a planned attempt")
				require.InDelta(t, 1, time.Since(start).Seconds(), 0.1)
			})
		})
	}
}

func TestRelayDrainNoTargetThenTargetAppears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, w, ids := drainPlane(t, 1)
		b := p.relaySet["r2"]
		b.draining = true
		p.relaySet["r2"] = b
		actorDone := make(chan struct{})
		go func() {
			defer close(actorDone)
			time.Sleep(8 * time.Second)
			p.mu.Lock()
			b := p.relaySet["r2"]
			b.draining = false
			p.relaySet["r2"] = b
			p.mu.Unlock()
		}()
		start := time.Now()
		result := drainHTTP(t, p, context.Background())
		<-actorDone
		require.Equal(t, []RelayMoveResult{{ID: ids[0], From: "r1", To: "r2"}}, result.Moves)
		require.Zero(t, result.NotMovable)
		require.EqualValues(t, 1, w.attempts.Load())
		require.InDelta(t, 11, time.Since(start).Seconds(), 0.1, "wait 1, 2, 4, then keep waiting 4 seconds")
	})
}
