package main

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
)

// Expose only tutorial text, never a snapshot, transport state or credentials.
func (d *demo) voiceStatus(w http.ResponseWriter, r *http.Request) {
	if d.agentName != "voice-agent/v1" || d.attached {
		http.NotFound(w, r)
		return
	}
	owner, err := d.owner(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, "voice owner unavailable", http.StatusNotFound)
		return
	}
	d.mu.Lock()
	worker := d.workers[owner.Owner]
	d.mu.Unlock()
	if worker == nil {
		http.Error(w, "voice worker unavailable", http.StatusServiceUnavailable)
		return
	}
	var status mediaworker.WorkerStatus
	err = privateapi.Do(r.Context(), d.client, worker.URL, http.MethodGet, "/status?agent-session="+url.QueryEscape(owner.ID), nil, &status, nil)
	if err != nil || status.AgentSession == nil || status.AgentSession.Name != d.agentName {
		http.Error(w, "voice status unavailable", http.StatusServiceUnavailable)
		return
	}
	var app struct {
		Timings struct {
			TranscribeFinished      int64 `json:"transcribe_finished_ns"`
			ReplyStarted            int64 `json:"reply_started_ns"`
			ReplyFinished           int64 `json:"reply_finished_ns"`
			ReplyCharacters         int   `json:"reply_characters"`
			FirstSentenceCharacters int   `json:"first_sentence_characters"`
			SpeechStarted           int64 `json:"speech_started_ns"`
			SpeechFinished          int64 `json:"speech_finished_ns"`
			SpeechAudio             int64 `json:"speech_audio_ns"`
			FirstQueued             int64 `json:"first_queued_ns"`
		} `json:"timings"`
		Turn       uint64 `json:"turn"`
		Phase      string `json:"phase"`
		Segment    int    `json:"segment,omitempty"`
		Render     int    `json:"render,omitempty"`
		Transcript string `json:"transcript,omitempty"`
		Reply      string `json:"reply,omitempty"`
		Notice     string `json:"notice,omitempty"`
		Error      string `json:"error,omitempty"`
		Outcome    string `json:"outcome,omitempty"`
	}
	if json.Unmarshal(status.AgentSession.App, &app) != nil {
		http.Error(w, "voice status invalid", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	privateapi.Write(w, app)
}
