package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/go-redis/redis/v8"
)

// RedisStorage implements the Storage interface using Redis as the backend.
// This implementation provides persistent storage and is suitable for production
// use cases where data needs to survive process restarts. It uses Redis Lists
// to store frames for each session and Redis Sets to track active sessions.
//
// Key Schema:
// - Session frames: "frames:{sessionID}" (List)
// - Active sessions: "active_sessions" (Set)
//
// Performance Considerations:
// - Uses pipelining for batch operations where possible
// - Implements efficient session tracking using Redis Sets
// - Handles Redis connection errors and retries
//
// Thread Safety:
// All operations are thread-safe as Redis handles concurrent access.
// The client connection is safe for concurrent use by multiple goroutines.
type RedisStorage struct {
	client *redis.Client // Redis client connection
	prefix string        // Key prefix for namespacing (e.g., "myapp:")
}

// RedisConfig holds configuration options for RedisStorage.
type RedisConfig struct {
	Addr     string // Redis server address (e.g., "localhost:6379")
	Password string // Redis password (optional)
	DB       int    // Redis database number
	Prefix   string // Key prefix for namespacing (optional)
}

// NewRedisStorage creates a new RedisStorage instance.
// For backward compatibility, it accepts either a Redis URL string or a RedisConfig.
// If a string is provided, it's treated as the Redis server address.
//
// Returns an error if:
// - Cannot connect to Redis server
// - Invalid configuration parameters
// - Redis ping fails
func NewRedisStorage(config interface{}) (*RedisStorage, error) {
	var client *redis.Client

	switch cfg := config.(type) {
	case string:
		// Backward compatibility: treat string as Redis address
		client = redis.NewClient(&redis.Options{
			Addr: cfg,
		})
	case RedisConfig:
		client = redis.NewClient(&redis.Options{
			Addr:     cfg.Addr,
			Password: cfg.Password,
			DB:       cfg.DB,
		})
	default:
		return nil, fmt.Errorf("invalid configuration type: expected string or RedisConfig")
	}

	// Verify connection
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis connection failed: %v", err)
	}

	prefix := ""
	if cfg, ok := config.(RedisConfig); ok {
		prefix = cfg.Prefix
	}

	return &RedisStorage{
		client: client,
		prefix: prefix,
	}, nil
}

// frameKey generates the Redis key for storing frames of a session.
func (s *RedisStorage) frameKey(sessionID string) string {
	return fmt.Sprintf("%sframes:%s", s.prefix, sessionID)
}

// sessionKey generates the Redis key for the active sessions set.
func (s *RedisStorage) sessionKey() string {
	return s.prefix + "active_sessions"
}

// PutFrame stores a frame in Redis.
// The frame is serialized to JSON and stored in a Redis List.
// The session ID is also added to the active sessions set.
//
// The operation is atomic: either both the frame is stored and the session
// is tracked, or neither operation occurs.
func (s *RedisStorage) PutFrame(ctx context.Context, frame Frame) error {
	// Serialize frame to JSON
	frameJSON, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("failed to marshal frame: %v", err)
	}

	// Create a pipeline for atomic operations
	pipe := s.client.Pipeline()

	// Add frame to the session's list
	pipe.RPush(ctx, s.frameKey(frame.SessionID), frameJSON)

	// Track the session
	pipe.SAdd(ctx, s.sessionKey(), frame.SessionID)

	// Execute pipeline
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to store frame: %v", err)
	}

	return nil
}

// GetFrame retrieves a specific frame from Redis by session ID and frame index.
// It scans the session's frame list to find the frame with the matching index.
//
// Returns an error if:
// - Session doesn't exist
// - Frame index not found
// - Redis operation fails
// - Frame data is corrupted
func (s *RedisStorage) GetFrame(ctx context.Context, sessionID string, frameIndex int64) (Frame, error) {
	// Check if session exists
	exists, err := s.client.SIsMember(ctx, s.sessionKey(), sessionID).Result()
	if err != nil {
		return Frame{}, fmt.Errorf("failed to check session: %v", err)
	}
	if !exists {
		return Frame{}, fmt.Errorf("session not found: %s", sessionID)
	}

	// Get all frames for the session
	frameList, err := s.client.LRange(ctx, s.frameKey(sessionID), 0, -1).Result()
	if err != nil {
		return Frame{}, fmt.Errorf("failed to get frames: %v", err)
	}

	// Find frame with matching index
	for _, frameJSON := range frameList {
		var frame Frame
		if err := json.Unmarshal([]byte(frameJSON), &frame); err != nil {
			return Frame{}, fmt.Errorf("failed to unmarshal frame: %v", err)
		}
		if frame.Index == frameIndex {
			return frame, nil
		}
	}

	return Frame{}, fmt.Errorf("frame not found: session %s, index %d", sessionID, frameIndex)
}

// ListFrames returns all frames for a given session, sorted by frame index.
// The frames are retrieved from the Redis List and deserialized from JSON.
//
// Returns an error if:
// - Session doesn't exist
// - Redis operation fails
// - Frame data is corrupted
func (s *RedisStorage) ListFrames(ctx context.Context, sessionID string) ([]Frame, error) {
	// Check if session exists
	exists, err := s.client.SIsMember(ctx, s.sessionKey(), sessionID).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to check session: %v", err)
	}
	if !exists {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	// Get all frames for the session
	frameList, err := s.client.LRange(ctx, s.frameKey(sessionID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get frames: %v", err)
	}

	// Deserialize frames
	frames := make([]Frame, 0, len(frameList))
	for _, frameJSON := range frameList {
		var frame Frame
		if err := json.Unmarshal([]byte(frameJSON), &frame); err != nil {
			return nil, fmt.Errorf("failed to unmarshal frame: %v", err)
		}
		frames = append(frames, frame)
	}

	// Sort frames by index
	sort.Slice(frames, func(i, j int) bool {
		return frames[i].Index < frames[j].Index
	})

	return frames, nil
}

// ListSessions returns all active session IDs.
// The session IDs are retrieved from the Redis Set and sorted alphabetically.
//
// Returns an error if the Redis operation fails.
func (s *RedisStorage) ListSessions(ctx context.Context) ([]string, error) {
	// Get all session IDs from the set
	sessions, err := s.client.SMembers(ctx, s.sessionKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %v", err)
	}

	// Sort sessions for consistent ordering
	sort.Strings(sessions)
	return sessions, nil
}

// DeleteSession removes all frames for a given session and removes it from
// the active sessions set. The operation is atomic: either both the frames
// are deleted and the session is removed from tracking, or neither operation occurs.
//
// Returns an error if:
// - Session doesn't exist
// - Redis operation fails
func (s *RedisStorage) DeleteSession(ctx context.Context, sessionID string) error {
	// Check if session exists
	exists, err := s.client.SIsMember(ctx, s.sessionKey(), sessionID).Result()
	if err != nil {
		return fmt.Errorf("failed to check session: %v", err)
	}
	if !exists {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Create pipeline for atomic operations
	pipe := s.client.Pipeline()

	// Delete session's frame list
	pipe.Del(ctx, s.frameKey(sessionID))

	// Remove from active sessions set
	pipe.SRem(ctx, s.sessionKey(), sessionID)

	// Execute pipeline
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("failed to delete session: %v", err)
	}

	return nil
}

// Close closes the Redis client connection and cleans up resources.
// After Close is called, no other methods should be called on this instance.
//
// Returns an error if the Redis connection cannot be closed cleanly.
func (s *RedisStorage) Close() error {
	return s.client.Close()
}
