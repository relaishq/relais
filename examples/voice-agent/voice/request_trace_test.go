package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestTraceLogsHTTPRefusalWithoutResponseData(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Private", "secret")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, "secret body")
	}))
	defer server.Close()
	p := NewOpenAIWithClient("secret-key", server.Client())
	p.origin = server.URL
	var rows []RequestTrace
	p.Trace = func(r RequestTrace) { rows = append(rows, r) }
	_, err := p.Reply(requestContext(context.Background(), 20, "reply"), "secret-id", "secret question", nil)
	require.EqualError(t, err, "provider_uncertain")
	require.Len(t, rows, 1)
	require.Equal(t, 429, rows[0].Status)
	require.NotContains(t, rows[0].Line(), "secret")
}

func TestRequestTraceReportsHTTP2AndReuse(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"text":"fixture"}`) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	p := NewOpenAIWithClient("fixture", server.Client())
	p.origin = server.URL
	var rows []RequestTrace
	p.Trace = func(row RequestTrace) { rows = append(rows, row) }
	for range 2 {
		_, err := p.Transcribe(requestContext(context.Background(), 10, "transcribe"), "fixture", []byte("OggS"))
		require.NoError(t, err)
	}
	require.Len(t, rows, 2)
	require.Equal(t, "h2", rows[0].Protocol)
	require.Equal(t, "h2", rows[1].Protocol)
	require.True(t, rows[1].Reused)
}
