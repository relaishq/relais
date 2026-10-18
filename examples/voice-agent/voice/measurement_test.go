package voice

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMeasurementTableProvesColdAndIdleTLSReuse(t *testing.T) {
	var mu sync.Mutex
	seen := map[net.Conn]bool{}
	type connectionKey struct{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := r.Context().Value(connectionKey{}).(net.Conn)
		mu.Lock()
		cold := !seen[conn]
		seen[conn] = true
		mu.Unlock()
		if cold {
			time.Sleep(35 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(w, `{"text":"private-body"}`)
	}))
	server.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, connectionKey{}, c)
	}
	server.StartTLS()
	defer server.Close()
	var rows []Measurement
	var history RequestHistory
	for worker := 1; worker <= 2; worker++ {
		transport := server.Client().Transport.(*http.Transport).Clone()
		defer transport.CloseIdleConnections()
		transport.TLSClientConfig.MinVersion = tls.VersionTLS12
		p := NewOpenAIWithClient("private-key", &http.Client{Transport: transport, Timeout: time.Second})
		p.origin = server.URL + "?private-query="
		// Keep the query absent from request routing but present in the private URL.
		for turn := 1; turn <= 2; turn++ {
			var traces []RequestTrace
			p.Trace = func(r RequestTrace) { traces = append(traces, r) }
			ctx := requestContext(context.Background(), 10, "transcribe")
			_, err := p.Transcribe(ctx, "private-idempotency", []byte("private-audio"))
			require.NoError(t, err)
			require.Len(t, traces, 1)
			r := traces[0]
			require.Equal(t, "h1", r.Protocol)
			require.Equal(t, 200, r.Status)
			require.GreaterOrEqual(t, r.BodyRead, 4*time.Millisecond)
			if turn == 1 {
				require.False(t, r.Reused)
				require.False(t, r.Idle)
				require.GreaterOrEqual(t, r.Think, 30*time.Millisecond)
				require.Positive(t, r.TLS)
				require.Positive(t, r.Connect)
			} else {
				require.True(t, r.Reused)
				require.True(t, r.Idle)
				require.Positive(t, r.IdleTime)
				require.Zero(t, r.TLS)
				require.Zero(t, r.Connect)
			}
			t.Log(r.Line())
			rows = append(rows, Measurement{Worker: worker, Turn: turn, Timings: Timings{TranscribeStarted: 1000000, TranscribeFinished: 41000000, ReplyCharacters: 42, FirstSentenceCharacters: 30}, FirstAudio: 50 * time.Millisecond, Requests: history.Observe(traces)})
			time.Sleep(5 * time.Millisecond)
		}
	}
	var table bytes.Buffer
	require.NoError(t, WriteMeasurementTable(&table, rows))
	lines := strings.Split(strings.TrimSpace(table.String()), "\n")
	require.Len(t, lines, 5)
	for _, line := range lines {
		require.Len(t, strings.Split(line, "\t"), 31)
	}
	require.Contains(t, table.String(), "40.000")
	require.Contains(t, table.String(), "50.000")
	require.Contains(t, table.String(), "\tauth\tfirst\t")
	for _, secret := range []string{"private", "Authorization", "https://", "Bearer", "OggS"} {
		require.NotContains(t, table.String(), secret)
		for _, row := range rows {
			require.NotContains(t, row.Requests[0].Line(), secret)
		}
	}
	t.Log("\n" + table.String())
}
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

func TestMeasurementIdleIsProcessWideAndAccountsForOverlaps(t *testing.T) {
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	var history RequestHistory
	first := history.Observe([]RequestTrace{{started: at.Add(time.Second), finished: at.Add(4 * time.Second)}, {started: at, finished: at.Add(3 * time.Second)}})
	require.Equal(t, "first", first[0].idleBefore)
	require.Equal(t, "0.000", first[1].idleBefore)
	second := history.Observe([]RequestTrace{{started: at.Add(6 * time.Second), finished: at.Add(7 * time.Second)}})
	require.Equal(t, "2.000", second[0].idleBefore)
	require.NotContains(t, second[0].Line(), "idle_before")
	var table bytes.Buffer
	require.NoError(t, WriteMeasurementTable(&table, []Measurement{{Worker: 2, Turn: 1, WarmMode: "auth", Requests: second, Timings: Timings{ReplyStarted: int64(time.Second), ReplyFinished: int64(500 * time.Millisecond)}}}))
	require.Contains(t, table.String(), "2\t1\tauth\t2.000\t")
	require.NotContains(t, table.String(), "-500.000")
}
