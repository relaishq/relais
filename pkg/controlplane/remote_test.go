package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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

func TestRemoteWorkerRelayInputReplayWirePresence(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(stringBool(enabled), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]json.RawMessage
				require.True(t, privateapi.Read(w, r, &req))
				_, present := req["relay_input_replay"]
				require.Equal(t, enabled, present, "omit unused fields rejected by older strict resume readers")
				if enabled {
					require.JSONEq(t, "true", string(req["relay_input_replay"]))
				}
				privateapi.Write(w, mediaworker.ResumeReply{ID: "call"})
			}))
			defer server.Close()
			_, err := (&RemoteWorker{URL: server.URL}).ResumeSession(nil, mediaworker.ResumeOptions{RelayInputReplay: enabled})
			require.NoError(t, err)
		})
	}
}

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
				// Test decoding this specific wire code. A child sentinel such as
				// export_not_flushed also matches its move_budget parent.
				privateapi.Error(w, sentinel, map[string]error{code: sentinel})
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
	// A stalled first dial stands in for a loaded machine: without
	// handlerFirst the client times out before that request reaches the
	// handler, and the count below drops to 11.
	for _, stall := range []time.Duration{0, 100 * time.Millisecond} {
		t.Run("timeouts/first_dial_stall="+stall.String(), func(t *testing.T) {
			var attempts atomic.Int32
			reached := make(chan struct{}, 1)
			slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				// The server sees the client hang up only after the body is read.
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case reached <- struct{}{}:
				case <-r.Context().Done():
				}
				<-r.Context().Done()
			}))
			defer slow.Close()
			var dials atomic.Int32
			base := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					time.Sleep(stall)
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}}
			defer base.CloseIdleConnections()
			client := &http.Client{Timeout: 20 * time.Millisecond, Transport: handlerFirst{t: t, base: base, reached: reached}}
			worker := &RemoteWorker{URL: slow.URL, Client: client}
			_, _, err := worker.CreateSession(context.Background(), "offer")
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
			require.EqualValues(t, 12, attempts.Load(), "mutations must not retry after a lost response")
			// Every handler above has started, so the count is settled. This
			// client has no handlerFirst and must send nothing.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			worker.Client = &http.Client{Timeout: 20 * time.Millisecond, Transport: base}
			_, err = worker.ResumeSession(nil, mediaworker.ResumeOptions{Context: ctx})
			require.ErrorIs(t, err, context.Canceled)
			require.EqualValues(t, 12, attempts.Load(), "a cancelled call must send no request")
		})
	}
}

// handlerFirst lets the client's own timeout end a request only after the
// server handler has received it. The handler count then equals the
// requests sent, whatever the load: a slow dial delays the request but
// cannot drop it. The client still sees its own timeout error, so the
// request must end only through that timeout: any other transport result,
// such as a reset or EOF, fails the test here, before http.Client can
// rewrite it as a timeout.
type handlerFirst struct {
	t       testing.TB
	base    http.RoundTripper
	reached <-chan struct{}
}

func (h handlerFirst) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(req.Context()))
	defer cancel()
	forwarded := req.WithContext(ctx)
	//nolint:staticcheck // SA1019: http.Client also signals its timeout on Cancel.
	forwarded.Cancel = nil
	result := make(chan error, 1)
	go func() {
		resp, err := h.base.RoundTrip(forwarded)
		if err == nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-h.reached:
	case err := <-result:
		// Only this helper cancels forwarded, and only below, so any result
		// here ended some other way.
		h.t.Errorf("%s %s ended by %v before the handler signalled", req.Method, req.URL.Path, err)
		return nil, errors.Join(errors.New("request ended before the handler signalled"), err)
	}
	<-req.Context().Done()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		h.t.Errorf("%s %s ended by %v, not by the client timeout", req.Method, req.URL.Path, err)
	}
	return nil, req.Context().Err()
}

func TestHTTPResumeExhaustionIsTerminal(t *testing.T) {
	for _, sentinel := range []error{mediaworker.ErrSequenceBudgetExhausted, mediaworker.ErrSRTCPIndexExhausted, mediaworker.ErrDTLSSequenceExhausted} {
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
			expireObservedWorker(t, source)
			require.True(t, p.recoverWorker(ctx, source, time.Now()))
			status, err := p.Status(ctx)
			require.NoError(t, err)
			require.Len(t, status.Takeovers, 1)
			require.True(t, status.Takeovers[0].Lost)
			require.Equal(t, "checkpoint_unsafe", status.Takeovers[0].LossCause)
			require.Contains(t, status.Takeovers[0].Error, sentinel.Error())
			require.NotContains(t, status.Takeovers[0].Error, "attempts exhausted")
			require.EqualValues(t, 1, attempts.Load())
			require.Empty(t, recoveryWork(p, source))
		})
	}
}
