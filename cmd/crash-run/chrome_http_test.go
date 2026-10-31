//go:build !windows

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func chromeHTTPServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	return server
}

// Each browser call holds an SSE stream open. Call creation must still work
// after six streams occupy the HTTP/1.1 per-origin connection allowance.
func TestChromeHTTPStreamsDoNotBlockCallCreation(t *testing.T) {
	server := chromeHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := server.Client()
	transport := client.Transport.(*http.Transport)
	transport.MaxConnsPerHost = 6
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := range 8 {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/calls", strings.NewReader("offer"))
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("call %d creation blocked while previous event streams remain open: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("call %d status %s", i+1, response.Status)
		}
		if response.ProtoMajor != 2 {
			t.Fatalf("call %d negotiated %s; gate requires multiplexed HTTP/2", i+1, response.Proto)
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/calls/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := client.Do(request)
		if err != nil {
			t.Fatalf("call %d event stream: %v", i+1, err)
		}
		defer stream.Body.Close()
	}
}
