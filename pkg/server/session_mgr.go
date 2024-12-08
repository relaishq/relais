// Package server implements the core server components of Relais.
package server

import (
	"context"
	"sync"
	"time"
)

// SessionInfo holds metadata about an active media session.
// Each session represents a streaming connection with its configuration.
type SessionInfo struct {
	ID        string                 // Unique session identifier
	CreatedAt time.Time              // When the session was created
	Type      string                 // Session type ("webrtc", "rtsp", etc.)
	Metadata  map[string]interface{} // Additional session metadata
}

// SessionManager handles active media sessions.
// It provides thread-safe access to session information.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*SessionInfo
}

// NewSessionManager creates a new session manager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*SessionInfo),
	}
}

// CreateSession initializes a new media session.
// Returns the created session info and any error encountered.
func (sm *SessionManager) CreateSession(ctx context.Context, sessionType string, metadata map[string]interface{}) (*SessionInfo, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session := &SessionInfo{
		ID:        generateSessionID(),
		CreatedAt: time.Now(),
		Type:      sessionType,
		Metadata:  metadata,
	}

	sm.sessions[session.ID] = session
	return session, nil
}

// GetSession retrieves session information by ID.
// Returns the session info and whether it exists.
func (sm *SessionManager) GetSession(sessionID string) (*SessionInfo, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, exists := sm.sessions[sessionID]
	return session, exists
}

// CleanupSession removes a session and its associated resources.
func (sm *SessionManager) CleanupSession(ctx context.Context, sessionID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[sessionID]; !exists {
		return nil
	}

	delete(sm.sessions, sessionID)
	return nil
}

// StartCleanupWorker starts a background worker to cleanup expired sessions.
func (sm *SessionManager) StartCleanupWorker(ctx context.Context, maxAge time.Duration) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sm.cleanupExpiredSessions(maxAge)
			}
		}
	}()
}

func (sm *SessionManager) cleanupExpiredSessions(maxAge time.Duration) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for id, session := range sm.sessions {
		if now.Sub(session.CreatedAt) > maxAge {
			delete(sm.sessions, id)
		}
	}
}

// GetActiveSessions returns a list of all active sessions.
func (sm *SessionManager) GetActiveSessions() []*SessionInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessions := make([]*SessionInfo, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

// generateSessionID creates a unique session identifier.
func generateSessionID() string {
	// Implementation would generate a unique session ID
	return "session_" + time.Now().Format("20060102150405")
}
