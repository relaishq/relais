package controlplane

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

type retainedIntentStore struct {
	*sessionstore.Memory
	retain atomic.Bool
}

func (s *retainedIntentStore) DeleteControlIntent(ctx context.Context, id, token string) error {
	if s.retain.Load() {
		return sessionstore.ErrTransient
	}
	return s.Memory.DeleteControlIntent(ctx, id, token)
}

// A worker HTTP boundary backed by the existing lease-aware unit worker.
// This verifies command/intent lifecycle, not media or cryptographic recovery.
func unitWorkerHandler(worker *fakeWorker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sessions":
			id, answer, err := worker.CreateSession(r.Context(), "offer")
			if err != nil {
				privateapi.Error(w, err, mediaworker.RemoteErrors)
				return
			}
			lease, err := worker.store.Get(r.Context(), id)
			if err != nil {
				privateapi.Error(w, err, mediaworker.RemoteErrors)
				return
			}
			privateapi.Write(w, mediaworker.CreateReply{ID: id, Answer: answer, Lease: lease})
		case strings.HasSuffix(r.URL.Path, "/export"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/export")
			state, err := worker.ExportSession(id)
			if err != nil {
				privateapi.Error(w, err, mediaworker.RemoteErrors)
				return
			}
			privateapi.Write(w, mediaworker.ExportReply{State: state})
		case r.URL.Path == "/resume":
			var req mediaworker.ResumeRequest
			if !privateapi.Read(w, r, &req) {
				return
			}
			id, err := worker.ResumeSession(req.State, mediaworker.ResumeOptions{Lease: req.Lease, SequenceMargin: req.SequenceMargin})
			if err != nil {
				privateapi.Error(w, err, mediaworker.RemoteErrors)
				return
			}
			privateapi.Write(w, mediaworker.ResumeReply{ID: id})
		case r.URL.Path == "/status":
			status := mediaworker.WorkerStatus{Address: worker.addr, InventoryKnown: true}
			worker.mu.Lock()
			for id := range worker.running {
				lease, err := worker.store.Get(r.Context(), id)
				if err == nil {
					status.Inventory = append(status.Inventory, mediaworker.InstalledSession{ID: id, Owner: lease.Worker, Epoch: lease.Epoch})
				}
			}
			status.Sessions = len(status.Inventory)
			worker.mu.Unlock()
			privateapi.Write(w, status)
		default:
			http.NotFound(w, r)
		}
	})
}

func TestShutdownFinishesMoveBeforeClosingLink(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "operator", true: "rebind"}[automatic], func(t *testing.T) {
			store := &retainedIntentStore{Memory: sessionstore.NewMemory()}
			r := newPipeAdmissionRig(t, noContentHandler(), store)
			r.p.relay = &fakeRelay{}
			authorizeLongControl(t, r.p)
			r.p.SetRelayReady(true)
			source := &fakeWorker{addr: r.addr, store: store, running: make(map[string]bool), exportGate: make(chan struct{}), exported: make(chan struct{}, 1)}
			target := &fakeWorker{addr: netip.MustParseAddrPort("127.0.0.1:17003"), store: store, running: make(map[string]bool)}
			stream, err := r.open(t, "A", 1, 0, source.addr)
			require.NoError(t, err)
			peer := stream.run(unitWorkerHandler(source))
			stream, err = r.openWorker(t, "b", "B", 1, 0, target.addr)
			require.NoError(t, err)
			stream.run(unitWorkerHandler(target))
			id, _, err := r.p.Create(context.Background(), "offer", "a")
			require.NoError(t, err)
			// A failed journal clear leaves actual durable work for the successor.
			store.retain.Store(true)
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(source.exportGate) }) })
			moved := make(chan error, 1)
			if automatic {
				require.NoError(t, reportLeg(context.Background(), &http.Client{Transport: peer}, netip.MustParseAddrPort("127.0.0.1:17002")))
			} else {
				go func() { _, err := r.p.Move(context.Background(), id, "b"); moved <- err }()
			}
			receiveLinkTest(t, source.exported, "applied export")
			stopped := make(chan struct{})
			go func() { r.p.BeginShutdown(); close(stopped) }()
			select {
			case <-peer.Done():
				t.Fatal("shutdown cut the command path during an applied export")
			case <-time.After(200 * time.Millisecond):
			}
			release.Do(func() { close(source.exportGate) })
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown did not join the move")
			}
			if !automatic {
				require.NoError(t, receiveLinkTest(t, moved, "operator move completion"))
			}
			lease, err := store.Get(context.Background(), id)
			require.NoError(t, err)
			require.True(t, target.runs(id), "shutdown must finish the applied move on its target")
			require.False(t, source.runs(id), "shutdown must not roll the applied move back")
			require.Equal(t, target.addr, lease.Worker)
			intents, err := store.ControlIntents(context.Background())
			require.NoError(t, err)
			require.Len(t, intents, 1)
			require.NoError(t, r.p.ReleaseControl(context.Background()))
			store.retain.Store(false)

			next := newPipeAdmissionRig(t, noContentHandler(), store)
			next.p.relay = &fakeRelay{}
			authorizeLongControl(t, next.p)
			next.p.SetRelayReady(true)
			stream, err = next.open(t, "A", 2, 0, source.addr)
			require.NoError(t, err)
			stream.run(unitWorkerHandler(source))
			stream, err = next.openWorker(t, "b", "B", 2, 0, target.addr)
			require.NoError(t, err)
			stream.run(unitWorkerHandler(target))
			// Ready-frame delivery can precede server-side link availability.
			// Establish both surviving command paths before testing rebuild.
			for _, name := range []string{"a", "b"} {
				require.Eventually(t, func() bool {
					_, err := next.links.entries[name].remote.Status(context.Background())
					return err == nil
				}, 5*time.Second, 10*time.Millisecond, "successor link %s", name)
			}
			require.NoError(t, next.p.rebuild(context.Background(), []FixedWorker{{Name: "a", URL: "dial-out"}, {Name: "b", URL: "dial-out"}}))
			intents, err = store.ControlIntents(context.Background())
			require.NoError(t, err)
			require.Empty(t, intents)
			current, err := store.Get(context.Background(), id)
			require.NoError(t, err)
			require.Equal(t, lease.Worker, current.Worker)
			require.Equal(t, lease.Epoch, current.Epoch, "reconciliation must preserve the observed live tenure")
			state, err := next.p.Status(context.Background())
			require.NoError(t, err)
			require.Len(t, state.Calls, 1)
			require.Zero(t, state.LostCount)
		})
	}
}
