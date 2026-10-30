package callharness

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// Real private HTTP and UDP boundaries, also used by the acceptance run.
func relayChangeTopology(t *testing.T, store sessionstore.Store, refuse, drop bool) (*controlplane.Plane, *mediaworker.Worker, *Harness, []*relay.Relay, *atomic.Uint64) {
	t.Helper()
	ctx := context.Background()
	var relays []*relay.Relay
	var bindings []controlplane.RelayBinding
	drops := new(atomic.Uint64)
	for _, id := range []string{"r1", "r2"} {
		r, err := relay.New(relay.Config{Owners: store, SlotID: id})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		relays = append(relays, r)
		public := r.PublicAddr()
		if drop && id == "r2" {
			sock, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, e)
			public = sock.LocalAddr().(*net.UDPAddr).AddrPort()
			done := make(chan struct{})
			go func() {
				defer close(done)
				b := make([]byte, 2048)
				for {
					n, _, e := sock.ReadFromUDP(b)
					if e != nil {
						return
					}
					if stun.IsMessage(b[:n]) {
						drops.Add(1)
					}
				}
			}()
			t.Cleanup(func() { require.NoError(t, sock.Close()); <-done })
		}
		bindings = append(bindings, controlplane.RelayBinding{Slot: mediaworker.RelaySlot{ID: id, Leg: r.WorkerAddr(), Public: public}, Relay: r})
	}
	w, err := mediaworker.New(mediaworker.Config{Relay: &mediaworker.RelayConfig{Owners: store, Addr: relays[0].WorkerAddr(), PublicAddr: relays[0].PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	handler := w.PrivateHandler()
	workerServer := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if refuse && strings.HasSuffix(r.URL.Path, "/restart") && r.Method == http.MethodPost {
			data, e := io.ReadAll(r.Body)
			require.NoError(t, e)
			var req mediaworker.RestartRequest
			require.NoError(t, json.Unmarshal(data, &req))
			if req.Slot == "r2" {
				privateapi.Error(rw, mediaworker.ErrRestartMismatch, mediaworker.RemoteErrors)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(data)))
		}
		handler.ServeHTTP(rw, r)
	}))
	t.Cleanup(workerServer.Close)
	p := controlplane.New(relays[0], store)
	require.NoError(t, p.Register("0", w.LocalAddr(), &controlplane.RemoteWorker{URL: workerServer.URL}))
	for _, r := range relays {
		r.AddWorker(w.LocalAddr())
	}
	require.NoError(t, p.ConfigureRelaySet(ctx, bindings))
	server := httptest.NewServer(p.Handler())
	t.Cleanup(server.Close)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: relays[0].PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	return p, w, h, relays, drops
}

func TestPlannedRelayRollbackKeepsCall(t *testing.T) {
	for _, fault := range []string{"offer-refused", "checks-dropped"} {
		t.Run(fault, func(t *testing.T) {
			forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
				p, w, h, relays, drops := relayChangeTopology(t, store, fault == "offer-refused", fault == "checks-dropped")
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
				require.NoError(t, err)
				sent := make(chan error, 1)
				go func() { sent <- call.SendMedia(ctx, 14*time.Second) }()
				time.Sleep(time.Second)
				require.Error(t, p.ChangeRelay(ctx, call.SessionID(), "r2"), "failed change must report rollback")
				require.Eventually(t, func() bool {
					_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, call.SessionID())
					return e == nil && a.Slot == "r1" && a.Pending == "" && a.Generation >= 2
				}, 2*time.Second, 10*time.Millisecond, "rollback must complete a higher-generation restart on the old slot")
				require.Eventually(t, func() bool { return call.selectedRemoteAddr() == relays[0].PublicAddr().String() }, time.Second, 10*time.Millisecond)
				before := call.rec.report()
				time.Sleep(500 * time.Millisecond)
				after := call.rec.report()
				for _, a := range after.Tracks {
					for _, b := range before.Tracks {
						if a.Kind == b.Kind {
							require.Greater(t, a.Packets, b.Packets, "caller media must progress after rollback")
						}
					}
				}
				if fault == "checks-dropped" {
					require.Positive(t, drops.Load())
				}
				require.NoError(t, <-sent)
				failures, err := w.SessionDecryptFailures(call.SessionID())
				require.NoError(t, err)
				require.Zero(t, failures)
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Zero(t, report.DecryptionFailures.Total())
				for _, track := range report.Tracks {
					t.Logf("ROLLBACK_GAP fault=%s track=%s caller_gap=%s", fault, track.Kind, track.MediaGap)
					require.Zero(t, track.DuplicatePackets)
				}
				t.Logf("ROLLBACK fault=%s generation>=2 old_slot=live auth=0 duplicates=0", fault)
			})
		})
	}
}
