package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/relais/pkg/metrics"
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
	client           redis.UniversalClient // Redis client connection (single or cluster)
	prefix           string                // Key prefix for namespacing (e.g., "myapp:")
	sessionMaxFrames int                   // Trim window for session-wide list
	trackMaxFrames   int                   // Trim window for per-track list
	enableStreams    bool                  // If true, also write frames to Redis Streams
	streamMaxLen     int64                 // Approximate MAXLEN for streams; <=0 disables trimming
	// Streams consumer group config
	groupEnabled bool   // If true, use XREADGROUP instead of XREAD
	groupName    string // Consumer group name
	consumerName string // Default consumer name (can be overridden per-call)
}

// ClaimPending claims pending messages from the session stream's PEL for this group.
// It wraps XCLAIM and records Prometheus metrics for outcomes and claimed count.
// If consumer is empty, the storage default consumer is used.
func (s *RedisStorage) ClaimPending(ctx context.Context, sessionID, consumer string, ids []string, minIdle time.Duration) ([]string, error) {
	if !s.groupEnabled || len(ids) == 0 {
		return nil, nil
	}
	cons := consumer
	if cons == "" {
		cons = s.consumerName
	}
	args := &redis.XClaimArgs{
		Stream:   s.streamKey(sessionID),
		Group:    s.groupName,
		Consumer: cons,
		MinIdle:  minIdle,
		Messages: ids,
	}
	t0 := time.Now()
	msgs, err := s.client.XClaim(ctx, args).Result()
	metrics.RedisOpDuration.WithLabelValues("xclaim").Observe(time.Since(t0).Seconds())
	if err != nil {
		metrics.RedisErrors.WithLabelValues("xclaim", classifyRedisErr(err)).Inc()
		metrics.RedisGroupClaims.WithLabelValues("error").Inc()
		return nil, err
	}
	if len(msgs) == 0 {
		metrics.RedisGroupClaims.WithLabelValues("empty").Inc()
		return nil, nil
	}
	metrics.RedisGroupClaims.WithLabelValues("success").Inc()
	metrics.RedisGroupClaimMessages.Add(float64(len(msgs)))
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out, nil
}

// classifyRedisErr returns a coarse error category for Prometheus labels.
func classifyRedisErr(err error) string {
	if err == nil {
		return ""
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return "canceled"
	}
	// net errors
	if ne, ok := err.(net.Error); ok {
		if ne.Timeout() {
			return "timeout"
		}
		return "net"
	}
	s := strings.ToUpper(err.Error())
	switch {
	case strings.Contains(s, "MOVED"):
		return "moved"
	case strings.Contains(s, "ASK"):
		return "ask"
	case strings.Contains(s, "TRYAGAIN"):
		return "tryagain"
	case strings.Contains(s, "BUSY"):
		return "busy"
	case strings.Contains(s, "CLUSTERDOWN"):
		return "clusterdown"
	case strings.Contains(s, "EOF"):
		return "eof"
	default:
		return "other"
	}
}

func isRetryable(kind string) bool {
	switch kind {
	case "timeout", "net", "eof", "moved", "ask", "tryagain", "busy", "clusterdown":
		return true
	default:
		return false
	}
}

// withRetry wraps a Redis op with capped exponential backoff + jitter and metrics.
func (s *RedisStorage) withRetry(ctx context.Context, op string, fn func() error) error {
	backoff := 20 * time.Millisecond
	const maxBackoff = 500 * time.Millisecond
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := time.Now()
		err := fn()
		metrics.RedisOpDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// Context canceled; surface original error
			return err
		}
		kind := classifyRedisErr(err)
		// Do not count redis.Nil as an error here; callers handle that case.
		if err != redis.Nil {
			metrics.RedisErrors.WithLabelValues(op, kind).Inc()
		}
		if attempt == maxAttempts || !isRetryable(kind) {
			return err
		}
		metrics.RedisRetries.WithLabelValues(op).Inc()
		// jitter in [0, backoff/2]
		jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
		sleep := backoff + jitter
		if sleep > maxBackoff {
			sleep = maxBackoff
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	return fmt.Errorf("unreachable")
}

// sessionTag returns a Redis Cluster hash tag to co-locate all keys for a session
func sessionTag(sessionID string) string {
	return "{sess:" + sessionID + "}"
}

// Stream cursor helpers for resume across reconnects.
func (s *RedisStorage) streamCursorKey(sessionID, participantID, name string) string {
	return fmt.Sprintf("%sstream_cursor:%s:%s:%s", s.prefix, sessionID, participantID, name)
}

// GetStreamCursor returns stored lastID for a given cursor key.
func (s *RedisStorage) GetStreamCursor(ctx context.Context, sessionID, participantID, name string) (string, error) {
	v, err := s.client.Get(ctx, s.streamCursorKey(sessionID, participantID, name)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

// SetStreamCursor persists lastID for a given cursor key.
func (s *RedisStorage) SetStreamCursor(ctx context.Context, sessionID, participantID, name, lastID string) error {
	return s.client.Set(ctx, s.streamCursorKey(sessionID, participantID, name), lastID, 0).Err()
}

// ReadTrackStream reads frames from the per-track stream using XREAD.
// Behavior is the same as ReadStream but restricted to a track ID.
func (s *RedisStorage) ReadTrackStream(ctx context.Context, sessionID, trackID, lastID string, count int, block time.Duration) ([]Frame, string, error) {
	if lastID == "" {
		lastID = "$"
	}
	args := &redis.XReadArgs{
		Streams: []string{s.streamTrackKey(sessionID, trackID), lastID},
	}
	if count > 0 {
		args.Count = int64(count)
	}
	if block > 0 {
		args.Block = block
	}
	t0 := time.Now()
	res, err := s.client.XRead(ctx, args).Result()
	metrics.RedisOpDuration.WithLabelValues("xread_track").Observe(time.Since(t0).Seconds())
	if err != nil {
		if err == redis.Nil {
			return nil, lastID, nil
		}
		metrics.RedisErrors.WithLabelValues("xread_track", classifyRedisErr(err)).Inc()
		return nil, lastID, fmt.Errorf("xread track error: %v", err)
	}
	out := make([]Frame, 0)
	nextID := lastID
	for _, str := range res {
		for _, msg := range str.Messages {
			var f Frame
			if raw, ok := msg.Values["data"].(string); ok {
				if err := json.Unmarshal([]byte(raw), &f); err == nil {
					out = append(out, f)
				}
			}
			nextID = msg.ID
		}
	}
	return out, nextID, nil
}

// EnableStreams toggles Redis Streams writes and sets optional max length.
// If maxLen <= 0, stream trimming is disabled.
func (s *RedisStorage) EnableStreams(enable bool, maxLen int64) {
	s.enableStreams = enable
	s.streamMaxLen = maxLen
}

// StreamsEnabled returns true if Redis Streams writes are enabled.
func (s *RedisStorage) StreamsEnabled() bool { return s.enableStreams }

// EnableStreamGroups toggles use of Redis Stream consumer groups and sets defaults.
// If group is empty, defaults to "relais". If consumer is empty, callers should
// pass a consumer per call (e.g., participant ID).
func (s *RedisStorage) EnableStreamGroups(enable bool, group, consumer string) {
	s.groupEnabled = enable
	if group == "" {
		group = "relais"
	}
	s.groupName = group
	s.consumerName = consumer
}

// StreamsGroupEnabled returns true if group reads are enabled.
func (s *RedisStorage) StreamsGroupEnabled() bool { return s.groupEnabled }

// ensureGroup creates a consumer group for the given stream if it does not exist.
func (s *RedisStorage) ensureGroup(ctx context.Context, stream string) error {
	if !s.groupEnabled || s.groupName == "" {
		return nil
	}
	// MKSTREAM creates stream if missing. Ignore BUSYGROUP errors.
	err := s.client.XGroupCreateMkStream(ctx, stream, s.groupName, "$").Err()
	if err == nil {
		metrics.RedisGroupEnsure.WithLabelValues("created").Inc()
		return nil
	}
	up := strings.ToUpper(err.Error())
	if strings.Contains(up, "BUSYGROUP") {
		metrics.RedisGroupEnsure.WithLabelValues("exists").Inc()
		return nil
	}
	metrics.RedisGroupEnsure.WithLabelValues("error").Inc()
	return fmt.Errorf("xgroup create failed: %v", err)
}

// AckStream acknowledges messages by ID on the session stream for a given session.
func (s *RedisStorage) AckStream(ctx context.Context, sessionID string, ids []string) error {
	if len(ids) == 0 || !s.groupEnabled {
		return nil
	}
	t0 := time.Now()
	err := s.client.XAck(ctx, s.streamKey(sessionID), s.groupName, ids...).Err()
	metrics.RedisOpDuration.WithLabelValues("xack").Observe(time.Since(t0).Seconds())
	if err != nil {
		metrics.RedisErrors.WithLabelValues("xack", classifyRedisErr(err)).Inc()
		return err
	}
	metrics.RedisGroupAcks.WithLabelValues("session").Inc()
	metrics.RedisGroupAckMessages.WithLabelValues("session").Add(float64(len(ids)))
	return nil
}

// AckTrackStream acknowledges messages by ID on a per-track stream.
func (s *RedisStorage) AckTrackStream(ctx context.Context, sessionID, trackID string, ids []string) error {
	if len(ids) == 0 || !s.groupEnabled {
		return nil
	}
	key := s.streamTrackKey(sessionID, trackID)
	t0 := time.Now()
	err := s.client.XAck(ctx, key, s.groupName, ids...).Err()
	metrics.RedisOpDuration.WithLabelValues("xack_track").Observe(time.Since(t0).Seconds())
	if err != nil {
		metrics.RedisErrors.WithLabelValues("xack_track", classifyRedisErr(err)).Inc()
		return err
	}
	metrics.RedisGroupAcks.WithLabelValues("track").Inc()
	metrics.RedisGroupAckMessages.WithLabelValues("track").Add(float64(len(ids)))
	return nil
}

// ReadStreamGroup reads new entries using XREADGROUP from the session stream.
// Consumer may be empty to use the storage default consumer name.
func (s *RedisStorage) ReadStreamGroup(ctx context.Context, sessionID, consumer string, count int, block time.Duration) ([]Frame, []string, error) {
	if !s.groupEnabled {
		return nil, nil, fmt.Errorf("stream groups not enabled")
	}
	if err := s.ensureGroup(ctx, s.streamKey(sessionID)); err != nil {
		return nil, nil, err
	}
	cons := consumer
	if cons == "" {
		cons = s.consumerName
	}
	args := &redis.XReadGroupArgs{
		Group:    s.groupName,
		Consumer: cons,
		Streams:  []string{s.streamKey(sessionID), ">"},
	}
	if count > 0 {
		args.Count = int64(count)
	}
	if block > 0 {
		args.Block = block
	}
	t0 := time.Now()
	res, err := s.client.XReadGroup(ctx, args).Result()
	metrics.RedisOpDuration.WithLabelValues("xreadgroup").Observe(time.Since(t0).Seconds())
	if err != nil {
		if err == redis.Nil {
			return nil, nil, nil
		}
		metrics.RedisErrors.WithLabelValues("xreadgroup", classifyRedisErr(err)).Inc()
		return nil, nil, fmt.Errorf("xreadgroup error: %v", err)
	}
	metrics.RedisGroupReadOps.WithLabelValues("session").Inc()
	out := make([]Frame, 0)
	ids := make([]string, 0)
	for _, str := range res {
		for _, msg := range str.Messages {
			var f Frame
			if raw, ok := msg.Values["data"].(string); ok {
				if err := json.Unmarshal([]byte(raw), &f); err == nil {
					out = append(out, f)
					ids = append(ids, msg.ID)
				}
			}
		}
	}
	if n := len(ids); n > 0 {
		metrics.RedisGroupReadMessages.WithLabelValues("session").Add(float64(n))
	}
	return out, ids, nil
}

// ReadTrackStreamGroup reads new entries using XREADGROUP from a per-track stream.
func (s *RedisStorage) ReadTrackStreamGroup(ctx context.Context, sessionID, trackID, consumer string, count int, block time.Duration) ([]Frame, []string, error) {
	if !s.groupEnabled {
		return nil, nil, fmt.Errorf("stream groups not enabled")
	}
	key := s.streamTrackKey(sessionID, trackID)
	if err := s.ensureGroup(ctx, key); err != nil {
		return nil, nil, err
	}
	cons := consumer
	if cons == "" {
		cons = s.consumerName
	}
	args := &redis.XReadGroupArgs{
		Group:    s.groupName,
		Consumer: cons,
		Streams:  []string{key, ">"},
	}
	if count > 0 {
		args.Count = int64(count)
	}
	if block > 0 {
		args.Block = block
	}
	t0 := time.Now()
	res, err := s.client.XReadGroup(ctx, args).Result()
	metrics.RedisOpDuration.WithLabelValues("xreadgroup_track").Observe(time.Since(t0).Seconds())
	if err != nil {
		if err == redis.Nil {
			return nil, nil, nil
		}
		metrics.RedisErrors.WithLabelValues("xreadgroup_track", classifyRedisErr(err)).Inc()
		return nil, nil, fmt.Errorf("xreadgroup track error: %v", err)
	}
	metrics.RedisGroupReadOps.WithLabelValues("track").Inc()
	out := make([]Frame, 0)
	ids := make([]string, 0)
	for _, str := range res {
		for _, msg := range str.Messages {
			var f Frame
			if raw, ok := msg.Values["data"].(string); ok {
				if err := json.Unmarshal([]byte(raw), &f); err == nil {
					out = append(out, f)
					ids = append(ids, msg.ID)
				}
			}
		}
	}
	if n := len(ids); n > 0 {
		metrics.RedisGroupReadMessages.WithLabelValues("track").Add(float64(n))
	}
	return out, ids, nil
}

// ReadStream reads frames from the session stream using XREAD with optional blocking.
// lastID: use "$" to start from new entries only, or a concrete ID like "1680000000000-0" to resume.
// Returns decoded frames and the last ID seen (to be used for subsequent calls).
func (s *RedisStorage) ReadStream(ctx context.Context, sessionID string, lastID string, count int, block time.Duration) ([]Frame, string, error) {
	if lastID == "" {
		lastID = "$"
	}
	if s.groupEnabled {
		frames, ids, err := s.ReadStreamGroup(ctx, sessionID, "", count, block)
		if err != nil {
			return nil, lastID, err
		}
		if len(ids) > 0 {
			lastID = ids[len(ids)-1]
		}
		return frames, lastID, nil
	}
	args := &redis.XReadArgs{
		Streams: []string{s.streamKey(sessionID), lastID},
	}
	if count > 0 {
		args.Count = int64(count)
	}
	if block > 0 {
		args.Block = block
	}
	t0 := time.Now()
	res, err := s.client.XRead(ctx, args).Result()
	metrics.RedisOpDuration.WithLabelValues("xread").Observe(time.Since(t0).Seconds())
	if err != nil {
		if err == redis.Nil {
			return nil, lastID, nil
		}
		metrics.RedisErrors.WithLabelValues("xread", classifyRedisErr(err)).Inc()
		return nil, lastID, fmt.Errorf("xread error: %v", err)
	}
	out := make([]Frame, 0)
	nextID := lastID
	for _, str := range res {
		for _, msg := range str.Messages {
			// Fields were written as JSON under "data" along with metadata values
			var f Frame
			if raw, ok := msg.Values["data"].(string); ok {
				if err := json.Unmarshal([]byte(raw), &f); err == nil {
					out = append(out, f)
				}
			}
			nextID = msg.ID
		}
	}
	return out, nextID, nil
}

// ListTracks returns known track IDs for a session by reading the track set.
func (s *RedisStorage) ListTracks(ctx context.Context, sessionID string) ([]string, error) {
	// Ensure session exists in active set (optional sanity check)
	// We won't error if not present; simply return empty list
	tracks, err := s.client.SMembers(ctx, s.sessionTracksKey(sessionID)).Result()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("failed to list tracks: %v", err)
	}
	sort.Strings(tracks)
	return tracks, nil
}

// ListFramesSince returns frames with Index >= fromIndex up to limit for a session.
// This MVP implementation scans the session list and filters by Index. For large
// lists, consider storing index in a separate sorted set or using chunked ranges.
func (s *RedisStorage) ListFramesSince(ctx context.Context, sessionID string, fromIndex int64, limit int) ([]Frame, error) {
	// Check if session exists
	exists, err := s.client.SIsMember(ctx, s.sessionKey(), sessionID).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to check session: %v", err)
	}
	if !exists {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	// Get all frames for the session (optimize later)
	frameList, err := s.client.LRange(ctx, s.frameKey(sessionID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get frames: %v", err)
	}

	out := make([]Frame, 0, len(frameList))
	for _, frameJSON := range frameList {
		var f Frame
		if err := json.Unmarshal([]byte(frameJSON), &f); err != nil {
			return nil, fmt.Errorf("failed to unmarshal frame: %v", err)
		}
		if f.Index >= fromIndex {
			out = append(out, f)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}

	// Already ordered by append op but sort to be safe
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

// RedisConfig holds configuration options for RedisStorage.
type RedisConfig struct {
	// Single-node options
	Addr string // Redis server address (e.g., "localhost:6379")
	// Cluster options (if Addrs is non-empty or Cluster is true, cluster client is used)
	Addrs    []string // Redis Cluster addresses
	Cluster  bool     // Force cluster client
	Password string   // Redis password (optional)
	DB       int      // Redis database number (single-node only)
	Prefix   string   // Key prefix for namespacing (optional)
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
	var client redis.UniversalClient

	switch cfg := config.(type) {
	case string:
		// Backward compatibility: treat string as Redis address
		client = redis.NewClient(&redis.Options{Addr: cfg})
	case RedisConfig:
		if cfg.Cluster || len(cfg.Addrs) > 0 {
			addrs := cfg.Addrs
			if len(addrs) == 0 && cfg.Addr != "" {
				addrs = []string{cfg.Addr}
			}
			client = redis.NewClusterClient(&redis.ClusterOptions{
				Addrs:    addrs,
				Password: cfg.Password,
			})
		} else {
			client = redis.NewClient(&redis.Options{
				Addr:     cfg.Addr,
				Password: cfg.Password,
				DB:       cfg.DB,
			})
		}
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
		client:           client,
		prefix:           prefix,
		sessionMaxFrames: 5000,
		trackMaxFrames:   5000,
		enableStreams:    false,
		streamMaxLen:     0,
		groupEnabled:     false,
		groupName:        "",
		consumerName:     "",
	}, nil
}

// SetRetention configures trimming windows for session and track lists.
func (s *RedisStorage) SetRetention(sessionMaxFrames, trackMaxFrames int) {
	if sessionMaxFrames > 0 {
		s.sessionMaxFrames = sessionMaxFrames
	}
	if trackMaxFrames > 0 {
		s.trackMaxFrames = trackMaxFrames
	}
}

// frameKey generates the Redis key for storing frames of a session.
func (s *RedisStorage) frameKey(sessionID string) string {
	return fmt.Sprintf("%s%s:frames:%s", s.prefix, sessionTag(sessionID), sessionID)
}

// trackFrameKey generates the Redis key for storing frames of a specific track within a session.
func (s *RedisStorage) trackFrameKey(sessionID, trackID string) string {
	return fmt.Sprintf("%s%s:frames:%s:%s", s.prefix, sessionTag(sessionID), sessionID, trackID)
}

// sessionKey generates the Redis key for the active sessions set.
func (s *RedisStorage) sessionKey() string {
	return s.prefix + "active_sessions"
}

// sessionTracksKey stores track IDs for a session.
func (s *RedisStorage) sessionTracksKey(sessionID string) string {
	return fmt.Sprintf("%s%s:session_tracks:%s", s.prefix, sessionTag(sessionID), sessionID)
}

// streamKey returns the Redis Stream key for session frames
func (s *RedisStorage) streamKey(sessionID string) string {
	return fmt.Sprintf("%s%s:stream:frames:%s", s.prefix, sessionTag(sessionID), sessionID)
}

// streamTrackKey returns the Redis Stream key for per-track frames
func (s *RedisStorage) streamTrackKey(sessionID, trackID string) string {
	return fmt.Sprintf("%s%s:stream:frames:%s:%s", s.prefix, sessionTag(sessionID), sessionID, trackID)
}

// PutFrame stores a frame in Redis.
// The frame is serialized to JSON and stored in a Redis List.
// The session ID is also added to the active sessions set.
//
// The operation is atomic: either both the frame is stored and the session
// is tracked, or neither operation occurs.
func (s *RedisStorage) PutFrame(ctx context.Context, frame Frame) error {
	return s.withRetry(ctx, "pipeline_exec", func() error {
		// Serialize frame to JSON per-attempt to avoid reusing partially-executed pipelines
		frameJSON, err := json.Marshal(frame)
		if err != nil {
			return fmt.Errorf("failed to marshal frame: %v", err)
		}

		pipe := s.client.Pipeline()

		// Add frame to the session's list
		pipe.RPush(ctx, s.frameKey(frame.SessionID), frameJSON)
		// Optional trim window for session list
		if s.sessionMaxFrames > 0 {
			pipe.LTrim(ctx, s.frameKey(frame.SessionID), int64(-s.sessionMaxFrames), -1)
		}

		// Track the session
		pipe.SAdd(ctx, s.sessionKey(), frame.SessionID)

		// If TrackID present, also push to per-track list and track the track ID
		if frame.TrackID != "" {
			tKey := s.trackFrameKey(frame.SessionID, frame.TrackID)
			pipe.RPush(ctx, tKey, frameJSON)
			if s.trackMaxFrames > 0 {
				pipe.LTrim(ctx, tKey, int64(-s.trackMaxFrames), -1)
			}
			pipe.SAdd(ctx, s.sessionTracksKey(frame.SessionID), frame.TrackID)
		}

		// Optional: write to Redis Streams for low-latency tailing
		if s.enableStreams {
			// Session stream
			sessFields := map[string]interface{}{
				"index":     frame.Index,
				"trackId":   frame.TrackID,
				"mediaType": frame.MediaType,
				"codec":     frame.Codec,
				"rtpTime":   frame.RTPTime,
				"clockRate": frame.ClockRate,
				"data":      frameJSON,
			}
			xaddSess := &redis.XAddArgs{Stream: s.streamKey(frame.SessionID), ID: "*", Values: sessFields}
			if s.streamMaxLen > 0 {
				xaddSess.MaxLen = s.streamMaxLen
				xaddSess.Approx = true
			}
			pipe.XAdd(ctx, xaddSess)
			// Per-track stream
			if frame.TrackID != "" {
				xaddTr := &redis.XAddArgs{Stream: s.streamTrackKey(frame.SessionID, frame.TrackID), ID: "*", Values: sessFields}
				if s.streamMaxLen > 0 {
					xaddTr.MaxLen = s.streamMaxLen
					xaddTr.Approx = true
				}
				pipe.XAdd(ctx, xaddTr)
			}
		}

		t0 := time.Now()
		_, err = pipe.Exec(ctx)
		metrics.RedisOpDuration.WithLabelValues("pipeline_exec").Observe(time.Since(t0).Seconds())
		return err
	})
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
