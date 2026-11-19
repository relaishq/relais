package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	return w.relayProtocolWorker.RelayDelivery(ctx, id, g)
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
		store.afterRead = func() {
			require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
			require.Equal(t, "r2", (<-w.committed).Slot)
			p.mu.Lock()
			b := p.relaySet["r2"]
			b.draining = true // no planned target remains after recovery's commit
			p.relaySet["r2"] = b
			p.mu.Unlock()
			close(observed)
		}
		done := make(chan RelayDrainResult, 1)
		go func() { done <- drainHTTP(t, p, context.Background()) }()
		<-observed
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
