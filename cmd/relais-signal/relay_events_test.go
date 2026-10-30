package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
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
