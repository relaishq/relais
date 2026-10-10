package framecache

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

func TestBinaryFrameRoundTripAndTruncation(t *testing.T) {
	f := testFrame(65535, true)
	f.Arrival = time.Now().Round(0)
	f.SourceSSRC = 77
	f.EchoTimestamp = 123
	f.Track.Kind = "video\x00\xff"
	f.Packets = []Packet{{SequenceNumber: 65535, Payload: []byte{0, 255, 0x10}}, {SequenceNumber: 0, Marker: true, Payload: []byte{0x11}}}
	raw, err := encodeFrame(f)
	require.NoError(t, err)
	decoded, err := decodeFrame(raw)
	require.NoError(t, err)
	require.Equal(t, f, decoded)
	for n := range len(raw) {
		_, err = decodeFrame(raw[:n])
		require.Error(t, err)
	}
	_, err = decodeFrame(append(raw, 0))
	require.Error(t, err)
	raw[0] = 2
	_, err = decodeFrame(raw)
	require.Error(t, err)
}
func TestRedisCiphertextAuthenticationBindingAndRotation(t *testing.T) {
	r := testRedis(t, Limits{})
	ctx := context.Background()
	f := testFrame(1, true)
	f.Packets[0].Payload = bytes.Repeat([]byte("private-video\x00\xff"), 20)
	require.NoError(t, r.Append(ctx, "a", f))
	field := trackField(f.Track) + "f1"
	raw, err := r.client.HGet(ctx, r.key("a"), field).Bytes()
	require.NoError(t, err)
	require.Equal(t, byte(1), raw[0])
	require.NotContains(t, string(raw), string(f.Packets[0].Payload))
	require.NoError(t, r.Append(ctx, "a", f))
	next, err := r.client.HGet(ctx, r.key("a"), field).Bytes()
	require.NoError(t, err)
	require.NotEqual(t, raw, next, "fresh nonce even for identical frames")
	_, err = r.open("b", f.Track, raw)
	require.Error(t, err)
	other := f.Track
	other.SSRC++
	_, err = r.open("a", other, raw)
	require.Error(t, err)
	raw[len(raw)-1] ^= 1
	require.NoError(t, r.client.HSet(ctx, r.key("a"), field, raw).Err())
	frames, err := r.Current(ctx, "a", f.Track)
	require.Error(t, err)
	require.Nil(t, frames)
	key := bytes.Repeat([]byte{7}, 32)
	rotated, err := NewRedis(ctx, storage.RedisConfig{Addr: os.Getenv("RELAIS_TEST_REDIS_ADDR"), Prefix: r.prefix}, key, Limits{}, RedisOptions{KeyID: 1, OldKeys: map[byte][]byte{0: make([]byte, 32)}})
	require.NoError(t, err)
	defer rotated.Close()
	require.NoError(t, r.Append(ctx, "a", f))
	frames, err = rotated.Current(ctx, "a", f.Track)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	require.NoError(t, rotated.Append(ctx, "a", f))
	_, err = r.Current(ctx, "a", f.Track)
	require.Error(t, err, "old reader does not know new key")
}
func TestRedisCrossClientAndSessionKeys(t *testing.T) {
	r := testRedis(t, Limits{})
	ctx := context.Background()
	other, err := NewRedis(ctx, storage.RedisConfig{Addr: os.Getenv("RELAIS_TEST_REDIS_ADDR"), Prefix: r.prefix}, make([]byte, 32), Limits{})
	require.NoError(t, err)
	defer other.Close()
	for _, id := range []string{"a", "a}", "a{", "~61", "\x00\xff"} {
		require.NoError(t, r.Append(ctx, id, testFrame(1, true)))
		frames, err := other.Current(ctx, id, testTrack)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		ttl, err := r.client.PTTL(ctx, r.key(id)).Result()
		require.NoError(t, err)
		require.Positive(t, ttl)
		require.LessOrEqual(t, ttl, 30*time.Second)
	}
	require.NoError(t, other.DeleteSession(ctx, "a"))
	frames, err := r.Current(ctx, "a", testTrack)
	require.NoError(t, err)
	require.Empty(t, frames)
	frames, err = r.Current(ctx, "a}", testTrack)
	require.NoError(t, err)
	require.Len(t, frames, 1)
}

// A stalled RESP peer exercises the real Redis client's socket deadline,
// without pausing the Redis shared by other concurrently running packages.
func TestRedisCurrentHonorsReadDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		if _, err = conn.Read(buf); err != nil {
			return
		}
		if _, err = conn.Write([]byte("+PONG\r\n")); err != nil {
			return
		}
		// Drain requests but return no script response, as a slow Redis would.
		_, _ = io.Copy(io.Discard, conn)
	}()
	r, err := NewRedis(context.Background(), storage.RedisConfig{Addr: listener.Addr().String()}, make([]byte, 32), Limits{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	frames, err := r.Current(ctx, "s", testTrack)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, frames)
	require.Less(t, time.Since(start), 250*time.Millisecond)
	require.NoError(t, r.Close())
	<-done
}

func TestRedisReplayAgeIgnoresHostClockSkew(t *testing.T) {
	r := testRedis(t, Limits{})
	for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		f := testFrame(1, true)
		f.Arrival = time.Now().Add(skew)
		require.NoError(t, r.Append(context.Background(), "skew", f))
		time.Sleep(50 * time.Millisecond)
		frames, err := r.Current(context.Background(), "skew", f.Track)
		require.NoError(t, err)
		require.Len(t, frames, 1)
		require.True(t, f.Arrival.Equal(frames[0].Arrival), "diagnostic arrival is preserved")
		age := frames[0].ReplayAge(time.Now())
		require.GreaterOrEqual(t, age, 50*time.Millisecond)
		require.Less(t, age, time.Second, "replay age comes from Redis, not either host")
		later := frames[0].ReplayAge(frames[0].readAt.Add(100 * time.Millisecond))
		require.Equal(t, frames[0].ageAtRead+100*time.Millisecond, later)
	}
}
