// Package clockprobe records independent frontend-to-host clock observations.
// It does not select clock bounds or judge media timing.
package clockprobe

import (
	"encoding/json"
	"net/http"
	"time"
)

// Sample matches the signaling frontend's existing GET /clock response.
type Sample struct {
	ReceivedAt time.Time `json:"received_at"`
	SentAt     time.Time `json:"sent_at"`
}

// Handler brackets execution with the supplied wall clock.
func Handler(now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received := now().UTC()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Sample{ReceivedAt: received, SentAt: now().UTC()})
	})
}

// Wrap registers a clock route only on an explicitly enabled private API.
func Wrap(base http.Handler, enabled bool) http.Handler {
	if !enabled {
		return base
	}
	mux := http.NewServeMux()
	mux.Handle("GET /clock", Handler(time.Now))
	mux.Handle("/", base)
	return mux
}
