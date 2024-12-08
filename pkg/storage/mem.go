// Package storage provides storage implementations for media frames.
package storage

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// MemoryStorage implements the Storage interface using in-memory maps.
// This implementation is suitable for development, testing, and scenarios
// where persistence is not required. It stores all frames in memory, which
// means it's fast but not suitable for large amounts of data or when
// persistence across restarts is needed.
//
// Thread Safety:
// All operations are protected by a RWMutex, making it safe for concurrent
// access from multiple goroutines. Read operations (Get*, List*) acquire
// read locks, while write operations (Put*, Delete*) acquire write locks.
//
// Memory Usage:
// Since all frames are stored in memory, users should be mindful of:
// - The number of sessions and frames being stored
// - The size of frame data (especially for high-resolution video)
// - Cleaning up sessions that are no longer needed via DeleteSession
type MemoryStorage struct {
	mu       sync.RWMutex                    // Protects access to the frames map
	frames   map[string]map[int64]Frame      // Maps session ID to a map of frame index to Frame
	sessions map[string]struct{}             // Tracks active sessions for efficient listing
}

// NewMemoryStorage creates a new MemoryStorage instance.
// It initializes the internal maps used for storing frames and tracking sessions.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		frames:   make(map[string]map[int64]Frame),
		sessions: make(map[string]struct{}),
	}
}

// PutFrame stores a frame in memory, creating the session map if it doesn't exist.
// If a frame with the same session ID and index already exists, it will be overwritten.
//
// The context parameter is included for interface compatibility but is not used
// since memory operations are immediate.
func (s *MemoryStorage) PutFrame(_ context.Context, frame Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create session map if it doesn't exist
	if _, exists := s.frames[frame.SessionID]; !exists {
		s.frames[frame.SessionID] = make(map[int64]Frame)
		s.sessions[frame.SessionID] = struct{}{}
	}

	// Store the frame
	s.frames[frame.SessionID][frame.Index] = frame
	return nil
}

// GetFrame retrieves a specific frame from memory by session ID and frame index.
// Returns an error if the session or frame doesn't exist.
//
// The context parameter is included for interface compatibility but is not used
// since memory operations are immediate.
func (s *MemoryStorage) GetFrame(_ context.Context, sessionID string, frameIndex int64) (Frame, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check if session exists
	sessionFrames, exists := s.frames[sessionID]
	if !exists {
		return Frame{}, fmt.Errorf("session not found: %s", sessionID)
	}

	// Check if frame exists
	frame, exists := sessionFrames[frameIndex]
	if !exists {
		return Frame{}, fmt.Errorf("frame not found: session %s, index %d", sessionID, frameIndex)
	}

	return frame, nil
}

// ListFrames returns all frames for a given session, sorted by frame index.
// Returns an error if the session doesn't exist.
//
// The context parameter is included for interface compatibility but is not used
// since memory operations are immediate.
func (s *MemoryStorage) ListFrames(_ context.Context, sessionID string) ([]Frame, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check if session exists
	sessionFrames, exists := s.frames[sessionID]
	if !exists {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	// Convert map to sorted slice
	frames := make([]Frame, 0, len(sessionFrames))
	for _, frame := range sessionFrames {
		frames = append(frames, frame)
	}

	// Sort frames by index
	sort.Slice(frames, func(i, j int) bool {
		return frames[i].Index < frames[j].Index
	})

	return frames, nil
}

// ListSessions returns a list of all active session IDs.
// The returned list is sorted alphabetically for consistent ordering.
//
// The context parameter is included for interface compatibility but is not used
// since memory operations are immediate.
func (s *MemoryStorage) ListSessions(_ context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Convert session map keys to sorted slice
	sessions := make([]string, 0, len(s.sessions))
	for sessionID := range s.sessions {
		sessions = append(sessions, sessionID)
	}
	sort.Strings(sessions)

	return sessions, nil
}

// DeleteSession removes all frames for a given session.
// Returns an error if the session doesn't exist.
//
// The context parameter is included for interface compatibility but is not used
// since memory operations are immediate.
func (s *MemoryStorage) DeleteSession(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if session exists
	if _, exists := s.frames[sessionID]; !exists {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Remove session data
	delete(s.frames, sessionID)
	delete(s.sessions, sessionID)
	return nil
}

// Close implements the Storage interface but does nothing for MemoryStorage
// since there are no resources to clean up.
//
// This method is included for interface compatibility and always returns nil.
func (s *MemoryStorage) Close() error {
	return nil
}
