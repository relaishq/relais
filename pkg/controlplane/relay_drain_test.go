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
	failureID string
	failure   error
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
		if w.gate != nil {
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
					_ = w.store.(sessionstore.RelayAssignments).CommitRelay(ctx, lease, a, sessionstore.RelayAssignment{Slot: a.Pending, Generation: g})
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
		status := relayStatusOf(t, p, ids[0])
		require.Equal(t, "r1", status.Slot)
		require.Equal(t, result.Moves[0].Error, status.NotMovable)
		require.EqualValues(t, 4, w.attempts.Load())
		require.Len(t, w.times, 4)
		for i, want := range []float64{1, 2, 4} {
			require.InDelta(t, want, w.times[i+1].Sub(w.times[i]).Seconds(), 0.1)
		}
		// Failed attempts do not become successes; only rollback attempts complete.
		require.Equal(t, relayEventCounts(0, 0, 0, 0, 0, 4, 0), relayEvents(t, p, "r2"))
		require.Equal(t, relayEventCounts(4, 4, 4, 0, 4, 0, 0), relayEvents(t, p, "r1"))
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
		for _, move := range result.Moves {
			require.True(t, move.NotMovable)
			require.Equal(t, context.DeadlineExceeded.Error(), move.Error)
			require.Empty(t, move.To)
			require.Equal(t, "r1", relayStatusOf(t, p, move.ID).Slot)
		}
		require.EqualValues(t, 8, w.attempts.Load(), "queued calls never start after cancellation")
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
	})
}

func TestRelayDrainEmptyHTTPResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _, _ := drainPlane(t, 1)
		drainHTTP(t, p, context.Background())
		result := drainHTTP(t, p, context.Background())
		require.NotNil(t, result.Moves)
		require.Empty(t, result.Moves)
	})
}
