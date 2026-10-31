package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func stopRelayRecoveries(t *testing.T, p *Plane) {
	t.Helper()
	t.Cleanup(func() {
		p.mu.Lock()
		var calls []*call
		for _, c := range p.calls {
			calls = append(calls, c)
		}
		p.mu.Unlock()
		for _, c := range calls {
			c.hungUp.Store(true)
			c.controlMu.Lock()
			if c.relayCancel != nil {
				c.relayCancel()
			}
			c.controlMu.Unlock()
			c.relayEpisodeMu.Lock()
			if e := c.relayEpisode; e != nil {
				e.completed.Store(true)
				e.once.Do(func() { close(e.done) })
			}
			c.relayEpisodeMu.Unlock()
		}
		for _, c := range calls {
			require.Eventually(t, func() bool {
				c.relayEpisodeMu.Lock()
				defer c.relayEpisodeMu.Unlock()
				return c.relayEpisode == nil || !c.relayEpisode.running
			}, time.Second, time.Millisecond)
		}
	})
}

func TestSingleRelayStallRequestPreservesCall(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p := New(&fakeRelay{}, store)
		w := &relayProtocolWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:17001"), running: map[string]bool{}}}
		require.NoError(t, p.Register("a", w.addr, w))
		id, _, err := p.Create(context.Background(), "offer", "a")
		require.NoError(t, err)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		c.stateMu.Lock()
		c.relayCapable = true
		c.stateMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		response := httptest.NewRecorder()
		p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/calls/"+id+"/ice-restart", strings.NewReader("offer")).WithContext(ctx))
		require.Equal(t, http.StatusBadRequest, response.Code)
		c.relayEpisodeMu.Lock()
		episode := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		require.Nil(t, episode, "one-relay requests must not install a watchdog")
		_, err = store.Get(context.Background(), id)
		require.NoError(t, err)
		journal, err := store.(sessionstore.ControlStore).ControlIntents(context.Background())
		require.NoError(t, err)
		require.Empty(t, journal)
		// Preserve the old request-validation result even during managed rebuild.
		require.NoError(t, p.EnableRestart())
		response = httptest.NewRecorder()
		p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/calls/"+id+"/ice-restart", strings.NewReader("offer")))
		require.Equal(t, http.StatusBadRequest, response.Code)
	})
}

type unavailableRelayWorker struct{ *relayProtocolWorker }

func (w *unavailableRelayWorker) ConfigureRelays(context.Context, []mediaworker.RelaySlot) error {
	return mediaworker.ErrClosed
}

func TestRelayLossSkipsOnlySettledDeadWorker(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		p.mu.Lock()
		b := p.workers["b"]
		b.worker = &unavailableRelayWorker{b.worker.(*relayProtocolWorker)}
		p.mu.Unlock()
		require.ErrorIs(t, p.loseRelay(context.Background(), "r1"), mediaworker.ErrClosed, "unfenced worker still requires cut-off")
		p.mu.Lock()
		expireObservedWorker(t, b)
		p.mu.Unlock()
		require.True(t, p.recoverWorker(context.Background(), b, time.Now()), "empty worker recovery fences its registration")
		p.mu.Lock()
		b.observation.recoveryFinished(true)
		p.mu.Unlock()
		require.NoError(t, p.loseRelay(context.Background(), "r1"))
		_, assignment := waitRelayPending(t, store, id)
		require.Equal(t, "r2", assignment.Pending)
	})
}

type enrolledRelayWorker struct {
	*mediaworker.Worker
	configured atomic.Bool
}

func (w *enrolledRelayWorker) ConfigureRelays(ctx context.Context, slots []mediaworker.RelaySlot) error {
	err := w.Worker.ConfigureRelays(ctx, slots)
	if err == nil {
		w.configured.Store(true)
	}
	return err
}

// A real fresh worker starts with the old bootstrap leg, after loss has already
// shrunk the set. Register must configure it before making it eligible.
func TestLateRelayWorkerStartsWithSurvivingSet(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "register"
		if replace {
			name = "replace"
		}
		t.Run(name, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, _, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				require.NoError(t, p.loseRelay(context.Background(), "r1"))
				_, _ = waitRelayPending(t, store, id)
				p.mu.Lock()
				old, live := p.relaySet["r1"].Slot, p.relaySet["r2"].Slot
				p.mu.Unlock()
				worker, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: old.Leg, PublicAddr: old.Public}})
				require.NoError(t, err)
				w := &enrolledRelayWorker{Worker: worker}
				defer w.Close()
				if replace {
					p.mu.Lock()
					b := p.workers["b"]
					expireObservedWorker(t, b)
					b.observation.recoveryFinished(true) // no sessions on this fenced registration
					p.mu.Unlock()
					require.NoError(t, p.Replace("b", w.LocalAddr(), w))
				} else {
					require.NoError(t, p.Register("late", w.LocalAddr(), w))
				}
				require.True(t, w.configured.Load(), "registration must initialize relays before eligibility")
				// Fresh workers initialized from the current view can acknowledge retries.
				require.NoError(t, w.ConfigureRelays(context.Background(), []mediaworker.RelaySlot{live}))
				require.Error(t, w.ConfigureRelays(context.Background(), []mediaworker.RelaySlot{old, live}), "lost leg must never be readmitted")
				require.NoError(t, p.loseRelay(context.Background(), "r1"))
			})
		})
	}
}

func TestRelayEpisodeExpiresAfterSameAuthorityReturns(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		guard := *p.restart.guard.Load()
		c, err := p.lookup(id)
		require.NoError(t, err)
		// Put the local authority gap over the fixed episode deadline. Renewal
		// of this same token/epoch must not require reconstructing the call.
		expired := guard
		expired.deadline = time.Now().Add(-time.Millisecond)
		p.restart.guard.Store(&expired)
		episode, err := p.relayRecoveryEpisode(context.Background(), c, time.Now().Add(-relayRecoveryLimit+50*time.Millisecond))
		require.NoError(t, err)
		timer := time.NewTimer(time.Until(episode.deadline) + 100*time.Millisecond)
		<-timer.C
		c.stateMu.Lock()
		reason := c.relayEndReason
		c.stateMu.Unlock()
		require.Empty(t, reason)
		guard.deadline = time.Now().Add(time.Second)
		p.restart.guard.Store(&guard)
		require.ErrorIs(t, p.startRelayRecovery(context.Background(), c, true), sessionstore.ErrEnded, "expired context must not restart a no-op driver")
		require.Eventually(t, func() bool {
			c.stateMu.Lock()
			defer c.stateMu.Unlock()
			return c.relayEndReason == "relay_change_failed"
		}, 500*time.Millisecond, time.Millisecond)
		require.ErrorIs(t, p.startRelayRecovery(context.Background(), c, true), sessionstore.ErrEnded)
	})
}

func TestRelayEpisodeRetriesPreviouslyTriedLiveSlot(t *testing.T) {
	for _, refuse := range []bool{true, false} {
		name := "checks-dropped"
		if refuse {
			name = "offer-refused"
		}
		t.Run(name, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p, _, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				p.mu.Lock()
				binding := p.relaySet["r1"]
				binding.lost = true
				p.relaySet["r1"] = binding
				p.mu.Unlock()
				c, err := p.lookup(id)
				require.NoError(t, err)
				require.NoError(t, p.startRelayRecovery(context.Background(), c, false))
				_, first := waitRelayPending(t, store, id)
				c.relayEpisodeMu.Lock()
				deadline := c.relayEpisode.deadline
				c.relayEpisodeMu.Unlock()
				bound := 5 * time.Second // includes the production four-second check timeout
				if refuse {
					c.stateMu.Lock()
					c.relayRejected = true
					c.stateMu.Unlock()
					bound = time.Second
				}
				require.Eventually(t, func() bool {
					_, a, err := store.(sessionstore.RelayAssignments).RelayOwner(context.Background(), id)
					return err == nil && a.Pending == "r2" && a.Generation > first.Generation
				}, bound, time.Millisecond, "a transient refusal or dropped checks must permit the only surviving slot to retry")
				c.relayEpisodeMu.Lock()
				after := c.relayEpisode.deadline
				c.relayEpisodeMu.Unlock()
				require.Equal(t, deadline, after)
			})
		})
	}
}

type lossWorkStore struct {
	sessionstore.Store
	sessionstore.RelayAssignments
	sessionstore.ControlStore
	slow      atomic.Bool
	lists     atomic.Int64
	cuts      atomic.Int64
	beforeCut atomic.Int64
	first     chan time.Time
}

func (s *lossWorkStore) pause(ctx context.Context) error {
	if !s.slow.Load() {
		return nil
	}
	if s.cuts.Load() != 2 {
		s.beforeCut.Add(1)
	}
	timer := time.NewTimer(4 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *lossWorkStore) RelayOwner(ctx context.Context, id string) (sessionstore.Lease, sessionstore.RelayAssignment, error) {
	if err := s.pause(ctx); err != nil {
		return sessionstore.Lease{}, sessionstore.RelayAssignment{}, err
	}
	return s.RelayAssignments.RelayOwner(ctx, id)
}
func (s *lossWorkStore) ControlIntents(ctx context.Context) ([]sessionstore.ControlIntent, error) {
	s.lists.Add(1)
	if err := s.pause(ctx); err != nil {
		return nil, err
	}
	return s.ControlStore.ControlIntents(ctx)
}
func (s *lossWorkStore) CASRelay(ctx context.Context, lease sessionstore.Lease, from, to sessionstore.RelayAssignment) error {
	if s.slow.Load() && to.Pending != "" {
		select {
		case s.first <- time.Now():
		default:
		}
	}
	return s.RelayAssignments.CASRelay(ctx, lease, from, to)
}

type lossCutWorker struct {
	*relayProtocolWorker
	cuts *atomic.Int64
}

func (w *lossCutWorker) ConfigureRelays(ctx context.Context, slots []mediaworker.RelaySlot) error {
	if err := w.relayProtocolWorker.ConfigureRelays(ctx, slots); err != nil {
		return err
	}
	w.cuts.Add(1)
	return nil
}

func TestRelayLossDispatchScalesToTwoHundredCalls(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		measured := &lossWorkStore{Store: store, RelayAssignments: store.(sessionstore.RelayAssignments), ControlStore: store.(sessionstore.ControlStore), first: make(chan time.Time, 1)}
		p, a, _, _ := relayFailurePlane(t, measured)
		stopRelayRecoveries(t, p)
		for _, w := range p.workers {
			w.worker = &lossCutWorker{relayProtocolWorker: w.worker.(*relayProtocolWorker), cuts: &measured.cuts}
		}
		for range 199 {
			id, _, lease, err := a.CreateSessionAtRelay(context.Background(), "offer", "r1")
			require.NoError(t, err)
			p.calls[id] = &call{id: id, lease: lease, relayCapable: true, relaySlot: "r1", relayWake: make(chan struct{}, 1)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		measured.lists.Store(0)
		measured.slow.Store(true)
		defer measured.slow.Store(false)
		started := time.Now()
		done := make(chan error, 1)
		go func() { done <- p.loseRelay(ctx, "r1") }()
		select {
		case first := <-measured.first:
			gap := first.Sub(started)
			t.Logf("LOSS_SCALE calls=200 loss_to_first_dispatch=%s journal_lists=%d", gap, measured.lists.Load())
			require.Less(t, gap, 500*time.Millisecond)
		case <-ctx.Done():
			t.Fatal("no relay recovery dispatch")
		}
		require.NoError(t, <-done)
		require.Zero(t, measured.beforeCut.Load(), "cut-off acknowledgements must precede per-call/journal reads")
		require.LessOrEqual(t, measured.lists.Load(), int64(1), "one journal snapshot per loss batch")
	})
}

func TestWorkerEnrollmentDoesNotWaitForUnrelatedCreate(t *testing.T) {
	for _, configured := range []bool{false, true} {
		name := "single-relay"
		if configured {
			name = "relay-set"
		}
		t.Run(name, func(t *testing.T) {
			forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
				p := New(&fakeRelay{}, store)
				a := &relayProtocolWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:17001"), running: map[string]bool{}}}
				if configured {
					p, a, _, _ = relayFailurePlane(t, store)
				} else {
					require.NoError(t, p.Register("a", a.addr, a))
				}
				stopRelayRecoveries(t, p)
				a.createStarted, a.createGate = make(chan struct{}), make(chan struct{})
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				created, registered := make(chan struct{}), make(chan error, 1)
				go func() { defer close(created); _, _, _ = p.Create(ctx, "offer", "a") }()
				<-a.createStarted
				late := &relayProtocolWorker{fakeWorker: &fakeWorker{store: store, addr: netip.MustParseAddrPort("127.0.0.1:17003"), running: map[string]bool{}}}
				go func() { registered <- p.Register("late", late.addr, late) }()
				gate := a.createGate
				var release sync.Once
				drain := func() { cancel(); release.Do(func() { close(gate) }); <-created }
				t.Cleanup(drain)
				select {
				case err := <-registered:
					require.NoError(t, err)
				case <-time.After(150 * time.Millisecond):
					drain()
					require.NoError(t, <-registered)
					t.Fatal("registration waited for an unrelated blocked create")
				}
			})
		})
	}
}
