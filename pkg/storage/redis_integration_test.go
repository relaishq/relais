package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

func requireRedis(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	// Try to connect
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Skipf("skipping: cannot connect to redis at %s: %v", addr, err)
	}
	_ = rs.Close()
	return addr
}

func TestStreamsWriteReadAndCursorResume(t *testing.T) {
	addr := requireRedis(t)
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	ctx := context.Background()
	session := "test-sess-streams"
	participant := "p1"

	// Write a few frames
	for i := 0; i < 3; i++ {
		f := Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}
		if err := rs.PutFrame(ctx, f); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	// Read from session stream from beginning ($ means only new; use 0-0)
	frames, lastID, err := rs.ReadStream(ctx, session, "0-0", 10, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(frames) < 3 {
		t.Fatalf("expected >=3 frames, got %d", len(frames))
	}
	if lastID == "" {
		t.Fatalf("expected lastID")
	}

	// Persist cursor and resume
	if err := rs.SetStreamCursor(ctx, session, participant, "session", lastID); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	cid, err := rs.GetStreamCursor(ctx, session, participant, "session")
	if err != nil {
		t.Fatalf("get cursor: %v", err)
	}
	if cid != lastID {
		t.Fatalf("cursor mismatch: %s != %s", cid, lastID)
	}

	// Write another frame and ensure resume reads only new
	f := Frame{SessionID: session, Index: 99, MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}
	if err := rs.PutFrame(ctx, f); err != nil {
		t.Fatalf("put new: %v", err)
	}

	frames2, lastID2, err := rs.ReadStream(ctx, session, cid, 10, 2*time.Second)
	if err != nil {
		t.Fatalf("resume read: %v", err)
	}
	if len(frames2) < 1 {
		t.Fatalf("expected new frames after resume, got %d", len(frames2))
	}
	if lastID2 == cid {
		t.Fatalf("lastID did not advance")
	}
}
