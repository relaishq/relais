package privateapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/movediag"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticsTimeFullBodyAndReuseConnections(t *testing.T) {
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	ctx := movediag.Phase(movediag.Begin(context.Background()), "export")
	server := httptest.NewServer(movediag.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, movediag.ID(ctx), movediag.ID(r.Context()))
		_, _ = w.Write([]byte(`{"state":`))
		w.(http.Flusher).Flush()
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write([]byte(`"secret"}`))
	})))
	defer server.Close()
	for i := 0; i < 2; i++ {
		var reply map[string]string
		require.NoError(t, Do(ctx, server.Client(), server.URL, http.MethodPost, "/sessions/private-session/export", nil, &reply, nil))
	}
	var events []movediag.Event
	for _, event := range movediag.Default.Snapshot().Events {
		if event.TraceID == movediag.ID(ctx) {
			events = append(events, event)
		}
	}
	require.Len(t, events, 2)
	for _, event := range events {
		require.GreaterOrEqual(t, event.Duration, 25*time.Millisecond)
		require.Equal(t, "POST /sessions/:id/export", event.Operation)
		require.Equal(t, len(`{"state":"secret"}`), event.ResponseBytes)
		require.False(t, event.Failed)
	}
	require.Equal(t, 1, events[1].ReusedConnections)
}

func TestDisabledDiagnosticsDoNotTraceRequests(t *testing.T) {
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "1")
	ctx := movediag.Begin(context.Background())
	t.Setenv("RELAIS_MOVE_DIAGNOSTICS", "")
	server := httptest.NewServer(movediag.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, movediag.ID(r.Context()))
		require.Empty(t, r.Header.Get("X-Relais-Move-Trace"))
		Write(w, map[string]bool{"ok": true})
	})))
	defer server.Close()
	require.NoError(t, Do(ctx, server.Client(), server.URL, http.MethodPost, "/resume", nil, nil, nil))
	for _, event := range movediag.Default.Snapshot().Events {
		require.NotEqual(t, movediag.ID(ctx), event.TraceID)
	}
}
