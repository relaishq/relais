package storage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func requireRedisCluster(t *testing.T) []string {
	t.Helper()
	addrCSV := os.Getenv("RELAIS_TEST_REDIS_CLUSTER_ADDRS")
	if addrCSV == "" {
		t.Skip("skipping: RELAIS_TEST_REDIS_CLUSTER_ADDRS not set")
	}
	addrs := strings.Split(addrCSV, ",")
	if len(addrs) == 0 {
		t.Skip("skipping: no cluster addrs provided")
	}
	// Try to connect
	rs, err := NewRedisStorage(RedisConfig{Addrs: addrs, Cluster: true})
	if err != nil {
		t.Skipf("skipping: cannot connect to cluster: %v", err)
	}
	_ = rs.Close()
	return addrs
}

func TestClusterStreamsAndLists(t *testing.T) {
	addrs := requireRedisCluster(t)
	_ = addrs
	rs, err := NewRedisStorage(RedisConfig{Addrs: addrs, Cluster: true})
	if err != nil {
		t.Fatalf("new redis cluster: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	rs.SetRetention(10, 10)
	ctx := context.Background()
	session := "cluster-sess-1"

	// Write frames for two tracks
	for i := 0; i < 5; i++ {
		if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}); err != nil {
			t.Fatalf("put v1: %v", err)
		}
		if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: int64(100 + i + 1), MediaType: "audio", Codec: "opus", TrackID: "a1", IngestTime: time.Now()}); err != nil {
			t.Fatalf("put a1: %v", err)
		}
	}

	// Verify track listing
	tracks, err := rs.ListTracks(ctx, session)
	if err != nil {
		t.Fatalf("list tracks: %v", err)
	}
	if len(tracks) < 2 {
		t.Fatalf("expected >=2 tracks, got %v", tracks)
	}

	// Read from session stream from the beginning
	frames, lastID, err := rs.ReadStream(ctx, session, "0-0", 20, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("expected frames from stream")
	}
	if lastID == "" {
		t.Fatalf("expected lastID")
	}
}

// Ensure all per-session keys hash to the same Redis Cluster slot via hash-tagging
func TestClusterKeySlotAffinity(t *testing.T) {
	addrs := requireRedisCluster(t)
	rs, err := NewRedisStorage(RedisConfig{Addrs: addrs, Cluster: true})
	if err != nil {
		t.Fatalf("new redis cluster: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	ctx := context.Background()
	session := "slot-sess-1"

	// Generate representative keys for the session
	kSessStream := rs.streamKey(session)
	kSessList := rs.frameKey(session)
	kTracks := rs.sessionTracksKey(session)
	kTrackStream := rs.streamTrackKey(session, "v1")

	// KEYSLOT for each key must match
	slot := func(key string) int {
		n, err := rs.client.Do(ctx, "CLUSTER", "KEYSLOT", key).Int()
		if err != nil {
			t.Fatalf("CLUSTER KEYSLOT %s: %v", key, err)
		}
		return n
	}
	s1 := slot(kSessStream)
	if s1 != slot(kSessList) || s1 != slot(kTracks) || s1 != slot(kTrackStream) {
		t.Fatalf("keys not co-located: %d, %d, %d, %d", s1, slot(kSessList), slot(kTracks), slot(kTrackStream))
	}
}

// Verify cursor persistence and resume on cluster
func TestClusterCursorResume(t *testing.T) {
	addrs := requireRedisCluster(t)
	rs, err := NewRedisStorage(RedisConfig{Addrs: addrs, Cluster: true})
	if err != nil {
		t.Fatalf("new redis cluster: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	ctx := context.Background()
	session := "cluster-resume-sess"
	participant := "p1"

	// Seed frames
	for i := 0; i < 3; i++ {
		if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	frames, lastID, err := rs.ReadStream(ctx, session, "0-0", 10, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(frames) < 3 || lastID == "" {
		t.Fatalf("expected initial frames and lastID")
	}

	if err := rs.SetStreamCursor(ctx, session, participant, "session", lastID); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	got, err := rs.GetStreamCursor(ctx, session, participant, "session")
	if err != nil || got != lastID {
		t.Fatalf("cursor get mismatch: %s vs %s (err=%v)", got, lastID, err)
	}

	// Write one new frame and ensure resume only gets new
	if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: 99, MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}); err != nil {
		t.Fatalf("put new: %v", err)
	}

	frames2, lastID2, err := rs.ReadStream(ctx, session, got, 10, 2*time.Second)
	if err != nil {
		t.Fatalf("resume read: %v", err)
	}
	if len(frames2) < 1 {
		t.Fatalf("expected new frames after resume")
	}
	if lastID2 == got {
		t.Fatalf("lastID did not advance on resume")
	}
}

// Verify per-track stream reads only return frames for the track
func TestClusterPerTrackStreamReads(t *testing.T) {
	addrs := requireRedisCluster(t)
	rs, err := NewRedisStorage(RedisConfig{Addrs: addrs, Cluster: true})
	if err != nil {
		t.Fatalf("new redis cluster: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	ctx := context.Background()
	session := "cluster-track-sess"

	// Write mixed tracks
	for i := 0; i < 4; i++ {
		if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}); err != nil {
			t.Fatalf("put v1: %v", err)
		}
		if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: int64(100 + i + 1), MediaType: "audio", Codec: "opus", TrackID: "a1", IngestTime: time.Now()}); err != nil {
			t.Fatalf("put a1: %v", err)
		}
	}

	// Read only v1 from its track stream
	tf, _, err := rs.ReadTrackStream(ctx, session, "v1", "0-0", 100, 0)
	if err != nil {
		t.Fatalf("read track stream: %v", err)
	}
	if len(tf) == 0 {
		t.Fatalf("expected track frames")
	}
	for _, f := range tf {
		if f.TrackID != "v1" {
			t.Fatalf("unexpected track id in track stream: %s", f.TrackID)
		}
	}

	// Session stream should yield combined frames
	sf, _, err := rs.ReadStream(ctx, session, "0-0", 200, 0)
	if err != nil {
		t.Fatalf("read session stream: %v", err)
	}
	if len(sf) < len(tf) {
		t.Fatalf("session stream should have at least as many frames as track stream")
	}
}
