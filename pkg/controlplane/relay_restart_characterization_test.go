package controlplane

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/relaylease"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Long leases deliberately remain valid when renewal stops. Expiry and the
// control-loss decision must not be prerequisites for choosing a survivor.
func TestCallerRelayRestartSelectionCharacterization(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		renew time.Duration
	}{{"alive", relaylease.DefaultRenew}, {"alive-with-jitter", 2 * relaylease.DefaultRenew}, {"renewal-stopped-before-loss", 0}} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				healthy := scenario.renew > 0
				store := sessionstore.NewMemory()
				p, _, _, id := relayFailurePlane(t, store)
				stopRelayRecoveries(t, p)
				renewRestartRelay(t, store, healthy, scenario.renew)
				c, err := p.lookup(id)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				answered := make(chan error, 1)
				go func() { _, err := p.callerRelayRestart(ctx, c); answered <- err }()
				_, pending := waitRelayPending(t, store, id)
				want := "r2"
				if healthy {
					want = "r1"
				}
				require.Equal(t, want, pending.Pending)
				require.False(t, p.relaySet["r1"].lost, "a caller request cannot declare relay loss")
				commitPending(t, store, id)
				require.NoError(t, <-answered)
			})
		})
	}
}

func renewRestartRelay(t *testing.T, store sessionstore.RelayLeases, healthy bool, interval ...time.Duration) {
	t.Helper()
	lease, err := store.ClaimRelay(t.Context(), "r1", sessionstore.RelayProcess{Owner: "r1-first", PID: 1, Start: "test", Host: "test", Namespace: "test", Executable: "test"}, time.Minute)
	require.NoError(t, err)
	if !healthy {
		return
	}
	ctx, cancel := context.WithCancel(t.Context())
	renew := relaylease.DefaultRenew
	if len(interval) > 0 {
		renew = interval[0]
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(renew)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				lease, _ = store.RenewRelay(ctx, lease, time.Minute)
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func TestUnverifiedOriginalWithoutSurvivorWaitsSafely(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := sessionstore.NewMemory()
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		renewRestartRelay(t, store, false)
		delete(p.relaySet, "r2")
		c, err := p.lookup(id)
		require.NoError(t, err)
		episode, err := p.relayRecoveryEpisode(t.Context(), c, time.Now())
		require.NoError(t, err)
		<-episode.armed
		tried := map[string]bool{}
		require.ErrorIs(t, p.recoverRelay(t.Context(), c, true, tried, episode, ""), ErrNoTarget)
		require.Empty(t, tried, "no unverified slot is attempted")
		_, assignment, err := store.RelayOwner(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1"}, assignment)
		_, rec, err := store.RelayConfirmation(t.Context(), id)
		require.NoError(t, err)
		require.Nil(t, rec.Attempt)
		require.False(t, p.relaySet["r1"].lost)
		require.True(t, episodeOf(t, store, id).Open(), "the existing fixed deadline still owns recovery")
	})
}

type renewalProbeStore struct {
	*sessionstore.Memory
	lease sessionstore.RelayLease
	err   error
	next  func(sessionstore.RelayLease) (sessionstore.RelayLease, error)
}

func (s *renewalProbeStore) GetRelay(context.Context, string) (sessionstore.RelayLease, error) {
	if s.next == nil {
		return s.lease, s.err
	}
	next := s.next
	s.next = nil
	first := s.lease
	s.lease, s.err = next(s.lease)
	return first, nil
}

func TestSameSlotRequiresRenewalOfAdmittedInstance(t *testing.T) {
	for _, change := range []string{"renewal", "unchanged", "expired", "epoch", "holder", "forwarder", "predecessor", "missing", "read-error"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := &renewalProbeStore{Memory: sessionstore.NewMemory()}
				p, _, _, _ := relayFailurePlane(t, store)
				store.lease = sessionstore.RelayLease{Epoch: 1, Holder: sessionstore.RelayProcess{Owner: "r1-first"}, ExpiresAt: time.Now().Add(time.Minute)}
				store.next = func(l sessionstore.RelayLease) (sessionstore.RelayLease, error) {
					l.ExpiresAt = l.ExpiresAt.Add(time.Second)
					switch change {
					case "unchanged":
						l.ExpiresAt = l.ExpiresAt.Add(-time.Second)
					case "expired":
						l.Expired = true
					case "epoch":
						l.Epoch++
					case "holder":
						l.Holder.Owner = "replacement"
					case "forwarder":
						l.Forwarder.Owner = "replacement"
					case "predecessor":
						l.PreviousHolder.Owner = "replacement"
					case "missing":
						return sessionstore.RelayLease{}, sessionstore.ErrNotFound
					case "read-error":
						return sessionstore.RelayLease{}, sessionstore.ErrTransient
					}
					return l, nil
				}
				want := uint64(0)
				if change == "renewal" {
					want = 1
				}
				require.Equal(t, want, p.verifyRelayRenewal(t.Context(), "r1"))
				require.False(t, p.relaySet["r1"].lost)
			})
		})
	}
}

func TestCallerRestartJoinsControlLossCharacterization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := sessionstore.NewMemory()
		p, _, _, id := relayFailurePlane(t, store)
		stopRelayRecoveries(t, p)
		c, err := p.lookup(id)
		require.NoError(t, err)
		require.NoError(t, p.loseRelay(t.Context(), "r1", 1))
		_, pending := waitRelayPending(t, store, id)
		require.Equal(t, "r2", pending.Pending)
		waitAttempt(t, store, id, 1)
		before := attemptOf(t, store, id)
		event, err := p.callerRelayRestart(t.Context(), c)
		require.NoError(t, err)
		require.Equal(t, before.Attempt.Generation, event.Generation)
		require.Equal(t, before.Episode.Token, attemptOf(t, store, id).Episode.Token)
		commitPending(t, store, id)
	})
}
