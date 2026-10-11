package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/relais/internal/privateapi"
	"github.com/stretchr/testify/require"
)

func TestReplayLostHTTPReplyReturnsReceipt(t *testing.T) {
	sys := startTestRelay(t, Config{Buffer: &BufferConfig{}})
	a, b := sys.worker(t, sessionA), sys.worker(t, sessionB)
	caller := newTestCaller(t)
	sys.connect(t, caller, a, sessionA)
	for _, seq := range []uint16{11, 12} {
		p := bufferRTP(t, 42, seq)
		caller.send(t, p, sys.relay.PublicAddr())
		a.expect(t, caller.addr(), p)
	}
	ctx := context.Background()
	inbound := map[uint32]uint64{42: 10}
	_, err := sys.relay.BeginReplay(ctx, sessionA, a.addr(), inbound)
	require.NoError(t, err)
	transferTestLease(t, sys.cfg.Owners, sessionA, b.addr())
	require.NoError(t, sys.relay.MoveSession(sessionA, a.addr(), b.addr()))
	handler := sys.relay.PrivateHandler()
	var lost atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/sessions/"+sessionA+"/replay" && lost.CompareAndSwap(false, true) {
			// Finish replay, then lose the HTTP reply before it reaches the client.
			handler.ServeHTTP(httptest.NewRecorder(), req)
			conn, _, err := rw.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		handler.ServeHTTP(rw, req)
	}))
	defer server.Close()
	var first ReplayResult
	err = privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/replay", ReplayRequest{To: b.addr()}, &first, RemoteErrors)
	require.ErrorIs(t, err, privateapi.ErrUncertain)
	for _, seq := range []uint16{11, 12} {
		b.expect(t, caller.addr(), bufferRTP(t, 42, seq))
	}
	caller.send(t, bufferRTP(t, 42, 13), sys.relay.PublicAddr())
	b.expect(t, caller.addr(), bufferRTP(t, 42, 13))
	var plan ReplayPlan
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/begin-replay", ReplayRequest{From: a.addr(), Inbound: inbound}, &plan, RemoteErrors))
	var receipt ReplayResult
	require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/sessions/"+sessionA+"/replay", ReplayRequest{To: b.addr()}, &receipt, RemoteErrors))
	require.Equal(t, 2, receipt.Packets)
	require.True(t, receipt.Complete)
	require.True(t, plan.Complete)
	b.expectNothing(t)
	require.Zero(t, sys.relay.Stats().Holds)
	caller.send(t, bufferRTP(t, 42, 14), sys.relay.PublicAddr())
	b.expect(t, caller.addr(), bufferRTP(t, 42, 14))
}
