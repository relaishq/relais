package framecache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func testRedis(t *testing.T, limits Limits, options ...RedisOptions) *Redis {
	t.Helper()
	addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
	if addr == "" {
		if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
			t.Fatal("required Redis endpoint is not configured")
		}
		t.Skip("Redis requires RELAIS_TEST_REDIS_ADDR")
	}
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.NotEqual(t, "6379", port, "tests must use a dedicated Redis")
	var token [16]byte
	_, err = rand.Read(token[:])
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := NewRedis(ctx, storage.RedisConfig{Addr: addr, Prefix: "framecache:test:" + hex.EncodeToString(token[:]) + ":"}, make([]byte, 32), limits, options...)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		keys, err := r.client.Keys(ctx, r.prefix+"*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, r.client.Del(ctx, keys...).Err())
		}
		require.NoError(t, r.Close())
	})
	return r
}
func forStores(t *testing.T, fn func(*testing.T, func(Limits) Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, func(l Limits) Store { return NewMemory(l) }) })
	t.Run("redis", func(t *testing.T) { fn(t, func(l Limits) Store { return testRedis(t, l) }) })
}

func TestStoreCurrentGroup(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(Limits) Store) {
		store := newStore(Limits{})
		ctx := context.Background()
		for seq := uint16(1); seq <= 100; seq++ {
			require.NoError(t, store.Append(ctx, "s", testFrame(seq, seq%10 == 1)))
		}
		frames, err := store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 10)
		require.Equal(t, testFrame(91, true), frames[0])
		require.Equal(t, testFrame(100, false), frames[9])
	})
}
func TestStoreWholeGroupBounds(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(Limits) Store) {
		for _, limits := range []Limits{{Bytes: 3, Frames: 100}, {Bytes: 100, Frames: 3}} {
			store := newStore(limits)
			ctx := context.Background()
			for seq := uint16(1); seq <= 7; seq++ {
				require.NoError(t, store.Append(ctx, "s", testFrame(seq, seq == 1 || seq == 3)))
			}
			frames, err := store.Current(ctx, "s", testTrack)
			require.NoError(t, err)
			require.Empty(t, frames)
			require.NoError(t, store.Append(ctx, "s", testFrame(8, true)))
			frames, err = store.Current(ctx, "s", testTrack)
			require.NoError(t, err)
			require.Len(t, frames, 1)
			oversized := testFrame(9, true)
			oversized.Packets[0].Payload = make([]byte, limits.Bytes+1)
			require.NoError(t, store.Append(ctx, "s", oversized))
			frames, err = store.Current(ctx, "s", testTrack)
			require.NoError(t, err)
			require.Empty(t, frames, "oversized keyframe replaces and drops the old group")
		}
	})
}
func TestStoreIsolationCopiesDeletionAndReferences(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(Limits) Store) {
		store := newStore(Limits{})
		ctx := context.Background()
		f := testFrame(1, true)
		require.NoError(t, store.Append(ctx, "a", f))
		f.Packets[0].Payload[0] = 99
		frames, err := store.Current(ctx, "a", testTrack)
		require.NoError(t, err)
		require.Equal(t, byte(1), frames[0].Packets[0].Payload[0])
		frames[0].Packets[0].Payload[0] = 98
		frames, err = store.Current(ctx, "a", testTrack)
		require.NoError(t, err)
		require.Equal(t, byte(1), frames[0].Packets[0].Payload[0])
		require.NoError(t, store.Append(ctx, "b", testFrame(1, true)))
		other := testFrame(1, true)
		other.Track.SSRC++
		require.NoError(t, store.Append(ctx, "a", other))
		require.NoError(t, store.Append(ctx, "a", testFrame(3, false)))
		frames, err = store.Current(ctx, "a", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames)
		frames, err = store.Current(ctx, "a", other.Track)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		require.NoError(t, store.DeleteSession(ctx, "a"))
		require.NoError(t, store.DeleteSession(ctx, "a"))
		frames, err = store.Current(ctx, "a", other.Track)
		require.NoError(t, err)
		require.Empty(t, frames)
		frames, err = store.Current(ctx, "b", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 1)
	})
}
func TestStoreRejectsInvalidAndCancelled(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(Limits) Store) {
		store := newStore(Limits{})
		ctx := context.Background()
		require.NoError(t, store.Append(ctx, "s", testFrame(1, true)))
		for _, f := range []Frame{{}, {Keyframe: true, Packets: []Packet{{Marker: true}}}, {Keyframe: true, Packets: []Packet{{Payload: []byte{0}}}}, {Keyframe: true, Packets: []Packet{{SequenceNumber: 1, Payload: []byte{0}}, {SequenceNumber: 3, Marker: true, Payload: []byte{0}}}}} {
			require.ErrorIs(t, store.Append(ctx, "s", f), ErrInvalidFrame)
		}
		frames, err := store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		require.ErrorIs(t, store.Append(cancelled, "s", testFrame(2, true)), context.Canceled)
		_, err = store.Current(cancelled, "s", testTrack)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, store.DeleteSession(cancelled, "s"), context.Canceled)
	})
}
func TestStoreSerialWrapAndTimestampLoss(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(Limits) Store) {
		store := newStore(Limits{})
		ctx := context.Background()
		first := testFrame(math.MaxUint16, true)
		first.Timestamp = math.MaxUint32 - 2000
		require.NoError(t, store.Append(ctx, "s", first))
		next := testFrame(0, false)
		next.Timestamp = 999
		require.NoError(t, store.Append(ctx, "s", next))
		frames, err := store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 2)
		next = testFrame(1, false)
		next.Timestamp = 999
		require.NoError(t, store.Append(ctx, "s", next))
		frames, err = store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames)
		require.NoError(t, store.Append(ctx, "s", testFrame(2, false)))
		frames, err = store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames)
		require.NoError(t, store.Append(ctx, "s", testFrame(3, true)))
		next = testFrame(4, false)
		next.Timestamp = 8000
		require.NoError(t, store.Append(ctx, "s", next))
		frames, err = store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames)
	})
}
func TestStoreIdleTTL(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(Limits) Store) {
		store := newStore(Limits{IdleTTL: 100 * time.Millisecond})
		ctx := context.Background()
		require.NoError(t, store.Append(ctx, "s", testFrame(1, true)))
		time.Sleep(60 * time.Millisecond)
		frames, err := store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		time.Sleep(60 * time.Millisecond)
		frames, err = store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 1, "read refreshes idle expiry")
		time.Sleep(130 * time.Millisecond)
		frames, err = store.Current(ctx, "s", testTrack)
		require.NoError(t, err)
		require.Empty(t, frames)
	})
}
func TestStoreAtomicConcurrentGroups(t *testing.T) {
	forStores(t, func(t *testing.T, newStore func(Limits) Store) {
		store := newStore(Limits{})
		ctx := context.Background()
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for worker := range 4 {
			wg.Go(func() {
				id := fmt.Sprint(worker)
				for seq := uint16(1); seq <= 60; seq++ {
					if err := store.Append(ctx, id, testFrame(seq, seq%3 == 1)); err != nil {
						errs <- err
						return
					}
				}
			})
		}
		for worker := range 4 {
			wg.Go(func() {
				for range 60 {
					frames, err := store.Current(ctx, fmt.Sprint(worker), testTrack)
					if err != nil {
						errs <- err
						return
					}
					if len(frames) > 0 {
						if !frames[0].Keyframe || len(frames) > 3 {
							errs <- fmt.Errorf("mixed group: %v", frames)
							return
						}
						for i := 1; i < len(frames); i++ {
							if frames[i].Keyframe || frames[i].Timestamp != frames[i-1].Timestamp+3000 {
								errs <- fmt.Errorf("partial or mixed group")
								return
							}
						}
					}
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
	})
}
