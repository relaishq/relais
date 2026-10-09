package framecache

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var testTrack = Track{Kind: "video", SSRC: 7}

func testFrame(seq uint16, key bool) Frame {
	return Frame{Track: testTrack, Timestamp: uint32(seq) * 3000, Keyframe: key, Packets: []Packet{{SequenceNumber: seq, Marker: true, Payload: []byte{byte(seq)}}}}
}

func TestMemoryCurrentGroupBound(t *testing.T) {
	m := NewMemory(Limits{})
	ctx := context.Background()
	for seq := uint16(1); seq <= 1000; seq++ {
		require.NoError(t, m.Append(ctx, "s", testFrame(seq, seq%10 == 1)))
		g := m.sessions["s"].tracks[testTrack]
		require.LessOrEqual(t, len(g.current.frames), 20)
		require.LessOrEqual(t, len(g.current.frames), 10)
	}
	frames, err := m.Current(ctx, "s", testTrack)
	require.NoError(t, err)
	require.Len(t, frames, 10)
	require.Equal(t, uint32(991*3000), frames[0].Timestamp)
	require.True(t, frames[0].Keyframe)
}

func TestMemoryCapsDropWholeGroups(t *testing.T) {
	for _, limits := range []Limits{{Bytes: 3, Frames: 100}, {Bytes: 100, Frames: 3}} {
		m := NewMemory(limits)
		ctx := context.Background()
		for seq := uint16(1); seq <= 7; seq++ {
			require.NoError(t, m.Append(ctx, "s", testFrame(seq, seq == 1 || seq == 3)))
			g := m.sessions["s"].tracks[testTrack]
			require.LessOrEqual(t, g.current.bytes, limits.Bytes)
			require.LessOrEqual(t, len(g.current.frames), limits.Frames)
		}
		frames, err := m.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames, "overflow never leaves a truncated current group")
		require.NoError(t, m.Append(ctx, "s", testFrame(8, true)))
		frames, err = m.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 1)
	}
}

func TestMemoryCopiesIsolationAndMissingReferences(t *testing.T) {
	m := NewMemory(Limits{})
	ctx := context.Background()
	f := testFrame(1, true)
	require.NoError(t, m.Append(ctx, "a", f))
	f.Packets[0].Payload[0] = 99
	frames, err := m.Current(ctx, "a", testTrack)
	require.NoError(t, err)
	require.Equal(t, byte(1), frames[0].Packets[0].Payload[0])
	frames[0].Packets[0].Payload[0] = 98
	frames, err = m.Current(ctx, "a", testTrack)
	require.NoError(t, err)
	require.Equal(t, byte(1), frames[0].Packets[0].Payload[0])
	require.NoError(t, m.Append(ctx, "b", testFrame(1, true)))
	other := testFrame(1, true)
	other.Track.SSRC++
	require.NoError(t, m.Append(ctx, "a", other))
	require.NoError(t, m.Append(ctx, "a", testFrame(3, false)))
	frames, err = m.Current(ctx, "a", testTrack)
	require.NoError(t, err)
	require.Empty(t, frames)
	frames, err = m.Current(ctx, "a", other.Track)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	require.NoError(t, m.DeleteSession(ctx, "a"))
	frames, err = m.Current(ctx, "a", other.Track)
	require.NoError(t, err)
	require.Empty(t, frames)
	frames, err = m.Current(ctx, "b", testTrack)
	require.NoError(t, err)
	require.Len(t, frames, 1)
}

func TestMemoryRejectsPartialAndCancelled(t *testing.T) {
	m := NewMemory(Limits{})
	ctx := context.Background()
	require.NoError(t, m.Append(ctx, "s", testFrame(1, true)))
	partial := testFrame(2, true)
	partial.Packets[0].Marker = false
	require.ErrorIs(t, m.Append(ctx, "s", partial), ErrInvalidFrame)
	frames, err := m.Current(ctx, "s", testTrack)
	require.NoError(t, err)
	require.Equal(t, testFrame(1, true), frames[0])
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, m.Append(cancelled, "s", testFrame(2, true)), context.Canceled)
	_, err = m.Current(cancelled, "s", testTrack)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, m.DeleteSession(cancelled, "s"), context.Canceled)
}

func TestMemoryConcurrent(t *testing.T) {
	m := NewMemory(Limits{})
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for seq := uint16(1); seq < 100; seq++ {
				_ = m.Append(context.Background(), "s", testFrame(seq, true))
				_, _ = m.Current(context.Background(), "s", testTrack)
				_ = m.DeleteSession(context.Background(), "s")
			}
		})
	}
	wg.Wait()
}

func TestMemoryGlobalEvictionAndIdleTTL(t *testing.T) {
	for _, limits := range []Limits{{TotalBytes: 2}, {Sessions: 2}} {
		m := NewMemory(limits)
		ctx := context.Background()
		for _, id := range []string{"old", "middle", "new"} {
			require.NoError(t, m.Append(ctx, id, testFrame(1, true)))
		}
		frames, err := m.Current(ctx, "old", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames)
		require.Len(t, m.sessions, 2)
		// Advance only this cache's activity clock; no timing-sensitive sleep.
		m.mu.Lock()
		m.sessions["middle"].touched = time.Now().Add(-31 * time.Second)
		m.mu.Unlock()
		frames, err = m.Current(ctx, "middle", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames)
		frames, err = m.Current(ctx, "new", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 1)
	}
}

func BenchmarkAppend(b *testing.B) {
	for _, count := range []int{10, 256, 1024} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			cache := NewMemory(Limits{})
			ctx := context.Background()
			ids := make([]string, count)
			frame := testFrame(1, true)
			frame.Packets[0].Payload = make([]byte, 1200)
			for i := range ids {
				ids[i] = strconv.Itoa(i)
				if err := cache.Append(ctx, ids[i], frame); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := cache.Append(ctx, ids[i%count], frame); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestMemoryLRUAndByteAccounting(t *testing.T) {
	cache := NewMemory(Limits{TotalBytes: 3})
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		require.NoError(t, cache.Append(ctx, id, testFrame(1, true)))
	}
	_, err := cache.Current(ctx, "a", testTrack)
	require.NoError(t, err)
	require.NoError(t, cache.Append(ctx, "d", testFrame(1, true)))
	require.Nil(t, cache.sessions["b"], "reading a makes b the oldest")
	require.Equal(t, 3, cache.bytes)
	require.NoError(t, cache.Append(ctx, "a", testFrame(3, false)))
	require.Equal(t, 2, cache.bytes, "lost references remove the whole group")
	require.NoError(t, cache.Append(ctx, "d", testFrame(2, true)))
	require.Equal(t, 2, cache.bytes, "replacement counts only the current group")
	require.NoError(t, cache.DeleteSession(ctx, "c"))
	require.Equal(t, 1, cache.bytes)
	require.Equal(t, len(cache.sessions), cache.lru.Len())
	cache.nextExpiry = time.Time{}
	cache.sessions["a"].touched = time.Now().Add(-31 * time.Second)
	cache.lru.MoveToFront(cache.sessions["a"].element)
	_, err = cache.Current(ctx, "d", testTrack)
	require.NoError(t, err)
	require.Nil(t, cache.sessions["a"])
	require.Equal(t, 1, cache.bytes)
}
