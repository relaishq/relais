package controlplane_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relais/pkg/callharness"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// A real caller crosses both HTTP adapters and the UDP drain barrier. The
// external harness owns no topology, and closing it must leave the system up.
func TestHTTPTopologyUsesExistingMediaPaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store := sessionstore.NewMemory()
	r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store})
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	relayServer := httptest.NewServer(r.PrivateHandler())
	defer relayServer.Close()
	remoteRelay := &controlplane.RemoteRelay{URL: relayServer.URL}
	plane := controlplane.New(remoteRelay, store)
	for _, name := range []string{"0", "1"} {
		worker, err := mediaworker.New(mediaworker.Config{ListenAddr: "127.0.0.1:0", Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}})
		require.NoError(t, err)
		defer func() { require.NoError(t, worker.Close()) }()
		server := httptest.NewServer(worker.PrivateHandler())
		defer server.Close()
		require.NoError(t, remoteRelay.AddWorker(ctx, worker.LocalAddr()))
		require.NoError(t, plane.Register(name, worker.LocalAddr(), &controlplane.RemoteWorker{URL: server.URL}))
	}
	server := httptest.NewServer(plane.ProcessHandler())
	defer server.Close()
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
