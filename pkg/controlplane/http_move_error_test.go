package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestHTTPMoveFailurePreservesResult(t *testing.T) {
	p, a, b, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	p.workers["b"].worker = &failedHTTPResume{fakeWorker: b, cause: mediaworker.ErrClosed}
	response := httptest.NewRecorder()
	p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/calls/"+id+"/move?to=b", nil))
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var reply struct {
		Error  string     `json:"error"`
		Codes  []string   `json:"codes"`
		Result MoveResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &reply))
	require.Contains(t, reply.Error, "rolled back")
	require.Equal(t, []string{"closed"}, reply.Codes)
	require.Empty(t, response.Header().Get("Relais-Error-Codes"))
	require.Equal(t, reply.Error, reply.Result.Error)
	require.Equal(t, id, reply.Result.ID)
	require.Equal(t, "a", reply.Result.From)
	require.Equal(t, "b", reply.Result.To)
	require.False(t, reply.Result.Start.IsZero())
	require.True(t, reply.Result.End.After(reply.Result.Start))
	require.True(t, reply.Result.Result.RolledBack)
	require.True(t, a.runs(id))
	server := httptest.NewServer(p.Handler())
	defer server.Close()
	var result MoveResult
	err = privateapi.DoResult(context.Background(), server.Client(), server.URL, http.MethodPost, "/calls/"+id+"/move?to=b", nil, &result, RemoteErrors)
	require.ErrorIs(t, err, mediaworker.ErrClosed)
	var failure *privateapi.HTTPError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, http.StatusInternalServerError, failure.StatusCode)
	require.Equal(t, id, result.ID)
	require.Equal(t, result.Error, err.Error())
	require.True(t, result.Result.RolledBack)
	require.False(t, result.Start.IsZero())
	require.False(t, result.End.Before(result.Start))
}

func TestHTTPMoveEmptyErrorPreservesResponse(t *testing.T) {
	for _, diagnostics := range []string{"0", "1"} {
		t.Run("diagnostics="+diagnostics, func(t *testing.T) {
			t.Setenv("RELAIS_MOVE_DIAGNOSTICS", diagnostics)
			for _, scenario := range []string{"unknown", "no-target", "in-progress", "ended"} {
				t.Run(scenario, func(t *testing.T) {
					p, source, _, _ := setup(t)
					id, _, err := p.Create(context.Background(), "offer", "a")
					require.NoError(t, err)
					var cause error
					to := "b"
					switch scenario {
					case "unknown":
						id, cause = "missing", ErrUnknownCall
					case "no-target":
						to, cause = "a", ErrNoTarget
					case "in-progress":
						p.calls[id].mu.Lock()
						defer p.calls[id].mu.Unlock()
						cause = ErrMoveInProgress
					case "ended":
						require.NoError(t, source.EndSession(id))
						cause = sessionstore.ErrEnded
					}
					response := httptest.NewRecorder()
					p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/calls/"+id+"/move?to="+to, nil))
					legacy := httptest.NewRecorder()
					code := http.StatusConflict
					if errors.Is(cause, ErrUnknownCall) || errors.Is(cause, sessionstore.ErrNotFound) {
						code = http.StatusNotFound
					}
					http.Error(legacy, cause.Error(), code)
					require.Equal(t, legacy.Code, response.Code)
					require.Equal(t, legacy.Header(), response.Header())
					require.Equal(t, legacy.Body.Bytes(), response.Body.Bytes())
					server := httptest.NewServer(p.Handler())
					defer server.Close()
					var result MoveResult
					err = privateapi.DoResult(context.Background(), server.Client(), server.URL, http.MethodPost, "/calls/"+id+"/move?to="+to, nil, &result, RemoteErrors)
					require.ErrorContains(t, err, cause.Error())
					var failure *privateapi.HTTPError
					require.ErrorAs(t, err, &failure)
					require.Equal(t, code, failure.StatusCode)
					require.Zero(t, result)
				})
			}
		})
	}
}

// A lost hold acknowledgement fails a planned move before export. The result
// must survive without changing the uncertainty or its cancellation cause.
type failedHTTPHold struct {
	*fakeRelay
	cause error
}

func (r *failedHTTPHold) HoldSession(context.Context, string, netip.AddrPort) error { return r.cause }

func TestHTTPMoveUncertainPreservesResult(t *testing.T) {
	p, _, _, relay := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	p.SetRelay(&failedHTTPHold{fakeRelay: relay, cause: privateapi.Uncertain(context.DeadlineExceeded)})
	response := httptest.NewRecorder()
	p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/calls/"+id+"/move?to=b", nil))
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var reply struct {
		Error  string     `json:"error"`
		Codes  []string   `json:"codes"`
		Result MoveResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &reply))
	require.Equal(t, context.DeadlineExceeded.Error(), reply.Error)
	require.Equal(t, []string{"deadline", "uncertain"}, reply.Codes)
	require.Empty(t, response.Header().Get("Relais-Error-Codes"))
	require.Equal(t, reply.Error, reply.Result.Error)
	require.Equal(t, id, reply.Result.ID)
	require.False(t, reply.Result.Start.IsZero())
	require.True(t, reply.Result.End.After(reply.Result.Start))
}

type failedHTTPResume struct {
	*fakeWorker
	cause error
}

func (w *failedHTTPResume) ResumeSession([]byte, mediaworker.ResumeOptions) (string, error) {
	return "", w.cause
}

func TestHTTPMoveFailureStatusAndSentinels(t *testing.T) {
	for _, test := range []struct {
		name      string
		cause     error
		status    int
		sentinels []error
	}{
		{"unknown-call", ErrUnknownCall, 404, []error{ErrUnknownCall}},
		{"not-found", sessionstore.ErrNotFound, 404, []error{sessionstore.ErrNotFound}},
		{"ended", sessionstore.ErrEnded, 404, []error{sessionstore.ErrEnded, sessionstore.ErrNotFound}},
		{"unknown-session", mediaworker.ErrUnknownSession, 404, []error{mediaworker.ErrUnknownSession}},
		{"in-progress", ErrMoveInProgress, 409, []error{ErrMoveInProgress}},
		{"no-target", ErrNoTarget, 409, []error{ErrNoTarget}},
		{"lease-lost", sessionstore.ErrLeaseLost, 409, []error{sessionstore.ErrLeaseLost}},
		{"unsupported-offer", mediaworker.ErrUnsupportedOffer, 400, []error{mediaworker.ErrUnsupportedOffer}},
		{"uncertain", privateapi.Uncertain(context.DeadlineExceeded), 500, []error{privateapi.ErrUncertain, context.DeadlineExceeded}},
		{"joined", errors.Join(ErrMoveInProgress, privateapi.Uncertain(context.Canceled)), 409, []error{ErrMoveInProgress, privateapi.ErrUncertain, context.Canceled}},
		{"internal", errors.New("injected hold failure"), 500, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, _, _, relay := setup(t)
			id, _, err := p.Create(context.Background(), "offer", "a")
			require.NoError(t, err)
			p.SetRelay(&failedHTTPHold{fakeRelay: relay, cause: fmt.Errorf("hold reply: %w", test.cause)})
			server := httptest.NewServer(p.Handler())
			defer server.Close()
			var result MoveResult
			err = privateapi.DoResult(context.Background(), server.Client(), server.URL, http.MethodPost, "/calls/"+id+"/move?to=b", nil, &result, RemoteErrors)
			require.Error(t, err)
			for _, sentinel := range test.sentinels {
				require.ErrorIs(t, err, sentinel)
			}
			var failure *privateapi.HTTPError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, test.status, failure.StatusCode)
			require.Equal(t, id, result.ID)
			require.Equal(t, "a", result.From)
			require.Equal(t, "b", result.To)
			require.Equal(t, "hold reply: "+test.cause.Error(), result.Error)
			require.Equal(t, result.Error, err.Error())
			require.False(t, result.Start.IsZero())
			require.False(t, result.End.Before(result.Start))
		})
	}
}
