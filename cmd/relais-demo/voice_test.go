package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

func TestVoiceLaunchEnvelopeAndKeyScope(t *testing.T) {
	cfg := config{Agent: "voice-agent/v1"}
	require.Equal(t, []string{"-agent-max-snapshot-bytes=24576", "-agent-snapshot-bytes-per-second=270336", "-agent-snapshot-burst-bytes=98304"}, workerArgs(cfg))
	require.Empty(t, workerArgs(config{Agent: "echo"}))
	token, err := configToken(cfg)
	require.NoError(t, err)
	require.NotEmpty(t, token)
	require.Contains(t, demoPageURL(cfg, "127.0.0.1:19102", token), "?source=microphone&autostart=0#token=")
	t.Setenv("OPENAI_API_KEY", "test-only-private-value")
	for _, entry := range processEnv("", "127.0.0.1:19386", "test:", false) {
		require.False(t, strings.HasPrefix(entry, "OPENAI_API_KEY="))
	}
}
func TestVoiceControlAuthenticationAndStatusWhitelist(t *testing.T) {
	manager := &clusterprocess.Manager{}
	defer manager.Stop(false)
	worker := testProcess(t, manager, t.TempDir(), "voice")
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "call", r.URL.Query().Get("agent-session"))
		privateapi.Write(w, map[string]any{"agent_session": map[string]any{"name": "voice-agent/v1", "app": map[string]any{"turn": 1, "phase": "idle", "transcript": "Hello.", "reply": "Hi.", "notice": "Ready.", "release_at_ns": 12345, "timings": map[string]any{"transcribe_finished_ns": 900000000, "reply_characters": 42, "unknown": "must-not-leak"}, "extra_private": "must-not-leak"}, "state": map[string]any{"Bytes": []byte("must-not-leak")}}})
	}))
	defer workerServer.Close()
	worker.URL = workerServer.URL
	requests := 0
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			privateapi.Write(w, controlplane.Status{Calls: []controlplane.CallStatus{{ID: "call", Owner: "0"}}})
			return
		}
		require.Equal(t, "/calls/call/agent/control", r.URL.Path)
		require.Empty(t, r.Header.Get("X-Relais-Demo-Token"))
		requests++
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"id":"press","sequence":1,"checkpointed":false}`))
	}))
	defer control.Close()
	d := &demo{ctx: context.Background(), agentName: "voice-agent/v1", launchToken: "test-token", control: control.URL, client: control.Client(), workers: map[string]*workerProcess{"0": worker}}
	handler := d.handler(http.NotFoundHandler())
	for _, token := range []string{"", "wrong", "test-token"} {
		r := httptest.NewRequest("POST", "http://localhost/calls/call/agent/control", strings.NewReader(`{"id":"press","sequence":1,"name":"ptt.start"}`))
		r.Header.Set("X-Relais-Demo-Token", token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if token == "test-token" {
			require.Equal(t, 202, w.Code)
		} else {
			require.Equal(t, 403, w.Code)
		}
	}
	require.Equal(t, 1, requests)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/demo/voice/call", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "Hello.")
	require.NotContains(t, w.Body.String(), "must-not-leak")
	require.NotContains(t, w.Body.String(), "extra_private")
	require.NotContains(t, w.Body.String(), "release_at_ns")
	require.NotContains(t, w.Body.String(), "unknown")
	require.Contains(t, w.Body.String(), `"transcribe_finished_ns":900000000`)
	require.Contains(t, w.Body.String(), `"reply_characters":42`)
	require.Contains(t, w.Body.String(), `"recording_closed_ns":0`)
}
