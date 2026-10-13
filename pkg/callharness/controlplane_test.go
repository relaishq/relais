package callharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestDrainRecordsLoss(t *testing.T) {
	rec := newRecorder()
	result := controlplane.MoveResult{ID: "call", From: "0", To: "1", Start: rec.start.Add(time.Second), End: rec.start.Add(2 * time.Second), Lost: true, LossCause: "recovery_failed", Error: "rollback failed"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/workers/0/drain", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMultiStatus)
		privateapi.Write(w, controlplane.DrainResult{Moves: []controlplane.MoveResult{result}, Error: "drain failed"})
	}))
	defer server.Close()
	h := &Harness{external: &ExternalTopology{}, workers: &workers{list: []*mediaworker.Worker{nil}}, signalingURL: server.URL + "/calls", httpClient: server.Client()}
	call := &Call{harness: h, rec: rec, remoteUfrag: "call"}
	h.calls = map[*Call]struct{}{call: {}}
	require.EqualError(t, h.Drain(0), "drain failed")
	reports := rec.moveReports()
	require.Len(t, reports, 1)
	require.True(t, reports[0].Lost)
	require.Equal(t, result.LossCause, reports[0].LossCause)
	require.Equal(t, result.Error, reports[0].Error)
	require.Equal(t, time.Second, reports[0].Start)
	require.Equal(t, 2*time.Second, reports[0].End)
}

func TestStatusAndDrainKeepLegacyErrors(t *testing.T) {
	const body = `{"error":"peer failed","codes":["uncertain","deadline"],"result":{}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	for _, endpoint := range []struct{ method, path string }{{http.MethodGet, "/status"}, {http.MethodPost, "/workers/0/drain"}} {
		t.Run(endpoint.path, func(t *testing.T) {
			h := &Harness{external: &ExternalTopology{}, workers: &workers{}, signalingURL: server.URL + "/calls", httpClient: server.Client()}
			err := h.controlRequest(context.Background(), endpoint.method, endpoint.path, new(any))
			require.EqualError(t, err, "callharness: 500 Internal Server Error: "+body)
			require.NotErrorIs(t, err, privateapi.ErrUncertain)
			h.httpClient = &http.Client{Transport: failedControlTransport{cause: context.Canceled}}
			err = h.controlRequest(context.Background(), endpoint.method, endpoint.path, new(any))
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, privateapi.ErrUncertain)
		})
	}
}

type failedControlTransport struct{ cause error }

func (f failedControlTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.cause }

func TestStatusAndDrainKeepLegacyResponseSize(t *testing.T) {
	body := strings.Repeat("x", privateapi.MaxBody+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, body)
	}))
	defer server.Close()
	h := &Harness{external: &ExternalTopology{}, workers: &workers{}, signalingURL: server.URL + "/calls", httpClient: server.Client()}
	for _, endpoint := range []struct{ method, path string }{{http.MethodGet, "/status"}, {http.MethodPost, "/workers/0/drain"}} {
		t.Run(endpoint.path, func(t *testing.T) {
			var result string
			require.NoError(t, h.controlRequest(context.Background(), endpoint.method, endpoint.path, &result))
			require.Len(t, result, privateapi.MaxBody+1)
		})
	}
}

func TestExternalMoveSuccessRecordsLoss(t *testing.T) {
	rec := newRecorder()
	result := controlplane.MoveResult{ID: "call", From: "0", To: "1", Start: rec.start.Add(time.Second), End: rec.start.Add(2 * time.Second), Lost: true, LossCause: "ownership_expired"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateapi.Write(w, result)
	}))
	defer server.Close()
	h := &Harness{external: &ExternalTopology{}, workers: &workers{}, signalingURL: server.URL + "/calls", httpClient: server.Client()}
	call := &Call{harness: h, rec: rec, remoteUfrag: "call"}
	require.NoError(t, call.MoveExternal(context.Background(), 1))
	reports := rec.moveReports()
	require.Len(t, reports, 1)
	require.True(t, reports[0].Lost)
	require.Equal(t, result.LossCause, reports[0].LossCause)
	require.Empty(t, reports[0].Error)
}
