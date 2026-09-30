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
	// Participants in this session. Keys are participant IDs.
	Participants map[string]*Participant
}

// Participant represents a user/peer in a session.
type Participant struct {
	ID       string                 // Unique participant identifier
	JoinedAt time.Time              // When the participant joined
	Meta     map[string]interface{} // Arbitrary participant metadata
	Tracks   []TrackInfo            // Tracks published by the participant
}

// TrackInfo contains metadata about a published track.
type TrackInfo struct {
	ID         string                 // Track identifier
	Kind       string                 // "audio" | "video"
	Codec      string                 // e.g., "opus", "h264"
	SSRC       uint32                 // Optional: RTP SSRC if known
	Attributes map[string]interface{} // Additional attributes
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
		ID:           generateSessionID(),
		CreatedAt:    time.Now(),
		Type:         sessionType,
		Metadata:     metadata,
		Participants: make(map[string]*Participant),
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

// AddParticipant adds a participant to a session.
func (sm *SessionManager) AddParticipant(sessionID string, p *Participant) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[sessionID]
	if !ok {
		return false
	}
	if _, exists := s.Participants[p.ID]; exists {
		return true
	}
	s.Participants[p.ID] = p
	return true
}

// RemoveParticipant removes a participant from a session.
func (sm *SessionManager) RemoveParticipant(sessionID, participantID string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[sessionID]
	if !ok {
		return false
	}
	delete(s.Participants, participantID)
	return true
}

// AddTrack associates a track with a participant.
func (sm *SessionManager) AddTrack(sessionID, participantID string, t TrackInfo) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[sessionID]
	if !ok {
		return false
	}
	p, ok := s.Participants[participantID]
	if !ok {
		return false
	}
	p.Tracks = append(p.Tracks, t)
	return true
}

// generateSessionID creates a unique session identifier.
func generateSessionID() string {
	// Implementation would generate a unique session ID
	return "session_" + time.Now().Format("20060102150405")
}
