package controlplane

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Inject the decoder's typed failure at the control/store seam. Raw corrupt
// bytes and the conditional clear are covered by the sessionstore contract.
type invalidConfirmationStore struct {
	relayStore
	mu                sync.Mutex
	id                string
	initialMode       string
	invalidOnProposal bool
	proposalToken     string
	readErr           error
	clearErr          error
	putErr            error
}

func (s *invalidConfirmationStore) RelayConfirmation(ctx context.Context, id string) (sessionstore.RelayAssignment, sessionstore.RelayConfirmation, error) {
	s.mu.Lock()
	readErr, mode := s.readErr, s.initialMode
	s.mu.Unlock()
	if id == s.id && readErr != nil {
		return sessionstore.RelayAssignment{}, sessionstore.RelayConfirmation{}, readErr
	}
	a, rec, err := s.relayStore.RelayConfirmation(ctx, id)
	if id == s.id && err == nil && mode != "" {
		rec.Mode = mode
	}
	return a, rec, err
}

func (s *invalidConfirmationStore) ClearInvalidRelayConfirmation(ctx context.Context, id string, notAfter time.Time) error {
	s.mu.Lock()
	clearErr := s.clearErr
	s.mu.Unlock()
	if clearErr != nil {
		return clearErr
	}
	if err := s.relayStore.ClearInvalidRelayConfirmation(ctx, id, notAfter); err != nil {
		return err
	}
	s.mu.Lock()
	s.readErr, s.initialMode = nil, ""
	s.mu.Unlock()
	return nil
}

func (s *invalidConfirmationStore) PutControlIntent(ctx context.Context, intent sessionstore.ControlIntent) error {
	s.mu.Lock()
	putErr := s.putErr
	s.mu.Unlock()
	if putErr != nil {
		return putErr
	}
	if err := s.relayStore.PutControlIntent(ctx, intent); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if intent.Lease.SessionID == s.id && intent.Kind == relayIntentKind && s.invalidOnProposal {
		s.invalidOnProposal = false
		s.proposalToken = intent.Token
		s.readErr = sessionstore.ErrRelayConfirmationInvalid
	}
	return nil
}

func TestRestartDiscardsUndecodableConfirmation(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, backing sessionstore.Store) {
		ctx := context.Background()
		store := &invalidConfirmationStore{relayStore: backing.(relayStore), id: "bad", readErr: sessionstore.ErrRelayConfirmationInvalid}
		addr := netip.MustParseAddrPort("127.0.0.1:17001")
		bad, err := store.Claim(ctx, "bad", addr, time.Minute)
		require.NoError(t, err)
		healthy, err := store.Claim(ctx, "healthy", addr, time.Minute)
		require.NoError(t, err)
		require.NoError(t, store.CASRelayConfirmation(ctx, healthy.SessionID, sessionstore.RelayAssignment{}, sessionstore.RelayConfirmation{}, sessionstore.RelayConfirmation{Mode: sessionstore.RelayConfirmDecode}, time.Time{}, time.Time{}))
		intent := sessionstore.ControlIntent{ID: "relay-recovery:bad", Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now(), Lease: bad, Seq: 1}
		require.NoError(t, store.PutControlIntent(ctx, intent))
		p := New(&fakeRelay{}, store)
		require.NoError(t, p.Register("a", addr, &fakeWorker{store: store, addr: addr, running: map[string]bool{}}))
		stopRelayRecoveries(t, p)
		authorizeTestControl(t, p)
		p.SetRelayReady(true)
		require.NoError(t, p.rebuild(ctx, nil), "one invalid confirmation must not stall the pool")
		c, err := p.lookup(healthy.SessionID)
		require.NoError(t, err)
		mode, err := p.relayMode(ctx, c)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayConfirmDecode, mode)
		c, err = p.lookup(bad.SessionID)
		require.NoError(t, err)
		mode, err = p.relayMode(ctx, c)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayConfirmNone, mode)
		require.False(t, c.relayCapable)
		require.False(t, p.callGone(c), "discarding corrupt metadata does not end the call")
		_, rec, err := store.RelayConfirmation(ctx, bad.SessionID)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayConfirmation{}, rec)
		intents, err := store.ControlIntents(ctx)
		require.NoError(t, err)
		require.Empty(t, intents)
	})
}

func TestBeginDiscardsUndecodableConfirmation(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, backing sessionstore.Store) {
		ctx := context.Background()
		store := &invalidConfirmationStore{relayStore: backing.(relayStore), id: "bad", readErr: sessionstore.ErrRelayConfirmationInvalid}
		lease, err := store.Claim(ctx, store.id, netip.MustParseAddrPort("127.0.0.1:17001"), time.Minute)
		require.NoError(t, err)
		p := New(&fakeRelay{}, store)
		authorizeTestControl(t, p)
		c := p.rebuildCall(lease)
		c.relayMode, c.relayCapable = sessionstore.RelayConfirmDecode, true
		intent := sessionstore.ControlIntent{ID: "relay-recovery:bad", Token: randomEpisodeToken(), Kind: relayIntentKind, Started: time.Now(), Lease: lease, Seq: 1}
		require.NoError(t, store.PutControlIntent(ctx, intent))
		e := newRelayEpisode(intent)
		require.NoError(t, store.CASRelay(ctx, lease, sessionstore.RelayAssignment{}, sessionstore.RelayAssignment{Slot: "r1"}))
		store.readErr = errors.New("temporary read failure")
		require.Equal(t, relayUncertain, p.beginRelayEpisode(ctx, c, e))
		store.readErr, store.clearErr = sessionstore.ErrRelayConfirmationInvalid, sessionstore.ErrTransient
		require.Equal(t, relayUncertain, p.beginRelayEpisode(ctx, c, e), "a failed cleanup must retry")
		store.clearErr, store.putErr = nil, sessionstore.ErrTransient
		require.Equal(t, relayUncertain, p.beginRelayEpisode(ctx, c, e), "a failed unsupported proposal write must retry after the record was cleared")
		store.putErr = nil
		require.Equal(t, relayArmed, p.beginRelayEpisode(ctx, c, e), "decode failure arms the unsupported path")
		require.True(t, e.unsupported())
		require.Equal(t, intent.Started, e.proposal().Started)
		_, rec, err := store.RelayConfirmation(ctx, lease.SessionID)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayConfirmation{}, rec)
		require.Equal(t, sessionstore.RelayConfirmNone, c.relayMode)
		require.False(t, c.relayCapable)
		require.False(t, p.callGone(c))
		intents, err := store.ControlIntents(ctx)
		require.NoError(t, err)
		require.Len(t, intents, 1)
		require.Equal(t, relayIntentKindUnsupported, intents[0].Kind)
		require.Equal(t, intent.Token, intents[0].Token)
	})
}

func TestRelayLossClearsUndecodableConfirmation(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		testRelayLossInvalidConfirmation(t, store, false)
	})
}

func TestRelayLossClearsConfirmationCorruptedBeforeBegin(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		testRelayLossInvalidConfirmation(t, store, true)
	})
}

func testRelayLossInvalidConfirmation(t *testing.T, backing sessionstore.Store, afterProposal bool) {
	t.Helper()
	ctx := context.Background()
	p, worker, _, healthyID := relayFailurePlane(t, backing)
	stopRelayRecoveries(t, p)
	badID, _, lease, err := worker.CreateSessionAtRelay(ctx, "offer", "r1")
	require.NoError(t, err)
	bad := &call{id: badID, lease: lease, relaySlot: "r1", relayMode: sessionstore.RelayConfirmDelivery, relayCapable: true, relayWake: make(chan struct{}, 1)}
	store := &invalidConfirmationStore{relayStore: backing.(relayStore), id: badID, initialMode: sessionstore.RelayConfirmDelivery, invalidOnProposal: afterProposal}
	if !afterProposal {
		store.readErr = sessionstore.ErrRelayConfirmationInvalid
	}
	p.store = store
	p.mu.Lock()
	p.calls[badID] = bad
	binding := p.relaySet["r1"]
	binding.lostAt = time.Now().Add(2*time.Second - relayRecoveryLimit)
	p.relaySet["r1"] = binding
	p.mu.Unlock()
	events := watchEvents(t, bad)
	var episode *relayEpisode
	func() {
		bad.mu.Lock() // Force the watchdog to publish before a driver can end the call.
		defer bad.mu.Unlock()
		require.NoError(t, p.loseRelay(ctx, "r1"), "an invalid call must not force per-tick cut-off retries")
		require.Eventually(t, func() bool {
			bad.relayEpisodeMu.Lock()
			defer bad.relayEpisodeMu.Unlock()
			episode = bad.relayEpisode
			return episode != nil && episode.unsupported()
		}, 5*time.Second, time.Millisecond)
		require.Equal(t, binding.lostAt, episode.proposal().Started, "conversion retains the first detection")
		require.Equal(t, binding.lostAt.Add(relayRecoveryLimit), episode.deadline)
		if afterProposal {
			store.mu.Lock()
			originalToken := store.proposalToken
			store.mu.Unlock()
			require.Equal(t, originalToken, episode.proposal().Token, "conversion retains the proposed token")
		}
		_, assignment, err := store.RelayOwner(ctx, badID)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1"}, assignment, "mode none never starts a relay change")
		intents, err := store.ControlIntents(ctx)
		require.NoError(t, err)
		var found bool
		for _, intent := range intents {
			if intent.Lease.SessionID == badID {
				found = true
				require.Equal(t, relayIntentKindUnsupported, intent.Kind)
				require.WithinDuration(t, binding.lostAt, intent.Started, time.Millisecond)
			}
		}
		require.True(t, found, "the unsupported proposal is durable")
		require.Eventually(t, func() bool {
			_, rec, err := store.RelayConfirmation(ctx, healthyID)
			return err == nil && rec.Attempt != nil
		}, 5*time.Second, time.Millisecond)
		committed := commitPending(t, store, healthyID)
		require.Equal(t, "r2", committed.Slot)
		require.Eventually(t, func() bool {
			a, rec, err := store.RelayConfirmation(ctx, healthyID)
			return err == nil && rec.Confirmed(a)
		}, 5*time.Second, time.Millisecond, "the healthy call on the same lost slot still recovers")
		awaitEvent(t, events, "published")
		require.False(t, time.Now().Before(episode.deadline))
		require.Equal(t, "relay_lost", endReason(bad), "mode none publishes the ordinary relay-loss reason")
		mode, err := p.relayMode(ctx, bad)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayConfirmNone, mode)
		_, rec, err := store.RelayConfirmation(ctx, badID)
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayConfirmation{}, rec)
	}()
	awaitDone(t, episode)
	require.True(t, p.callGone(bad))
	require.Equal(t, "relay_lost", endReason(bad))
	require.False(t, worker.runs(badID))
	require.True(t, worker.runs(healthyID))
	healthy, err := p.lookup(healthyID)
	require.NoError(t, err)
	require.False(t, p.callGone(healthy))
	require.Empty(t, endReason(healthy))
	require.Eventually(t, func() bool {
		intents, err := store.ControlIntents(ctx)
		if err != nil {
			return false
		}
		for _, intent := range intents {
			if intent.Lease.SessionID == badID {
				return false
			}
		}
		return true
	}, 5*time.Second, time.Millisecond)
}
