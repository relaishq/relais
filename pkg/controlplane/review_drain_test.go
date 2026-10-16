package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/stretchr/testify/require"
)

// A media-only worker that, like RemoteWorker, exposes the new preflight but
// whose preflight is transiently unavailable (no data sessions exist).
type reviewFlakyCheckWorker struct{ *fakeWorker }

func (w *reviewFlakyCheckWorker) CheckSessionMove(string) error { return privateapi.ErrUncertain }
func (w *reviewFlakyCheckWorker) EndSessionReason(id, _ string) error {
	return w.EndSession(id)
}

// Reviewer probe: media-only drain of reconstructed (unclassified) calls.
func TestReviewMediaDrainUnclassifiedCheckFailure(t *testing.T) {
	p, a, b, _ := setup(t)
	ctx := context.Background()
	id1, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	id2, _, err := p.Create(ctx, "offer", "a")
	require.NoError(t, err)
	// Simulate calls reconstructed after a control-plane restart/takeover.
	p.calls[id1].classified = false
	p.calls[id2].classified = false
	p.workers["a"].worker = &reviewFlakyCheckWorker{fakeWorker: a}
	results, err := p.Drain(ctx, "a")
	t.Logf("results=%+v err=%v a1=%t a2=%t b1=%t b2=%t", results, err, a.runs(id1), a.runs(id2), b.runs(id1), b.runs(id2))
	require.NoError(t, err, "media-only drain should not depend on the data preflight")
	require.Len(t, results, 2)
	require.True(t, b.runs(id1))
	require.True(t, b.runs(id2))
	require.False(t, a.runs(id1))
	require.False(t, a.runs(id2))
}

func TestReviewDataMoveHTTPConflictTypedCode(t *testing.T) {
	p, a, _, _ := setup(t)
	p.workers["a"].worker = &dataPolicyWorker{fakeWorker: a}
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	response := httptest.NewRecorder()
	p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/calls/"+id+"/move?to=b", nil))
	require.Equal(t, http.StatusConflict, response.Code)
	require.Contains(t, response.Body.String(), `"data_session_not_movable"`)
}

type reviewRemoteCheckWorker struct {
	*fakeWorker
	remote *RemoteWorker
}

func (w *reviewRemoteCheckWorker) CheckSessionMove(id string) error {
	return w.remote.CheckSessionMove(id)
}
func (w *reviewRemoteCheckWorker) CheckSessionMoveContext(ctx context.Context, id string) error {
	return w.remote.CheckSessionMoveContext(ctx, id)
}
func (w *reviewRemoteCheckWorker) EndSessionReason(id, _ string) error { return w.EndSession(id) }

func TestReviewDrainPreflightHonorsContext(t *testing.T) {
	p, a, _, _ := setup(t)
	id, _, err := p.Create(context.Background(), "offer", "a")
	require.NoError(t, err)
	p.calls[id].classified = false
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	p.workers["a"].worker = &reviewRemoteCheckWorker{fakeWorker: a, remote: &RemoteWorker{URL: server.URL, Client: &http.Client{Timeout: time.Second}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.Drain(ctx, "a"); done <- err }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Drain ignored its context while checking the data policy")
	}
}
