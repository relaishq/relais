package controlplane_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/relaishq/relais/internal/redistestenv"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/relaishq/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

// A real caller crosses both HTTP adapters and the UDP drain barrier. The
// external harness owns no topology, and closing it must leave the system up.
func TestHTTPTopologyUsesExistingMediaPaths(t *testing.T) {
	for _, diagnostics := range []bool{false, true} {
		for _, kind := range []string{"memory", "redis"} {
			t.Run(fmt.Sprintf("%s/diagnostics-%t", kind, diagnostics), func(t *testing.T) {
				value := ""
				if diagnostics {
					value = "1"
				}
				t.Setenv("RELAIS_MOVE_DIAGNOSTICS", value)
				before := movediag.Default.Snapshot()
				var privateURLs []string
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var store sessionstore.Store = sessionstore.NewMemory()
				if kind == "redis" {
					addr := redistestenv.Address(t)
					if addr == "" {
						if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
							t.Fatal("dedicated Redis required")
						}
						t.Skip("dedicated Redis not configured")
					}
					redisStore, err := sessionstore.NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: fmt.Sprintf("move-diag:%d:", time.Now().UnixNano())}, make([]byte, 32))
					require.NoError(t, err)
					defer redisStore.Close()
					store = redisStore
				}
				r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store})
				require.NoError(t, err)
				defer func() { require.NoError(t, r.Close()) }()
				relayServer := httptest.NewServer(movediag.Handler(r.PrivateHandler()))
				defer relayServer.Close()
				privateURLs = append(privateURLs, relayServer.URL)
				remoteRelay := &controlplane.RemoteRelay{URL: relayServer.URL}
				plane := controlplane.New(remoteRelay, store)
				for _, name := range []string{"0", "1"} {
					worker, err := mediaworker.New(mediaworker.Config{ListenAddr: "127.0.0.1:0", Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}})
					require.NoError(t, err)
					defer func() { require.NoError(t, worker.Close()) }()
					server := httptest.NewServer(movediag.Handler(worker.PrivateHandler()))
					defer server.Close()
					privateURLs = append(privateURLs, server.URL)
					require.NoError(t, remoteRelay.AddWorker(ctx, worker.LocalAddr()))
					require.NoError(t, plane.Register(name, worker.LocalAddr(), &controlplane.RemoteWorker{URL: server.URL}))
				}
				server := httptest.NewServer(movediag.Handler(plane.ProcessHandler()))
				defer server.Close()
				privateURLs = append(privateURLs, server.URL)
				h, err := callharness.Start(callharness.Options{External: &callharness.ExternalTopology{SignalingURL: server.URL + "/calls", RelayAddr: r.PublicAddr()}})
				require.NoError(t, err)
				defer func() { require.NoError(t, h.Close()) }()
				require.Equal(t, r.PublicAddr(), h.RelayAddr())
				call, err := h.Dial(ctx, callharness.CallOptions{Video: true})
				require.NoError(t, err)
				require.NoError(t, call.SendMedia(ctx, time.Second))
				status, err := h.Status(ctx)
				require.NoError(t, err)
				require.Len(t, status.Calls, 1)
				res, err := plane.Move(ctx, call.SessionID(), "1")
				require.NoError(t, err)
				require.False(t, res.Result.HoldExpired)
				if diagnostics {
					require.NotEmpty(t, res.Result.MoveDiagnosticID)
					phases := map[string]int{}
					redisPhases := map[string]int{}
					for _, event := range movediag.Default.Snapshot().Events {
						if event.TraceID != res.Result.MoveDiagnosticID {
							continue
						}
						if event.Kind == "redis" {
							redisPhases[event.Phase]++
							continue
						}
						if event.Kind != "http" {
							continue
						}
						phases[event.Phase]++
						require.Greater(t, event.Duration, time.Duration(0))
						require.Equal(t, event.End.Sub(event.Start), event.Duration)
					}
					require.Equal(t, map[string]int{"barrier": 1, "export": 1, "transfer": 1, "resume": 1, "release": 1}, phases)
					if kind == "redis" {
						for _, phase := range []string{"prepare", "transfer", "resume"} {
							require.Positive(t, redisPhases[phase], phase)
						}
						require.Zero(t, redisPhases["barrier"])
						require.Zero(t, redisPhases["export"])
						require.Zero(t, redisPhases["release"])
						t.Logf("move diagnostics HTTP=%v Redis=%v", phases, redisPhases)
					}
				} else {
					require.Empty(t, res.Result.MoveDiagnosticID)
					for _, base := range privateURLs {
						response, err := http.Get(base + movediag.Endpoint)
						require.NoError(t, err)
						require.NoError(t, response.Body.Close())
						require.Equal(t, http.StatusNotFound, response.StatusCode)
					}
				}
				require.NoError(t, call.SendMedia(ctx, time.Second))
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.True(t, report.ConnectedThroughout())
				require.Equal(t, 1, report.OfferAnswerExchanges)
				require.Zero(t, report.DecryptionFailures.Total())
				require.Zero(t, report.ICERestarts)
				require.Zero(t, report.Renegotiations)
				require.NotNil(t, report.Track("audio"))
				require.NotNil(t, report.Track("video"))
				require.Positive(t, report.Track("video").Video.KeyframesDecoded)
				require.NoError(t, h.Close())
				status, err = plane.Status(ctx)
				require.NoError(t, err)
				require.Empty(t, status.Calls)
				require.NoError(t, remoteRelay.RemoveWorker(ctx, status.Workers[0].Address))
				if !diagnostics {
					require.Equal(t, before, movediag.Default.Snapshot())
				}
			})
		}
	}
}

func TestPrivateAPIsRejectBadRequestsAndPreserveErrors(t *testing.T) {
	store := sessionstore.NewMemory()
	worker, err := mediaworker.New(mediaworker.Config{ListenAddr: "127.0.0.1:0"})
	require.NoError(t, err)
	defer func() { require.NoError(t, worker.Close()) }()
	workerServer := httptest.NewServer(worker.PrivateHandler())
	defer workerServer.Close()
	remote := &controlplane.RemoteWorker{URL: workerServer.URL}
	_, _, err = remote.CreateSession(context.Background(), "not SDP")
	require.ErrorIs(t, err, mediaworker.ErrUnsupportedOffer)
	require.ErrorIs(t, remote.EndSession("missing"), mediaworker.ErrUnknownSession)
	_, err = remote.ExportSession("missing")
	require.ErrorIs(t, err, mediaworker.ErrUnknownSession)
	_, err = remote.ResumeSession([]byte("bad state"), mediaworker.ResumeOptions{})
	require.Error(t, err)
	status, err := remote.Status(context.Background())
	require.NoError(t, err)
	require.Zero(t, status.Sessions)
	r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store, BarrierTimeout: 200 * time.Millisecond})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	relayServer := httptest.NewServer(r.PrivateHandler())
	defer relayServer.Close()
	remoteRelay := &controlplane.RemoteRelay{URL: relayServer.URL}
	for _, test := range []struct{ base, path, body string }{
		{workerServer.URL, "/sessions", "{"}, {workerServer.URL, "/sessions", `{"offer":"x","unknown":true}`},
		{workerServer.URL, "/resume", `{} {}`}, {relayServer.URL, "/workers", `{}`},
		{relayServer.URL, "/sessions/call/hold", "garbage"}, {relayServer.URL, "/sessions/call/move", "garbage"}, {relayServer.URL, "/sessions/call/release", "garbage"},
		{workerServer.URL, "/sessions", strings.Repeat("x", (4<<20)+1)},
	} {
		resp, err := http.Post(test.base+test.path, "application/json", strings.NewReader(test.body))
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	_, err = remoteRelay.ReleaseSession("missing", worker.LocalAddr())
	require.ErrorIs(t, err, relay.ErrHoldExpired)
	require.Error(t, remoteRelay.HoldSession(context.Background(), "call", worker.LocalAddr()), "unregistered worker")
	require.NoError(t, remoteRelay.AddWorker(context.Background(), worker.LocalAddr()))
	// This worker sends barrier ACKs only from its configured relay leg; the
	// standalone worker ignores this private datagram, exercising HTTP timeout.
	require.ErrorIs(t, remoteRelay.HoldSession(context.Background(), "call", worker.LocalAddr()), relay.ErrBarrierTimeout)
	stats, err := remoteRelay.Status(context.Background())
	require.NoError(t, err)
	require.Zero(t, stats.Stats.Holds)
	canceled, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	before := r.Stats().BarrierTimeouts
	go func() { done <- remoteRelay.HoldSession(canceled, "canceled", worker.LocalAddr()) }()
	require.Eventually(t, func() bool { return r.Stats().Holds == 1 }, time.Second, time.Millisecond, "cancellation must exercise an active server hold")
	stop()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Eventually(t, func() bool {
		status, err := remoteRelay.Status(context.Background())
		return err == nil && status.Stats.Holds == 0
	}, time.Second, time.Millisecond, "client cancellation must release the server's hold")
	require.Equal(t, before, r.Stats().BarrierTimeouts, "cancellation, not timeout, released the hold")
	require.NoError(t, worker.Close())
	_, _, err = remote.CreateSession(context.Background(), "not SDP")
	require.Error(t, err)
}
