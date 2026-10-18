package callharness

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/stretchr/testify/require"
)

// voiceMeasurement contains numbers only. FirstAudio is measured at the Pion client;
// FirstQueued is the independent host callback boundary.
type voiceMeasurement struct {
	WarmMode   string
	Worker     int
	Turn       int
	Timings    voice.Timings
	FirstAudio time.Duration
	Requests   []voiceRequest
}

// writeVoiceMeasurementTable repeats a turn's stage offsets/spans for each request,
// including warm and retries. It never includes text, audio, URLs or credentials.
func writeVoiceMeasurementTable(w io.Writer, rows []voiceMeasurement) error {
	_, err := fmt.Fprintln(w, "worker\tturn\twarm_mode\tidle_before_s\tstt_end_ms\tstt_ms\treply_start_ms\treply_end_ms\treply_ms\treply_chars\tfirst_sentence_chars\tspeech0_start_ms\tspeech0_end_ms\tspeech0_ms\tspeech0_audio_ms\tfirst_queued_ms\tpion_first_audio_ms\trequest_turn\tstage\tdns_ms\tconnect_ms\ttls_ms\treused\tidle\tidle_ms\tthink_ms\tbody_ms\tprotocol\tstatus")
	if err != nil {
		return err
	}
	ms := func(n int64) float64 { return float64(n) / float64(time.Millisecond) }
	for _, row := range rows {
		t := row.Timings
		warmMode := row.WarmMode
		if warmMode == "" {
			warmMode = "auth"
		}
		for _, r := range row.Requests {
			_, err = fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%d\t%d\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%d\t%s\t%.3f\t%.3f\t%.3f\t%t\t%t\t%.3f\t%.3f\t%.3f\t%s\t%d\n", row.Worker, row.Turn, warmMode, r.idleBefore, ms(t.TranscribeFinished), ms(t.TranscribeFinished), ms(t.ReplyStarted), ms(t.ReplyFinished), ms(max(0, t.ReplyFinished-t.ReplyStarted)), t.ReplyCharacters, t.FirstSentenceCharacters, ms(t.SpeechStarted), ms(t.SpeechFinished), ms(max(0, t.SpeechFinished-t.SpeechStarted)), ms(t.SpeechAudio), ms(t.FirstQueued), ms(int64(row.FirstAudio)), r.Turn, r.Stage, ms(int64(r.DNS)), ms(int64(r.Connect)), ms(int64(r.TLS)), r.Reused, r.Idle, ms(int64(r.IdleTime)), ms(int64(r.Think)), ms(int64(r.BodyRead)), r.Protocol, r.Status)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// voiceRequestHistory measures process-local idle time from the prior request's body
// completion to the next request start. Overlapping requests have zero idle.
// Keep one history across all workers in a measurement process.
type voiceRequestHistory struct{ finished time.Time }

type voiceRequest struct {
	voice.RequestTrace
	idleBefore string
}

func (h *voiceRequestHistory) observe(requests []voice.RequestTrace) []voiceRequest {
	rows := make([]voiceRequest, len(requests))
	for i, request := range requests {
		rows[i].RequestTrace = request
	}
	slices.SortStableFunc(rows, func(a, b voiceRequest) int { return a.StartedAt.Compare(b.StartedAt) })
	for i := range rows {
		r := &rows[i]
		r.idleBefore = "first"
		if !h.finished.IsZero() {
			r.idleBefore = fmt.Sprintf("%.3f", max(0, r.StartedAt.Sub(h.finished).Seconds()))
		}
		if r.FinishedAt.After(h.finished) {
			h.finished = r.FinishedAt
		}
	}
	return rows
}

type voiceMeasurementTransport func(*http.Request) (*http.Response, error)

func (f voiceMeasurementTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVoiceMeasurementTableProvesColdAndIdleTLSReuse(t *testing.T) {
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
	var rows []voiceMeasurement
	var history voiceRequestHistory
	for worker := 1; worker <= 2; worker++ {
		transport := server.Client().Transport.(*http.Transport).Clone()
		defer transport.CloseIdleConnections()
		transport.TLSClientConfig.MinVersion = tls.VersionTLS12
		client := &http.Client{Transport: voiceMeasurementTransport(func(r *http.Request) (*http.Response, error) {
			local := r.Clone(r.Context())
			local.URL.Scheme, local.URL.Host = "https", server.Listener.Addr().String()
			local.URL.RawQuery = "private-query=private-value"
			return transport.RoundTrip(local)
		}), Timeout: time.Second}
		p := voice.NewOpenAIWithClient("private-key", client)
		for turn := 1; turn <= 2; turn++ {
			var traces []voice.RequestTrace
			p.Trace = func(r voice.RequestTrace) { traces = append(traces, r) }
			ctx := context.Background()
			_, err := p.Transcribe(ctx, "private-idempotency", []byte("private-audio"))
			require.NoError(t, err)
			require.Len(t, traces, 1)
			r := traces[0]
			require.False(t, r.StartedAt.IsZero())
			require.False(t, r.FinishedAt.Before(r.StartedAt))
			require.NotContains(t, r.Line(), r.StartedAt.Format(time.RFC3339))
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
			rows = append(rows, voiceMeasurement{Worker: worker, Turn: turn, Timings: voice.Timings{TranscribeFinished: 40000000, ReplyCharacters: 42, FirstSentenceCharacters: 30}, FirstAudio: 50 * time.Millisecond, Requests: history.observe(traces)})
			time.Sleep(5 * time.Millisecond)
		}
	}
	var table bytes.Buffer
	require.NoError(t, writeVoiceMeasurementTable(&table, rows))
	lines := strings.Split(strings.TrimSpace(table.String()), "\n")
	require.Len(t, lines, 5)
	for _, line := range lines {
		require.Len(t, strings.Split(line, "\t"), 29)
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
func TestVoiceMeasurementIdleIsProcessWideAndAccountsForOverlaps(t *testing.T) {
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	var history voiceRequestHistory
	first := history.observe([]voice.RequestTrace{{StartedAt: at.Add(time.Second), FinishedAt: at.Add(4 * time.Second)}, {StartedAt: at, FinishedAt: at.Add(3 * time.Second)}})
	require.Equal(t, "first", first[0].idleBefore)
	require.Equal(t, "0.000", first[1].idleBefore)
	second := history.observe([]voice.RequestTrace{{StartedAt: at.Add(6 * time.Second), FinishedAt: at.Add(7 * time.Second)}})
	require.Equal(t, "2.000", second[0].idleBefore)
	require.NotContains(t, second[0].Line(), "idle_before")
	var table bytes.Buffer
	require.NoError(t, writeVoiceMeasurementTable(&table, []voiceMeasurement{{Worker: 2, Turn: 1, WarmMode: "auth", Requests: second, Timings: voice.Timings{ReplyStarted: int64(time.Second), ReplyFinished: int64(500 * time.Millisecond)}}}))
	require.Contains(t, table.String(), "2\t1\tauth\t2.000\t")
	require.NotContains(t, table.String(), "-500.000")
}
