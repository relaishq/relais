package room

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Signaler interface {
	Offer(context.Context, string) (string, string, error)
	Delete(context.Context, string) error
}

// WithMediaProxy keeps the signal bearer server-side. Capabilities own calls.
func (r *Room) WithMediaProxy(base http.Handler, s Signaler) http.Handler {
	var mu sync.Mutex
	calls := map[string]string{}
	mux := http.NewServeMux()
	mux.Handle("/", base)
	authorize := func(q *http.Request) string {
		if !localOrigin(q) {
			return ""
		}
		token := strings.TrimPrefix(q.Header.Get("Authorization"), "Bearer ")
		r.mu.Lock()
		defer r.mu.Unlock()
		p := r.identities[token]
		if p == nil || time.Now().After(p.expires) {
			return ""
		}
		return token
	}
	mux.HandleFunc("POST /media", func(w http.ResponseWriter, q *http.Request) {
		token := authorize(q)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, q.Body, 65536))
		if err != nil {
			http.Error(w, "offer too large", http.StatusRequestEntityTooLarge)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for _, owner := range calls {
			if owner == token {
				count++
			}
		}
		if count >= 4 {
			http.Error(w, "call limit", http.StatusTooManyRequests)
			return
		}
		answer, path, err := s.Offer(q.Context(), string(b))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		calls[path] = token
		_ = json.NewEncoder(w).Encode(map[string]string{"answer": answer, "session_id": strings.TrimPrefix(path, "/calls/"), "path": path})
	})
	mux.HandleFunc("DELETE /media", func(w http.ResponseWriter, q *http.Request) {
		token := authorize(q)
		path := q.URL.Query().Get("path")
		mu.Lock()
		defer mu.Unlock()
		if token == "" || calls[path] != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := s.Delete(q.Context(), path); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		delete(calls, path)
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
