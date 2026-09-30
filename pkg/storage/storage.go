// Package storage defines the interface and implementations for frame storage.
// This package provides a unified way to store and retrieve media frames,
// supporting different backend implementations like in-memory and Redis.
package storage

import (
	"context"
	"time"
)

// Frame represents a media frame with metadata.
// It contains both the raw frame data and associated information needed for
// proper playback and processing. Frames are the fundamental unit of media
// in the Relais system.
type Frame struct {
	SessionID  string    // Unique identifier for the media session this frame belongs to
	Index      int64     // Sequential frame number within the session, used for ordering
	Data       []byte    // Raw frame data (encoded video/audio) in the specified codec format
	Timestamp  time.Time // When the frame was captured/created, used for synchronization
	IngestTime time.Time // Server ingest time for cross-track ordering and resume
	MediaType  string    // Type of media ("video" or "audio")
	Codec      string    // Codec used for encoding (e.g., "h264", "opus", "jpeg")
	KeyFrame   bool      // Whether this is a key frame (for video), important for seeking
	// Optional per-track metadata for WebRTC-aware storage
	TrackID   string // WebRTC track ID
	SSRC      uint32 // RTP SSRC
	ClockRate uint32 // RTP ClockRate for the codec
	RTPTime   uint32 // RTP timestamp for this frame/packet
}

// Storage defines the interface for frame storage backends.
// Implementations must be thread-safe and handle concurrent access from
// multiple goroutines. The interface is designed to be simple yet flexible
// enough to support different storage backends (e.g., memory, Redis, S3).
type Storage interface {
	// PutFrame stores a frame for a given session.
	// This method should be optimized for high-throughput writes as it's
	// called frequently by ingress plugins.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - frame: The frame to store with all metadata
	//
	// Returns an error if:
	//   - The storage operation fails
	//   - The context is cancelled
	//   - The backend is unavailable
	PutFrame(ctx context.Context, frame Frame) error

	// GetFrame retrieves a specific frame by session and index.
	// This method is used when precise frame access is needed,
	// such as seeking to a specific point in the media.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - sessionID: Unique identifier for the media session
	//   - frameIndex: Sequential number of the frame to retrieve
	//
	// Returns:
	//   - The requested frame and nil error if successful
	//   - Empty frame and error if:
	//     * Frame not found
	//     * Session not found
	//     * Storage error occurs
	GetFrame(ctx context.Context, sessionID string, frameIndex int64) (Frame, error)

	// ListFrames returns all frames for a session.
	// This method should implement efficient pagination or streaming
	// if the number of frames is large.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - sessionID: Unique identifier for the media session
	//
	// Returns:
	//   - Slice of frames ordered by Index
	//   - Error if session not found or storage error occurs
	ListFrames(ctx context.Context, sessionID string) ([]Frame, error)

	// ListFramesSince returns frames for a session with Index >= fromIndex, up to limit.
	// Implementations should optimize to avoid scanning entire history when possible.
	// If limit <= 0, implementation may choose a reasonable default or return all.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - sessionID: Unique identifier for the media session
	//   - fromIndex: starting inclusive frame index
	//   - limit: maximum number of frames to return (<=0 means no limit)
	//
	// Returns:
	//   - Slice of frames ordered by Index
	//   - Error if session not found or storage error occurs
	ListFramesSince(ctx context.Context, sessionID string, fromIndex int64, limit int) ([]Frame, error)

	// ListSessions returns all active session IDs.
	// This method is used to discover available media sessions,
	// typically by transform plugins that need to process all sessions.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//
	// Returns:
	//   - Slice of session IDs
	//   - Error if storage error occurs
	ListSessions(ctx context.Context) ([]string, error)

	// ListTracks returns known track IDs for a session if the backend tracks them.
	// Backends that don't maintain explicit track sets may derive from stored frames.
	// Returns an empty slice if no tracks are known.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - sessionID: session to list tracks for
	//
	// Returns:
	//   - Slice of track IDs (unique)
	//   - Error if session not found or storage error occurs
	ListTracks(ctx context.Context, sessionID string) ([]string, error)

	// DeleteSession removes all frames for a session.
	// This should be called when a session is complete to free up resources.
	// The implementation should ensure atomic deletion of all session data.
	//
	// Parameters:
	//   - ctx: Context for cancellation and timeouts
	//   - sessionID: Unique identifier for the media session to delete
	//
	// Returns an error if:
	//   - Session not found
	//   - Deletion fails
	//   - Context is cancelled
	DeleteSession(ctx context.Context, sessionID string) error

	// Close cleans up any resources used by the storage backend.
	// This should be called when the storage is no longer needed.
	// After Close is called, no other methods should be called.
	//
	// Returns an error if cleanup fails.
	Close() error
}
