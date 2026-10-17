package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/stretchr/testify/require"
)

func TestMarkDrainingDoesNotMoveOrWait(t *testing.T) {
	p, a, b, _ := setup(t)
	ctx := context.Background()
	id, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	p.workers["a"].reserved++ // Mark must not wait for a raced create.
	server := httptest.NewServer(p.ProcessHandler())
	defer server.Close()
	for range 2 {
		require.NoError(t, privateapi.Do(ctx, server.Client(), server.URL, http.MethodPost, "/private/workers/a/mark-draining", nil, nil, nil))
	}
	require.NoError(t, p.Heartbeat(a.addr))
	status, err := p.Status(ctx)
	require.NoError(t, err)
	require.True(t, status.Workers[0].Draining)
	require.True(t, a.runs(id))
	require.False(t, b.runs(id))
	_, _, err = p.Create(ctx, "offer", "a")
	require.ErrorIs(t, err, ErrNoTarget)
	_, _, err = p.Create(ctx, "offer", "")
	require.NoError(t, err)
	require.Equal(t, 1, b.next)
	req := httptest.NewRequest(http.MethodPost, "/private/workers/missing/mark-draining", nil)
	rw := httptest.NewRecorder()
	p.ProcessHandler().ServeHTTP(rw, req)
	require.Equal(t, 404, rw.Code)
	require.NoError(t, p.MarkDraining("b"))
	rw = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/calls", nil)
	req.Header.Set("Content-Type", "application/sdp")
	p.Handler().ServeHTTP(rw, req)
	require.Equal(t, 503, rw.Code)
	var failure struct {
		Code      string
		Retryable bool
	}
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &failure))
	require.Equal(t, "no_target", failure.Code)
	require.True(t, failure.Retryable)
}

type refusingCreateWorker struct {
	*fakeWorker
	entered, release chan struct{}
}

func (w *refusingCreateWorker) CreateSession(ctx context.Context, _ string) (string, string, error) {
	close(w.entered)
	select {
	case <-w.release:
		return "", "", mediaworker.ErrDraining
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}
func TestRacedCreateReturnsRetryablePlacement(t *testing.T) {
	p, a, _, _ := setup(t)
	worker := &refusingCreateWorker{fakeWorker: a, entered: make(chan struct{}), release: make(chan struct{})}
	p.workers["a"].worker = worker
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rw := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/calls?worker=a", nil)
		req.Header.Set("Content-Type", "application/sdp")
		p.Handler().ServeHTTP(rw, req)
		done <- rw
	}()
	select {
	case <-worker.entered:
	case <-time.After(time.Second):
		t.Fatal("create never reached worker")
	}
	require.NoError(t, p.MarkDraining("a"))
	close(worker.release)
	rw := <-done
	require.Equal(t, 503, rw.Code)
	var failure struct {
		Code      string
		Retryable bool
	}
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &failure))
	require.Equal(t, "worker_draining", failure.Code)
	require.True(t, failure.Retryable)
	require.Empty(t, p.calls)
	require.Zero(t, p.workers["a"].reserved)
	require.Zero(t, a.next)
}
