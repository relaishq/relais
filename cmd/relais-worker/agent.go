package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/relais/internal/privateapi"
	"github.com/relais/pkg/agent"
	agentdemo "github.com/relais/pkg/agent/demo"
	"github.com/relais/pkg/mediaworker"
)

func selectAgent(name string) (mediaworker.AgentConfig, error) {
	switch name {
	case "echo":
		return mediaworker.AgentConfig{}, nil
	case "demo":
		return mediaworker.AgentConfig{Factory: agentdemo.Factory}, nil
	default:
		return mediaworker.AgentConfig{}, errors.New("-agent must be echo or demo")
	}
}

// The demo wrapper observes the existing private protocol; it never changes
// snapshot, adoption or transport policy. History is bounded per worker.
type agentHistory struct {
	mu     sync.Mutex
	values map[string]agentdemo.Status
	order  []string
}

func (h *agentHistory) put(id string, update func(*agentdemo.Status)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.values == nil {
		h.values = map[string]agentdemo.Status{}
	}
	value, exists := h.values[id]
	if !exists {
		if len(h.order) == 128 {
			delete(h.values, h.order[0])
			h.order = h.order[1:]
		}
		h.order = append(h.order, id)
	}
	update(&value)
	h.values[id] = value
}
func (h *agentHistory) get(id string) agentdemo.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.values[id]
}

type observedResponse struct {
	http.ResponseWriter
	code int
	body bytes.Buffer
}

func (w *observedResponse) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }
func (w *observedResponse) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	_, _ = w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func agentHandler(worker *mediaworker.Worker, name string) http.Handler {
	base := worker.PrivateHandler()
	if name != "demo" {
		return base
	}
	history := &agentHistory{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /demo/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		value := history.get(id)
		state, progress, err := worker.SessionAgent(id)
		if err == nil {
			value.State, err = agentdemo.Decode(state)
			value.Progress = progress
		}
		if err != nil && value.Exported == nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		privateapi.Write(w, value)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the two snapshot boundaries need observation. Never copy arbitrary
		// HTTP output or metrics. PrivateHandler still validates and executes them.
		export := r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sessions/") && strings.HasSuffix(r.URL.Path, "/export")
		resume := r.Method == http.MethodPost && r.URL.Path == "/resume"
		if !export && !resume {
			base.ServeHTTP(w, r)
			return
		}
		var incoming bytes.Buffer
		if resume {
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.TeeReader(r.Body, &incoming), r.Body}
		}
		response := &observedResponse{ResponseWriter: w}
		base.ServeHTTP(response, r)
		if response.code != http.StatusOK {
			return
		}
		if export {
			var reply mediaworker.ExportReply
			if json.Unmarshal(response.body.Bytes(), &reply) != nil {
				return
			}
			evidence, err := agentdemo.FromSnapshot(reply.State)
			if err != nil {
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sessions/"), "/export")
			history.put(id, func(value *agentdemo.Status) { value.Exported = &evidence })
		} else {
			var request mediaworker.ResumeRequest
			var reply mediaworker.ResumeReply
			if json.Unmarshal(incoming.Bytes(), &request) != nil || json.Unmarshal(response.body.Bytes(), &reply) != nil {
				return
			}
			evidence, err := agentdemo.FromSnapshot(request.State)
			if err != nil {
				return
			}
			history.put(reply.ID, func(value *agentdemo.Status) { value.Checkpoint = &evidence; value.Exported = nil })
		}
	}))
	return mux
}

var _ agent.Agent = (*agentdemo.Agent)(nil)
