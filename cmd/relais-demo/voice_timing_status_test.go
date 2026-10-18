package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relaishq/relais/examples/voice-agent/voice"
	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/stretchr/testify/require"
)

// voiceTimingStatus exercises the production HTTP whitelist, including its JSON
// encoding. The Node page test consumes this exact response, not a hand copy.
func voiceTimingStatus(t *testing.T, timings voice.Timings) []byte {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "")
	manager := &clusterprocess.Manager{}
	t.Cleanup(func() { manager.Stop(false) })
	process := testProcess(t, manager, t.TempDir(), "voice")
	app, err := json.Marshal(voice.State{Turn: 10, Phase: "idle", Timings: timings})
	require.NoError(t, err)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateapi.Write(w, map[string]any{"agent_session": map[string]any{"name": voice.Name, "app": json.RawMessage(app)}})
	}))
	t.Cleanup(worker.Close)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateapi.Write(w, controlplane.Status{Calls: []controlplane.CallStatus{{ID: "call", Owner: "0"}}})
	}))
	t.Cleanup(control.Close)
	process.URL = worker.URL
	d := &demo{ctx: context.Background(), agentName: voice.Name, control: control.URL, client: control.Client(), workers: map[string]*workerProcess{"0": process}}
	w := httptest.NewRecorder()
	d.handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/demo/voice/call", nil))
	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.Bytes()
}

func TestVoiceTimingStatusFixture(t *testing.T) {
	body := voiceTimingStatus(t, voice.Timings{TranscribeFinished: 900e6, ReplyStarted: 900e6, ReplyFinished: 3e9, SpeechStarted: 3e9, SpeechFinished: 4200e6, FirstQueued: 4400e6})
	t.Logf("voice-timing-status-json: %s", body)
}

func TestVoiceTimingStatusIncludesZeroValues(t *testing.T) {
	var status struct {
		Timings map[string]int64 `json:"timings"`
	}
	require.NoError(t, json.Unmarshal(voiceTimingStatus(t, voice.Timings{}), &status))
	require.Len(t, status.Timings, 9)
	for _, name := range []string{"transcribe_finished_ns", "reply_started_ns", "reply_finished_ns", "reply_characters", "first_sentence_characters", "speech_started_ns", "speech_finished_ns", "speech_audio_ns", "first_queued_ns"} {
		require.Contains(t, status.Timings, name)
		require.Zero(t, status.Timings[name])
	}
}
