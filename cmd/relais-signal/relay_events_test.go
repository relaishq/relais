package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRelayEventsAuthenticateStreamAndRestart(t *testing.T) {
	server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"))
		if restartPath(r.URL.Path) {
			require.Equal(t, "2", r.URL.Query().Get("generation"))
			w.Header().Set("Content-Type", "application/sdp")
			_, _ = io.WriteString(w, "answer")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"generation\":2}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/calls/id/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "data: {\"generation\":2}\n", line)
	_ = resp.Body.Close()
	cancel()
	resp, body := request(t, client, http.MethodPost, server.URL+"/calls/id/ice-restart?generation=2", "offer", testToken)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "answer", body)
	resp, _ = request(t, client, http.MethodGet, server.URL+"/calls/id/events", "", "")
	require.Equal(t, 401, resp.StatusCode)
}

// ADR 0016 section 5: signal forwards the A capability, its echoes, the
// restart reply metadata and the media-progress route. It never forges an
// acknowledgement and forwards an older control's metadata-free answer as is.
func TestRelayConfirmationSignaling(t *testing.T) {
	var progressStatus atomic.Int32
	var metadata atomic.Int32 // 0 valid, 1 none (older control), 2 malformed
	server, client := frontend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/calls":
			require.Equal(t, "true", r.Header.Get("Relais-Relay-Change"))
			require.Equal(t, "true", r.Header.Get("Relais-Media-Progress"))
			w.Header().Set("Location", "/calls/id")
			w.Header().Set("Relais-Relay-Change", "true")
			w.Header().Set("Relais-Media-Progress", "true")
			w.Header().Set("Content-Type", "application/sdp")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "answer")
		case restartPath(r.URL.Path):
			switch metadata.Load() {
			case 0:
				w.Header().Set("Relais-Relay-Generation", "3")
				w.Header().Set("Relais-Relay-Deadline", "2026-11-04T10:00:15Z")
				w.Header().Set("Relais-Required-Mids", `["0","1"]`)
				w.Header().Set("Relais-Relay-Confirmation", "decode")
			case 2:
				w.Header().Set("Relais-Relay-Generation", "3")
				w.Header().Set("Relais-Relay-Confirmation", "commit")
			}
			w.Header().Set("Content-Type", "application/sdp")
			_, _ = io.WriteString(w, "answer")
		case progressPath(r.URL.Path):
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			body, _ := io.ReadAll(r.Body)
			require.JSONEq(t, `{"generation":3,"decoded_mids":["0","1"]}`, string(body))
			if s := progressStatus.Load(); s != 0 {
				http.Error(w, "upstream detail that must not leak", int(s))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	req, err := http.NewRequest(http.MethodPost, server.URL+"/calls", strings.NewReader("offer"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Relais-Relay-Change", "true")
	req.Header.Set("Relais-Media-Progress", "true")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "true", resp.Header.Get("Relais-Relay-Change"))
	require.Equal(t, "true", resp.Header.Get("Relais-Media-Progress"))

	resp, body := request(t, client, http.MethodPost, server.URL+"/calls/id/ice-restart", "offer", testToken)
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, "answer", body)
	require.Equal(t, "3", resp.Header.Get("Relais-Relay-Generation"))
	require.Equal(t, "2026-11-04T10:00:15Z", resp.Header.Get("Relais-Relay-Deadline"))
	require.Equal(t, `["0","1"]`, resp.Header.Get("Relais-Required-Mids"))
	require.Equal(t, "decode", resp.Header.Get("Relais-Relay-Confirmation"))
	metadata.Store(1)
	resp, body = request(t, client, http.MethodPost, server.URL+"/calls/id/ice-restart", "offer", testToken)
	require.Equal(t, 200, resp.StatusCode, "an older control's answer still works")
	require.Equal(t, "answer", body)
	require.Empty(t, resp.Header.Get("Relais-Relay-Generation"))
	metadata.Store(2)
	resp, _ = request(t, client, http.MethodPost, server.URL+"/calls/id/ice-restart", "offer", testToken)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "malformed metadata is never forwarded")

	progress := func() *http.Response {
		req, err := http.NewRequest(http.MethodPost, server.URL+"/calls/id/media-progress", strings.NewReader(`{"generation":3,"decoded_mids":["0","1"]}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		data, _ := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NotContains(t, string(data), "upstream detail")
		return resp
	}
	require.Equal(t, http.StatusNoContent, progress().StatusCode)
	for _, status := range []int{http.StatusConflict, http.StatusGone, http.StatusServiceUnavailable, http.StatusRequestEntityTooLarge} {
		progressStatus.Store(int32(status))
		require.Equal(t, status, progress().StatusCode)
	}
	progressStatus.Store(http.StatusTooManyRequests)
	resp = progress()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("Retry-After"))
	resp, _ = request(t, client, http.MethodGet, server.URL+"/calls/id/media-progress", "", testToken)
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp, _ = request(t, client, http.MethodPost, server.URL+"/calls/id/media-progress", "", "")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
