package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type admissionRelay struct {
	*fakeRelay
	slot             mediaworker.RelaySlot
	lease            sessionstore.RelayLease
	online           atomic.Bool
	entered, release chan struct{}
}

func (r *admissionRelay) Status(context.Context) (relay.RelayStatus, error) {
	if !r.online.Load() {
		return relay.RelayStatus{}, errors.New("relay unreachable")
	}
	return relay.RelayStatus{Instance: r.lease.Holder.Owner, Continuity: &relay.ContinuityStatus{Epoch: r.lease.Epoch}, Private: r.slot.Leg, Public: r.slot.Public}, nil
}
func (r *admissionRelay) AddWorker(ctx context.Context, _ netip.AddrPort) error {
	if r.entered != nil {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func newAdmissionRelays(t *testing.T, store *sessionstore.Memory, count ...int) []*admissionRelay {
	t.Helper()
	var relays []*admissionRelay
	bindings := relayTestSlots()
	if len(count) > 0 {
		addr := netip.MustParseAddrPort("127.0.0.1:18003")
		bindings = append(bindings, RelayBinding{Slot: mediaworker.RelaySlot{ID: "r3", Leg: addr, Public: addr}})
	}
	for _, binding := range bindings {
		process := sessionstore.RelayProcess{Owner: binding.Slot.ID + "-first", PID: 1, Start: "test", Host: "test", Namespace: "test", Executable: "test"}
		lease, err := store.ClaimRelay(t.Context(), binding.Slot.ID, process, time.Hour)
		require.NoError(t, err)
		r := &admissionRelay{fakeRelay: &fakeRelay{}, slot: binding.Slot, lease: lease}
		r.online.Store(true)
		relays = append(relays, r)
	}
	return relays
}
func configureAdmission(t *testing.T, p *Plane, relays []*admissionRelay) {
	t.Helper()
	var bindings []RelayBinding
	for _, r := range relays {
		bindings = append(bindings, RelayBinding{Slot: mediaworker.RelaySlot{ID: r.slot.ID}, Relay: r})
	}
	require.NoError(t, p.ConfigureRelaySet(t.Context(), bindings))
}
func syncAdmission(ctx context.Context, p *Plane, r *admissionRelay) error {
	registry := RelayWorkerRegistry{Slot: r.slot.ID}
	return registry.Sync(ctx, p, r)
}

type admissionWorker struct {
	*relayProtocolWorker
	media            *mediaworker.Worker
	entered, release chan struct{}
}

func (w *admissionWorker) ConfigureRelays(ctx context.Context, req mediaworker.RelaySetRequest) error {
	if err := w.media.ConfigureRelays(ctx, req); err != nil {
		return err
	}
	if w.entered != nil && req.Epochs["r1"] > 0 {
		close(w.entered)
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func registerAdmissionWorker(t *testing.T, p *Plane, store *sessionstore.Memory, slot mediaworker.RelaySlot) *admissionWorker {
	t.Helper()
	media, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: slot.Leg, PublicAddr: slot.Public}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, media.Close()) })
	w := &admissionWorker{media: media, relayProtocolWorker: &relayProtocolWorker{fakeWorker: &fakeWorker{store: store, addr: media.LocalAddr(), running: map[string]bool{}}}}
	var worker Worker = w
	if p.restart != nil {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /relays", func(rw http.ResponseWriter, r *http.Request) {
			var req mediaworker.RelaySetRequest
			if !privateapi.Read(rw, r, &req) {
				return
			}
			if err := w.ConfigureRelays(r.Context(), req); err != nil {
				privateapi.Error(rw, err, mediaworker.RemoteErrors)
				return
			}
			rw.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST /sessions", func(rw http.ResponseWriter, r *http.Request) {
			var req mediaworker.CreateRequest
			if !privateapi.Read(rw, r, &req) {
				return
			}
			id, answer, lease, err := w.CreateSessionAtRelay(r.Context(), req.Offer, req.RelaySlot)
			if err != nil {
				privateapi.Error(rw, err, mediaworker.RemoteErrors)
				return
			}
			privateapi.Write(rw, mediaworker.CreateReply{ID: id, Answer: answer, Lease: lease})
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		worker = &RemoteWorker{URL: server.URL, Client: server.Client()}
	}
	require.NoError(t, p.Register("w", media.LocalAddr(), worker))
	return w
}
func awaitAdmission(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		t.Fatal("pair acknowledgement did not arrive")
	}
}
func createAdmissionCall(t *testing.T, p *Plane) string {
	t.Helper()
	id, _, err := p.Create(t.Context(), "offer", "w")
	require.NoError(t, err)
	return id
}
func replaceAdmissionRelay(t *testing.T, store *sessionstore.Memory, r *admissionRelay) {
	t.Helper()
	next := r.lease.Holder
	next.Owner += "-replacement"
	lease, err := store.TransferRelay(t.Context(), r.lease, next, time.Hour)
	require.NoError(t, err)
	r.lease = lease
}

func TestPendingRelayJoinsWithoutControlRestart(t *testing.T) {
	store := sessionstore.NewMemory()
	relays := newAdmissionRelays(t, store)
	p := New(relays[0], store)
	configureAdmission(t, p, relays)
	w := registerAdmissionWorker(t, p, store, relays[0].slot)
	relays[0].online.Store(false)
	require.Error(t, syncAdmission(t.Context(), p, relays[0]))
	require.NoError(t, syncAdmission(t.Context(), p, relays[1]))
	id := createAdmissionCall(t, p)
	require.Equal(t, "r2", relayStatusOf(t, p, id).Slot)
	relays[0].entered, relays[0].release = make(chan struct{}), make(chan struct{})
	w.entered, w.release = make(chan struct{}), make(chan struct{})
	relays[0].online.Store(true)
	joined := make(chan error, 1)
	go func() { joined <- syncAdmission(t.Context(), p, relays[0]) }()
	awaitAdmission(t, relays[0].entered)
	id = createAdmissionCall(t, p)
	require.Equal(t, "r2", relayStatusOf(t, p, id).Slot, "relay registration must acknowledge first")
	close(relays[0].release)
	awaitAdmission(t, w.entered)
	id = createAdmissionCall(t, p)
	require.Equal(t, "r2", relayStatusOf(t, p, id).Slot, "worker enrollment must acknowledge too")
	close(w.release)
	select {
	case err := <-joined:
		require.NoError(t, err)
	case <-time.After(8 * time.Second):
		t.Fatal("pair did not join")
	}
	id = createAdmissionCall(t, p)
	require.Equal(t, "r1", relayStatusOf(t, p, id).Slot)
}

func TestEmptyNewRelayInstanceRejoinsAfterCompletedDrain(t *testing.T) {
	store := sessionstore.NewMemory()
	relays := newAdmissionRelays(t, store)
	p := New(relays[0], store)
	configureAdmission(t, p, relays)
	registerAdmissionWorker(t, p, store, relays[0].slot)
	for _, r := range relays {
		require.NoError(t, syncAdmission(t.Context(), p, r))
	}
	require.NoError(t, p.DrainRelay(t.Context(), "r1"))
	require.NoError(t, p.loseRelay(t.Context(), "r2", relays[1].lease.Epoch))
	p.mu.Lock()
	ready := p.readyLocked()
	p.mu.Unlock()
	require.False(t, ready, "a draining pair cannot admit new calls when the other relay is lost")
	require.Error(t, syncAdmission(t.Context(), p, relays[0]), "same-instance resume is refused")
	replaceAdmissionRelay(t, store, relays[0])
	require.NoError(t, syncAdmission(t.Context(), p, relays[0]))
	require.False(t, p.relaySet["r1"].draining)
	id := createAdmissionCall(t, p)
	require.Equal(t, "r1", relayStatusOf(t, p, id).Slot)
}

func admitControlTerm(t *testing.T, p *Plane, store *sessionstore.Memory, holder string) sessionstore.ControlLease {
	t.Helper()
	require.NoError(t, p.EnableRestart())
	lease, err := store.ClaimControl(t.Context(), holder, 30*time.Second)
	require.NoError(t, err)
	p.restart.guard.Store(&controlGuard{lease: lease, deadline: time.Now().Add(25 * time.Second)})
	p.restart.healthy.Store(true)
	p.restart.ready.Store(true)
	records, err := store.ControlRecords(t.Context(), lease)
	require.NoError(t, err)
	require.NoError(t, p.restoreRelayCutoffs(records, lease.Epoch))
	return lease
}

func TestControlRestartDuringRelayOutageKeepsEpochCutoff(t *testing.T) {
	store := sessionstore.NewMemory()
	relays := newAdmissionRelays(t, store)
	old := New(relays[0], store)
	configureAdmission(t, old, relays)
	registerAdmissionWorker(t, old, store, relays[0].slot)
	for _, r := range relays {
		require.NoError(t, syncAdmission(t.Context(), old, r))
	}
	id := createAdmissionCall(t, old)
	callLease, err := store.Get(t.Context(), id)
	require.NoError(t, err)
	_, assigned, err := store.RelayOwner(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "r1", assigned.Slot)
	relays[0].online.Store(false)
	rebuilt := New(relays[0], store)
	configureAdmission(t, rebuilt, relays)
	term := admitControlTerm(t, rebuilt, store, "rebuilt")
	registerAdmissionWorker(t, rebuilt, store, relays[0].slot)
	rebuiltCall := rebuilt.rebuildCall(callLease)
	require.Len(t, rebuilt.calls, 1)
	require.Empty(t, rebuilt.relaySet["r1"].instance, "rebuild never saw the old status token")
	require.NoError(t, rebuilt.loseRelay(t.Context(), "r1", relays[0].lease.Epoch))
	rebuiltCall.relayEpisodeMu.Lock()
	episode := rebuiltCall.relayEpisode
	rebuiltCall.relayEpisodeMu.Unlock()
	if episode != nil {
		awaitAdmission(t, episode.done)
	}
	require.True(t, rebuiltCall.terminal)
	rebuiltCall.mu.Lock()
	rebuilt.cleanupExpired(t.Context(), rebuiltCall)
	rebuiltCall.mu.Unlock()
	_, err = store.Get(t.Context(), id)
	require.ErrorIs(t, err, sessionstore.ErrEnded, "ordinary rebuilt calls follow relay-loss end")
	records, err := store.ControlRecords(t.Context(), term)
	require.NoError(t, err)
	require.JSONEq(t, "1", string(records["relay-cutoff:r1"]))
	require.NoError(t, store.ReleaseControl(t.Context(), term))
	// Every old worker is replaced. Only the control record retains the cutoff.
	next := New(relays[0], store)
	configureAdmission(t, next, relays)
	admitControlTerm(t, next, store, "next")
	registerAdmissionWorker(t, next, store, relays[0].slot)
	require.NoError(t, next.loseRelay(t.Context(), "r1", 1))
	renewed, err := store.RenewRelay(t.Context(), relays[0].lease, time.Hour)
	require.NoError(t, err)
	require.Equal(t, relays[0].lease.Epoch, renewed.Epoch)
	relays[0].online.Store(true)
	require.ErrorIs(t, syncAdmission(t.Context(), next, relays[0]), mediaworker.ErrRelayUnsupported)
	require.NoError(t, syncAdmission(t.Context(), next, relays[1]))
	require.Equal(t, "r2", relayStatusOf(t, next, createAdmissionCall(t, next)).Slot)
	replaceAdmissionRelay(t, store, relays[0])
	require.NoError(t, syncAdmission(t.Context(), next, relays[0]))
	healthyID := createAdmissionCall(t, next)
	require.Equal(t, "r1", relayStatusOf(t, next, healthyID).Slot)
	current := next.workers["w"]
	healthyLease, err := store.Get(t.Context(), healthyID)
	require.NoError(t, err)
	require.NoError(t, store.ReleaseControl(t.Context(), next.restart.guard.Load().lease))
	healthy := New(relays[0], store)
	configureAdmission(t, healthy, relays)
	admitControlTerm(t, healthy, store, "healthy-restart")
	require.NoError(t, healthy.Register("w", current.addr, current.worker))
	healthy.rebuildCall(healthyLease)
	require.NoError(t, syncAdmission(t.Context(), healthy, relays[0]), "historical cutoff cannot turn healthy current calls into losses")
	_, err = store.Get(t.Context(), healthyID)
	require.NoError(t, err)
	require.False(t, healthy.relaySet["r1"].lost)
}

func saveOpenPlannedAttempt(t *testing.T, p *Plane, id, from, target string) sessionstore.Lease {
	t.Helper()
	ctx := t.Context()
	c, err := p.lookup(id)
	require.NoError(t, err)
	require.NoError(t, p.acceptRelayMode(ctx, c, sessionstore.RelayConfirmDelivery))
	store := p.store.(sessionstore.RelayAssignments)
	lease, current, err := store.RelayOwner(ctx, id)
	require.NoError(t, err)
	require.Equal(t, from, current.Slot)
	pending := current
	pending.Pending = target
	require.NoError(t, p.setRelay(ctx, lease, current, pending))
	_, err = p.beginRelayAttempt(ctx, c, lease, pending, sessionstore.RelayConfirmDelivery, relayAttemptOptions{kind: "planned", from: from, outer: time.Now().Add(time.Second), tried: []string{target}})
	require.NoError(t, err)
	require.NoError(t, store.CommitRelay(ctx, lease, pending, sessionstore.RelayAssignment{Slot: target, Generation: 1}))
	return lease
}

func TestOpenPlannedFromBlocksOnlyItsDrainedSlotRejoin(t *testing.T) {
	for _, from := range []string{"r1", "r3"} {
		t.Run(from, func(t *testing.T) {
			store := sessionstore.NewMemory()
			relays := newAdmissionRelays(t, store, 3)
			p := New(relays[0], store)
			configureAdmission(t, p, relays)
			registerAdmissionWorker(t, p, store, relays[0].slot)
			for _, r := range relays {
				require.NoError(t, syncAdmission(t.Context(), p, r))
			}
			for id, b := range p.relaySet {
				b.draining = id != from
				p.relaySet[id] = b
			}
			id := createAdmissionCall(t, p)
			for id, b := range p.relaySet {
				b.draining = false
				p.relaySet[id] = b
			}
			saveOpenPlannedAttempt(t, p, id, from, "r2")
			require.NoError(t, p.DrainRelay(t.Context(), "r1"))
			replaceAdmissionRelay(t, store, relays[0])
			err := syncAdmission(t.Context(), p, relays[0])
			if from == "r1" {
				require.ErrorIs(t, err, ErrMoveInProgress)
			} else {
				require.NoError(t, err, "an unrelated planned move does not block rejoin")
			}
		})
	}
}

func TestPlannedAttemptFromSurvivesRestartAndDrivesRollback(t *testing.T) {
	forBudgetRetryStores(t, func(t *testing.T, store sessionstore.Store) {
		p, a, b, id := relayFailurePlane(t, store)
		lease := saveOpenPlannedAttempt(t, p, id, "r1", "r2")
		next := successorPlane(t, store, a.fakeWorker, b)
		stopRelayRecoveries(t, next)
		next.rebuildCall(lease)
		worker := next.workers["a"].worker.(*relayProtocolWorker)
		committed := make(chan sessionstore.RelayAssignment, 1)
		next.workers["a"].worker = &drainWorker{relayProtocolWorker: worker, missingSlots: []string{"r2"}, committed: committed}
		binding := next.relaySet["r1"]
		binding.draining = true
		next.relaySet["r1"] = binding
		_, rec, err := confirmations(store).RelayConfirmation(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, "r1", rec.Attempt.From)
		next.resumeRelayAttempts(t.Context())
		select {
		case to := <-committed:
			require.Equal(t, "r1", to.Slot)
		case <-time.After(8 * time.Second):
			t.Fatal("stored From did not drive rollback")
		}
		require.Eventually(t, func() bool {
			at, rec, err := confirmations(store).RelayConfirmation(t.Context(), id)
			return err == nil && at.Slot == "r1" && rec.Attempt != nil && rec.Attempt.Generation == 2 && rec.Attempt.From == "r1" && rec.Attempt.Phase == sessionstore.RelayPhaseConfirmed
		}, 8*time.Second, time.Millisecond, "stored From must drive rollback even while its slot is draining")
	})
}

func TestRelayAdmissionRequiresCurrentWorkerInstance(t *testing.T) {
	store := sessionstore.NewMemory()
	relays := newAdmissionRelays(t, store)
	p := New(relays[0], store)
	configureAdmission(t, p, relays)
	registerAdmissionWorker(t, p, store, relays[0].slot)
	require.NoError(t, syncAdmission(t.Context(), p, relays[0]))
	p.mu.Lock()
	p.workers["w"].instance = "replacement"
	ready := p.readyLocked()
	p.mu.Unlock()
	require.False(t, ready, "old worker acknowledgements cannot admit its replacement")
	require.NoError(t, syncAdmission(t.Context(), p, relays[0]))
	p.mu.Lock()
	ready = p.readyLocked()
	p.mu.Unlock()
	require.True(t, ready)
}
