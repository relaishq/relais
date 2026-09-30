package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Validates XREADGROUP and XACK on the session stream
func TestStreamGroup_ReadAndAck(t *testing.T) {
	addr := requireRedis(t)
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	suffix := time.Now().UnixNano()
	group := fmt.Sprintf("cg-test-%d", suffix)
	rs.EnableStreamGroups(true, group, "c1")
	ctx := context.Background()
	session := fmt.Sprintf("group-sess-1-%d", suffix)

	// Create the consumer group explicitly before producing entries.
	if err := rs.client.XGroupCreateMkStream(ctx, rs.streamKey(session), group, "$").Err(); err != nil {
		// BUSYGROUP is fine if it already exists
		if errStr := err.Error(); errStr != "BUSYGROUP Consumer Group name already exists" && errStr != "BUSYGROUP Consumer Group already exists" {
			// Fallback: tolerate BUSYGROUP substrings for varied servers
			if !strings.Contains(strings.ToUpper(errStr), "BUSYGROUP") {
				t.Fatalf("xgroup create: %v", err)
			}
		}
	}

	// Seed frames after group creation
	for i := 0; i < 4; i++ {
		f := Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}
		if err := rs.PutFrame(ctx, f); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	frames, ids, err := rs.ReadStreamGroup(ctx, session, "c1", 10, 0)
	if err != nil {
		t.Fatalf("readgroup: %v", err)
	}
	if len(frames) < 4 {
		t.Fatalf("expected >=4 frames, got %d", len(frames))
	}
	if len(ids) != len(frames) {
		t.Fatalf("ids != frames: %d vs %d", len(ids), len(frames))
	}

	if err := rs.AckStream(ctx, session, ids); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// Pending for the group should be 0 after ack
	xp, err := rs.client.XPending(ctx, rs.streamKey(session), group).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if xp != nil && xp.Count != 0 {
		// Some Redis versions may report nil; if non-nil, enforce zero
		t.Fatalf("expected 0 pending, got %d", xp.Count)
	}
}

// Ensures two consumers in the same group receive distinct messages without duplicates
func TestStreamGroup_MultiConsumerDistribution(t *testing.T) {
	addr := requireRedis(t)
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	suffix := time.Now().UnixNano()
	group := fmt.Sprintf("cg-multi-%d", suffix)
	consumer1 := "c1"
	consumer2 := "c2"
	rs.EnableStreamGroups(true, group, consumer1)

	ctx := context.Background()
	session := fmt.Sprintf("group-sess-multi-%d", suffix)

	// Create the consumer group explicitly
	if err := rs.client.XGroupCreateMkStream(ctx, rs.streamKey(session), group, "$").Err(); err != nil {
		if errStr := err.Error(); errStr != "BUSYGROUP Consumer Group name already exists" && errStr != "BUSYGROUP Consumer Group already exists" {
			if !strings.Contains(strings.ToUpper(errStr), "BUSYGROUP") {
				t.Fatalf("xgroup create: %v", err)
			}
		}
	}

	// Produce N messages
	const N = 12
	for i := 0; i < N; i++ {
		f := Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}
		if err := rs.PutFrame(ctx, f); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	seen := make(map[string]bool)
	idsByC1 := []string{}
	idsByC2 := []string{}

	// Read with both consumers until we collect all N or hit attempts
	attempts := 0
	for len(seen) < N && attempts < 30 {
		attempts++
		if frames, ids, err := rs.ReadStreamGroup(ctx, session, consumer1, 10, 0); err == nil && len(frames) > 0 {
			for i, id := range ids {
				if seen[id] {
					t.Fatalf("duplicate id delivered to c1: %s", id)
				}
				seen[id] = true
				idsByC1 = append(idsByC1, id)
				_ = frames[i] // ensure access
			}
		}
		if frames, ids, err := rs.ReadStreamGroup(ctx, session, consumer2, 10, 0); err == nil && len(frames) > 0 {
			for i, id := range ids {
				if seen[id] {
					t.Fatalf("duplicate id delivered to c2: %s", id)
				}
				seen[id] = true
				idsByC2 = append(idsByC2, id)
				_ = frames[i]
			}
		}
		if len(seen) < N {
			time.Sleep(5 * time.Millisecond)
		}
	}

	if len(seen) != N {
		t.Fatalf("expected %d unique deliveries, got %d", N, len(seen))
	}
	if len(idsByC1) == 0 || len(idsByC2) == 0 {
		t.Fatalf("expected distribution across consumers, got c1=%d c2=%d", len(idsByC1), len(idsByC2))
	}

	// Ack all via group ack
	allIDs := append([]string{}, idsByC1...)
	allIDs = append(allIDs, idsByC2...)
	if err := rs.AckStream(ctx, session, allIDs); err != nil {
		t.Fatalf("ack all: %v", err)
	}
}

// Ensures unacked messages appear in PEL and can be claimed by another consumer
func TestStreamGroup_RedeliveryAndClaim(t *testing.T) {
	addr := requireRedis(t)
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	suffix := time.Now().UnixNano()
	group := fmt.Sprintf("cg-redeliver-%d", suffix)
	c1 := "c1"
	c2 := "c2"
	rs.EnableStreamGroups(true, group, c1)

	ctx := context.Background()
	session := fmt.Sprintf("group-sess-redeliver-%d", suffix)

	// Create the consumer group explicitly
	if err := rs.client.XGroupCreateMkStream(ctx, rs.streamKey(session), group, "$").Err(); err != nil {
		if errStr := err.Error(); errStr != "BUSYGROUP Consumer Group name already exists" && errStr != "BUSYGROUP Consumer Group already exists" {
			if !strings.Contains(strings.ToUpper(errStr), "BUSYGROUP") {
				t.Fatalf("xgroup create: %v", err)
			}
		}
	}

	// Produce M messages
	const M = 5
	for i := 0; i < M; i++ {
		f := Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}
		if err := rs.PutFrame(ctx, f); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	// c1 reads but does not ack
	_, ids1, err := rs.ReadStreamGroup(ctx, session, c1, M, 0)
	if err != nil {
		t.Fatalf("read c1: %v", err)
	}
	if len(ids1) != M {
		t.Fatalf("expected %d pending ids, got %d", M, len(ids1))
	}

	// Verify pending summary shows M
	xp, err := rs.client.XPending(ctx, rs.streamKey(session), group).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if xp == nil || xp.Count < int64(M) {
		t.Fatalf("expected pending >=%d, got %+v", M, xp)
	}

	// Claim by c2 (MinIdle 0 so we don't rely on sleeping)
	claimedIDs, err := rs.ClaimPending(ctx, session, c2, ids1, 0)
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if len(claimedIDs) != M {
		t.Fatalf("expected %d claimed, got %d", M, len(claimedIDs))
	}

	// Now c2 should be able to ack them
	if err := rs.AckStream(ctx, session, ids1); err != nil {
		t.Fatalf("ack after claim: %v", err)
	}

	// Pending should drop to 0
	xp2, err := rs.client.XPending(ctx, rs.streamKey(session), group).Result()
	if err != nil {
		t.Fatalf("xpending2: %v", err)
	}
	if xp2 != nil && xp2.Count != 0 {
		t.Fatalf("expected 0 pending after ack, got %d", xp2.Count)
	}
}

// Validates XREADGROUP and XACK on a per-track stream
func TestTrackStreamGroup_ReadAndAck(t *testing.T) {
	addr := requireRedis(t)
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	suffix := time.Now().UnixNano()
	group := fmt.Sprintf("cg-test-tr-%d", suffix)
	rs.EnableStreamGroups(true, group, "c1")
	ctx := context.Background()
	session := fmt.Sprintf("group-sess-track-1-%d", suffix)

	// Pre-create track consumer group explicitly
	if err := rs.client.XGroupCreateMkStream(ctx, rs.streamTrackKey(session, "v1"), group, "$").Err(); err != nil {
		if errStr := err.Error(); errStr != "BUSYGROUP Consumer Group name already exists" && errStr != "BUSYGROUP Consumer Group already exists" {
			if !strings.Contains(strings.ToUpper(errStr), "BUSYGROUP") {
				t.Fatalf("xgroup create track: %v", err)
			}
		}
	}

	// Seed mixed frames
	for i := 0; i < 5; i++ {
		if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}); err != nil {
			t.Fatalf("put v1: %v", err)
		}
		if err := rs.PutFrame(ctx, Frame{SessionID: session, Index: int64(100 + i + 1), MediaType: "audio", Codec: "opus", TrackID: "a1", IngestTime: time.Now()}); err != nil {
			t.Fatalf("put a1: %v", err)
		}
	}

	frames, ids, err := rs.ReadTrackStreamGroup(ctx, session, "v1", "c1", 100, 0)
	if err != nil {
		t.Fatalf("readgroup track: %v", err)
	}
	if len(frames) == 0 {
		t.Fatalf("expected frames for v1 track")
	}

	if err := rs.AckTrackStream(ctx, session, "v1", ids); err != nil {
		t.Fatalf("ack track: %v", err)
	}

	// Pending for the group on the track stream should be 0 after ack
	key := rs.streamTrackKey(session, "v1")
	xp, err := rs.client.XPending(ctx, key, group).Result()
	if err != nil {
		t.Fatalf("xpending track: %v", err)
	}
	if xp != nil && xp.Count != 0 {
		t.Fatalf("expected 0 pending for track, got %d", xp.Count)
	}
}

// Verifies that XCLAIM respects MinIdle: early claim returns 0, claim after MinIdle succeeds
func TestStreamGroup_XClaimMinIdle(t *testing.T) {
	addr := requireRedis(t)
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	suffix := time.Now().UnixNano()
	group := fmt.Sprintf("cg-minidle-%d", suffix)
	c1 := "c1"
	c2 := "c2"
	rs.EnableStreamGroups(true, group, c1)

	ctx := context.Background()
	session := fmt.Sprintf("group-sess-minidle-%d", suffix)

	// Create the consumer group explicitly
	if err := rs.client.XGroupCreateMkStream(ctx, rs.streamKey(session), group, "$").Err(); err != nil {
		if errStr := err.Error(); errStr != "BUSYGROUP Consumer Group name already exists" && errStr != "BUSYGROUP Consumer Group already exists" {
			if !strings.Contains(strings.ToUpper(errStr), "BUSYGROUP") {
				t.Fatalf("xgroup create: %v", err)
			}
		}
	}

	// Produce messages
	const M = 3
	for i := 0; i < M; i++ {
		f := Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}
		if err := rs.PutFrame(ctx, f); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	// c1 reads but does not ack
	_, ids1, err := rs.ReadStreamGroup(ctx, session, c1, M, 0)
	if err != nil {
		t.Fatalf("read c1: %v", err)
	}
	if len(ids1) != M {
		t.Fatalf("expected %d pending ids, got %d", M, len(ids1))
	}

	// Try to claim too early with MinIdle = 250ms (should get 0)
	earlyClaimIDs, err := rs.ClaimPending(ctx, session, c2, ids1, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("claim early: %v", err)
	}
	if len(earlyClaimIDs) != 0 {
		t.Fatalf("expected 0 early-claimed, got %d", len(earlyClaimIDs))
	}

	// Wait past MinIdle and claim again
	time.Sleep(300 * time.Millisecond)
	claimedIDs2, err := rs.ClaimPending(ctx, session, c2, ids1, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("claim after sleep: %v", err)
	}
	if len(claimedIDs2) != M {
		t.Fatalf("expected %d claimed after MinIdle, got %d", M, len(claimedIDs2))
	}

	// Ack them to clean up PEL
	if err := rs.AckStream(ctx, session, ids1); err != nil {
		t.Fatalf("ack after claim: %v", err)
	}
}

// Verifies ensureGroup auto-creates group when reading without explicit XGROUP
func TestStreamGroup_AutoEnsureGroup(t *testing.T) {
	addr := requireRedis(t)
	rs, err := NewRedisStorage(RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })

	rs.EnableStreams(true, 1000)
	suffix := time.Now().UnixNano()
	group := fmt.Sprintf("cg-autocreate-%d", suffix)
	consumer := "c1"
	rs.EnableStreamGroups(true, group, consumer)

	ctx := context.Background()
	session := fmt.Sprintf("group-sess-auto-%d", suffix)

	// Do NOT pre-create group; first read should trigger ensureGroup and return no frames
	// Use a short context timeout and small block to avoid any driver/server-specific indefinite waits
	ctxShort, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	frames0, ids0, err := rs.ReadStreamGroup(ctxShort, session, consumer, 10, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("initial readgroup: %v", err)
	}
	if len(frames0) != 0 || len(ids0) != 0 {
		t.Fatalf("expected no frames on initial read after ensureGroup, got %d", len(frames0))
	}

	// Now produce frames and read again; should deliver
	for i := 0; i < 4; i++ {
		f := Frame{SessionID: session, Index: int64(i + 1), MediaType: "video", Codec: "h264", TrackID: "v1", IngestTime: time.Now()}
		if err := rs.PutFrame(ctx, f); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	// Non-blocking read should succeed since frames exist; keep a small block to be robust
	frames, ids, err := rs.ReadStreamGroup(ctx, session, consumer, 10, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("readgroup after produce: %v", err)
	}
	if len(frames) < 4 {
		t.Fatalf("expected >=4 frames after produce, got %d", len(frames))
	}
	if len(ids) != len(frames) {
		t.Fatalf("ids != frames: %d vs %d", len(ids), len(frames))
	}

	if err := rs.AckStream(ctx, session, ids); err != nil {
		t.Fatalf("ack: %v", err)
	}
}
