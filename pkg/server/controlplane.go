package server

import (
	"encoding/json"
	"net/http"

	"github.com/relais/pkg/storage"
)

// ControlPlane handles the REST API for session management
type ControlPlane struct {
	sessionMgr *SessionManager
	storage    storage.Storage
}

// NewControlPlane creates a new control plane handler
func NewControlPlane(sessionMgr *SessionManager, storage storage.Storage) *ControlPlane {
	return &ControlPlane{
		sessionMgr: sessionMgr,
		storage:    storage,
	}
}

// RegisterRoutes sets up the HTTP routes
func (cp *ControlPlane) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/sessions", cp.handleSessions)
	mux.HandleFunc("/api/v1/sessions/", cp.handleSession)
	mux.HandleFunc("/api/v1/plugins/", cp.handlePlugins)
}

func (cp *ControlPlane) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		cp.createSession(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (cp *ControlPlane) createSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type     string                 `json:"type"`
		Metadata map[string]interface{} `json:"metadata"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	session, err := cp.sessionMgr.CreateSession(r.Context(), req.Type, req.Metadata)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(session)
}

func (cp *ControlPlane) handleSession(w http.ResponseWriter, r *http.Request) {
	// Extract session ID from URL path
	// Implementation details...
}

func (cp *ControlPlane) handlePlugins(w http.ResponseWriter, r *http.Request) {
	// Plugin management endpoints
	// Implementation details...
}
