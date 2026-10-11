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

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRemoteWorkerWireContract(t *testing.T) {
	storedAt := time.Unix(1234567, 0).UTC()
	lease := sessionstore.Lease{SessionID: "call", Worker: netip.MustParseAddrPort("127.0.0.1:9"), Epoch: 7}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /sessions":
			var req mediaworker.CreateRequest
			require.True(t, privateapi.Read(w, r, &req))
			require.Equal(t, "offer", req.Offer)
			privateapi.Write(w, mediaworker.CreateReply{ID: "call", Answer: "answer"})
		case "DELETE /sessions/call":
			w.WriteHeader(http.StatusNoContent)
		case "POST /sessions/call/export":
			privateapi.Write(w, mediaworker.ExportReply{State: []byte("snapshot")})
		case "POST /resume":
			var req mediaworker.ResumeRequest
			require.True(t, privateapi.Read(w, r, &req))
			require.Equal(t, []byte("snapshot"), req.State)
			require.Equal(t, lease, req.Lease)
			require.EqualValues(t, 8192, req.SequenceMargin)
			require.EqualValues(t, 128, req.SRTCPIndexMargin)
			require.EqualValues(t, 10064, req.CallerSequenceReserve)
			require.Equal(t, 400*time.Millisecond, req.CheckpointAge)
			require.Equal(t, 450*time.Millisecond, req.SnapshotAge)
			require.Equal(t, storedAt, req.CheckpointStoredAt)
			privateapi.Write(w, mediaworker.ResumeReply{ID: "call"})
		case "GET /status":
			privateapi.Write(w, mediaworker.WorkerStatus{Address: lease.Worker, Sessions: 1})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	remote := &RemoteWorker{URL: server.URL}
	id, answer, err := remote.CreateSession(context.Background(), "offer")
	require.NoError(t, err)
	require.Equal(t, "call", id)
	require.Equal(t, "answer", answer)
	state, err := remote.ExportSession(id)
	require.NoError(t, err)
	require.Equal(t, []byte("snapshot"), state)
	id, err = remote.ResumeSession(state, mediaworker.ResumeOptions{Lease: lease, SequenceMargin: 8192, SRTCPIndexMargin: 128, CallerSequenceReserve: 10064, CheckpointAge: 400 * time.Millisecond, SnapshotAge: 450 * time.Millisecond, CheckpointStoredAt: storedAt})
	require.NoError(t, err)
	require.Equal(t, "call", id)
	status, err := remote.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, lease.Worker, status.Address)
	require.Equal(t, 1, status.Sessions)
	require.NoError(t, remote.EndSession(id))
}

func TestRemoteRelayWireContract(t *testing.T) {
	from, to := netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/workers":
			var req relay.WorkerRequest
			require.True(t, privateapi.Read(w, r, &req))
			require.Equal(t, to, req.Address)
		case "/sessions/call/hold", "/sessions/call/move", "/sessions/call/release":
			var req relay.RouteRequest
			require.True(t, privateapi.Read(w, r, &req))
			switch r.URL.Path {
			case "/sessions/call/hold":
				require.Equal(t, from, req.From)
			case "/sessions/call/move":
				require.Equal(t, from, req.From)
				require.Equal(t, to, req.To)
			case "/sessions/call/release":
				require.Equal(t, to, req.To)
				privateapi.Write(w, relay.ReleaseReply{Packets: 3})
				return
			}
		case "/sessions/call":
			require.Equal(t, http.MethodDelete, r.Method)
		case "/status":
			privateapi.Write(w, relay.RelayStatus{Public: from, Private: to})
			return
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	remote := &RemoteRelay{URL: server.URL}
	ctx := context.Background()
	require.NoError(t, remote.AddWorker(ctx, to))
	require.NoError(t, remote.HoldSession(ctx, "call", from))
	require.NoError(t, remote.MoveSession("call", from, to))
	n, err := remote.ReleaseSession("call", to)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	remote.ForgetSession("call")
	require.NoError(t, remote.Forget(ctx, "call"))
	require.NoError(t, remote.RemoveWorker(ctx, to))
	status, err := remote.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, from, status.Public)
	require.EqualValues(t, 8, requests.Load())
}

func TestRemoteErrorsAndTimeouts(t *testing.T) {
	for _, sentinel := range []error{mediaworker.ErrUnknownSession, mediaworker.ErrUnsupportedOffer, mediaworker.ErrClosed, mediaworker.ErrNotEstablished, mediaworker.ErrSequenceBudgetExhausted, mediaworker.ErrSRTCPIndexExhausted, sessionstore.ErrLeaseLost, relay.ErrBarrierTimeout, relay.ErrHeld, relay.ErrHoldExpired} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			codes := make(map[string]error)
			for k, v := range mediaworker.RemoteErrors {
				codes[k] = v
			}
			for k, v := range relay.RemoteErrors {
				codes[k] = v
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { privateapi.Error(w, sentinel, codes) }))
			defer server.Close()
			require.ErrorIs(t, privateapi.Do(context.Background(), server.Client(), server.URL, http.MethodPost, "/", nil, nil, codes), sentinel)
		})
	}
	for code, sentinel := range mediaworker.RemoteErrors {
		t.Run("worker/"+code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				privateapi.Error(w, sentinel, mediaworker.RemoteErrors)
			}))
			defer server.Close()
			_, err := (&RemoteWorker{URL: server.URL, Client: server.Client()}).ResumeSession(nil, mediaworker.ResumeOptions{})
			require.ErrorIs(t, err, sentinel)
		})
	}
	candidate := sessionstore.Lease{SessionID: "pending", Epoch: 42}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Error(w, &sessionstore.TransientError{Op: "put", Err: errors.New("unavailable"), Candidate: &candidate}, nil)
	}))
	_, err := (&RemoteWorker{URL: server.URL}).ResumeSession(nil, mediaworker.ResumeOptions{})
	var transient *sessionstore.TransientError
	require.ErrorAs(t, err, &transient)
	require.Equal(t, &candidate, transient.Candidate)
	server.Close()
	var attempts atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { attempts.Add(1); time.Sleep(60 * time.Millisecond) }))
	defer slow.Close()
	client := &http.Client{Timeout: 20 * time.Millisecond}
	worker := &RemoteWorker{URL: slow.URL, Client: client}
	_, _, err = worker.CreateSession(context.Background(), "offer")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, worker.EndSession("call"), context.DeadlineExceeded)
	_, err = worker.ExportSession("call")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = worker.ResumeSession(nil, mediaworker.ResumeOptions{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = worker.Status(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	r := &RemoteRelay{URL: slow.URL, Client: client}
	require.ErrorIs(t, r.HoldSession(context.Background(), "call", netip.AddrPort{}), context.DeadlineExceeded)
	require.ErrorIs(t, r.MoveSession("call", netip.AddrPort{}, netip.AddrPort{}), context.DeadlineExceeded)
	_, err = r.ReleaseSession("call", netip.AddrPort{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, r.Forget(context.Background(), "call"), context.DeadlineExceeded)
	require.ErrorIs(t, r.AddWorker(context.Background(), netip.AddrPort{}), context.DeadlineExceeded)
	require.ErrorIs(t, r.RemoveWorker(context.Background(), netip.AddrPort{}), context.DeadlineExceeded)
	_, err = r.Status(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = worker.ResumeSession(nil, mediaworker.ResumeOptions{Context: ctx})
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 12, attempts.Load(), "mutations must not retry after a lost response")
}

func TestHTTPResumeExhaustionIsTerminal(t *testing.T) {
	for _, sentinel := range []error{mediaworker.ErrSequenceBudgetExhausted, mediaworker.ErrSRTCPIndexExhausted} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			ctx := context.Background()
			p, _, _, _ := setup(t)
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				privateapi.Error(w, sentinel, mediaworker.RemoteErrors)
			}))
			defer server.Close()
			p.workers["b"].worker = &RemoteWorker{URL: server.URL, Client: server.Client()}
			id, _, err := p.Create(ctx, "offer", "a")
			require.NoError(t, err)
			lease, err := p.store.Get(ctx, id)
			require.NoError(t, err)
			require.NoError(t, p.store.PutState(ctx, lease, takeoverSnapshot(t, id, 0)))
			source := p.workers["a"]
			source.dead = true
			require.True(t, p.recoverWorker(ctx, source, time.Now()))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Len(t, status.Takeovers, 1)
			require.True(t, status.Takeovers[0].Lost)
			require.Contains(t, status.Takeovers[0].Error, sentinel.Error())
			require.NotContains(t, status.Takeovers[0].Error, "attempts exhausted")
			require.EqualValues(t, 1, attempts.Load())
			require.Empty(t, source.pending)
		})
	}
}
