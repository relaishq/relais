package movediag

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTraceOptInAndContextSemantics(t *testing.T) {
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "")
	require.Empty(t, ID(Begin(context.Background())))
	require.Equal(t, http.StatusNotFound, func() int {
		w := httptest.NewRecorder()
		Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("GET", Endpoint, nil))
		return w.Code
	}())
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	trace := Phase(Begin(parent), "resume")
	detached := Copy(context.Background(), trace)
	require.NotEmpty(t, ID(trace))
	require.Equal(t, ID(trace), ID(detached))
	require.NoError(t, detached.Err())
	req := httptest.NewRequest("POST", "/resume", nil).WithContext(detached)
	Inject(req)
	var recorder Recorder
	recorder.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		require.Equal(t, ID(trace), ID(r.Context()))
		recorder.Finish(NewEvent(r.Context(), "redis", "PING"), false)
	})).ServeHTTP(httptest.NewRecorder(), req)
	require.Len(t, recorder.Snapshot().Events, 1)
	require.Equal(t, "resume", recorder.Snapshot().Events[0].Phase)
	req.Header.Set(traceHeader, "invalid")
	recorder.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { require.Empty(t, ID(r.Context())) })).ServeHTTP(httptest.NewRecorder(), req.WithContext(context.Background()))
	for i := 0; i < limit; i++ {
		recorder.Finish(NewEvent(trace, "redis", "PING"), false)
	}
	require.Len(t, recorder.Snapshot().Events, limit)
	require.Equal(t, 1, recorder.Snapshot().Dropped)
}
