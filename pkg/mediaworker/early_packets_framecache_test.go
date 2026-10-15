package mediaworker

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/framecache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A queued packet deliberately fails authentication after the keyframe. Its
// failure log runs after key installation releases mu, giving live input a
// precise window in which to overtake a queued interframe if appends are late.
type drainFailureLogger struct {
	logging.LeveledLogger
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *drainFailureLogger) Infof(format string, args ...any) {
	if strings.Contains(format, "decryption_failures=") {
		l.once.Do(func() {
			close(l.entered)
			select {
			case <-l.release:
			case <-l.ctx.Done():
			}
		})
	}
}

type drainLoggerFactory struct {
	logging.LoggerFactory
	logger *drainFailureLogger
}

func (f drainLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	if scope == "session" {
		f.logger.LeveledLogger = f.LoggerFactory.NewLogger(scope)
		return f.logger
	}
	return f.LoggerFactory.NewLogger(scope)
}

type drainFrameCache struct {
	framecache.Store
	mu         sync.Mutex
	order      []uint32
	liveStored chan struct{}
}

func (c *drainFrameCache) Append(ctx context.Context, id string, f framecache.Frame) error {
	if err := c.Store.Append(ctx, id, f); err != nil {
		return err
	}
	c.mu.Lock()
	c.order = append(c.order, f.Timestamp)
	c.mu.Unlock()
	if f.Timestamp == 9000 {
		close(c.liveStored)
	}
	return nil
}

func TestEarlyVP8FramesReachCacheBeforeLiveFrames(t *testing.T) {
	t.Cleanup(workerprobe.Enable())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger := &drainFailureLogger{ctx: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	defer close(logger.release)
	cache := &drainFrameCache{Store: framecache.NewMemory(framecache.Limits{}), liveStored: make(chan struct{})}
	w, err := New(Config{FrameCache: cache, LoggerFactory: drainLoggerFactory{LoggerFactory: logging.NewDefaultLoggerFactory(), logger: logger}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, w.Close()) })
	workerprobe.Register(w.LocalAddr(), nil)
	t.Cleanup(func() { workerprobe.Remove(w.LocalAddr()) })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	require.NoError(t, workerprobe.SetBeforeSRTP(w.LocalAddr(), func(ctx context.Context, _ string) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}))
	call, client, endpoint := handshakeDTLSCaller(t, w)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("worker did not reach the SRTP startup gate")
	}
	s := w.session(call.id)
	state, ok := client.ConnectionState()
	require.True(t, ok)
	cfg := srtp.Config{Profile: srtp.ProtectionProfileAeadAes128Gcm}
	require.NoError(t, cfg.ExtractSessionKeysFromDTLS(&state, true))
	caller := testContext(t, cfg.Keys.LocalMasterKey, cfg.Keys.LocalMasterSalt, cfg.Profile)
	send := func(seq uint16, ts uint32, marker bool, payload []byte) {
		raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 456, SequenceNumber: seq, Timestamp: ts, Marker: marker}, Payload: payload}).Marshal()
		require.NoError(t, err)
		encrypted, err := caller.EncryptRTP(nil, raw, nil)
		require.NoError(t, err)
		_, err = endpoint.conn.WriteToUDPAddrPort(encrypted, w.MediaAddr())
		require.NoError(t, err)
	}
	// The keyframe and the complete, two-packet interframe arrive before keys.
	// Put a truncated SRTP packet between them to gate the post-unlock logs.
	send(1, 3000, true, []byte{0x10, 0})
	bad := make([]byte, 12)
	bad[0], bad[1] = 0x80, 96
	_, err = endpoint.conn.WriteToUDPAddrPort(bad, w.MediaAddr())
	require.NoError(t, err)
	send(2, 6000, false, []byte{0x10, 1})
	send(3, 6000, true, []byte{0, 1})
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.srtpIn == nil && len(s.earlyPackets) == 4
	}, 5*time.Second, time.Millisecond)
	unblock()
	select {
	case <-logger.entered:
	case <-ctx.Done():
		t.Fatal("post-unlock drain log did not run")
	}
	// The connect goroutine is still in its post-unlock log. The shared UDP
	// reader must accept this interframe after both early frames' appends.
	send(4, 9000, true, []byte{0x10, 1})
	select {
	case <-cache.liveStored:
	case <-ctx.Done():
		t.Fatal("live frame could not reach the cache while the connect goroutine logged")
	}
	logger.release <- struct{}{}
	require.Eventually(t, func() bool {
		cache.mu.Lock()
		n := len(cache.order)
		cache.mu.Unlock()
		return n == 3 && w.ReplayStats().AppendPending == 0
	}, 5*time.Second, time.Millisecond)
	cache.mu.Lock()
	order := slices.Clone(cache.order)
	cache.mu.Unlock()
	assert.Equal(t, []uint32{3000, 6000, 9000}, order, "early complete frames precede later live frames")
	s.mu.Lock()
	track := framecache.Track{Kind: s.state.Video.ID, SSRC: s.state.Video.SSRC}
	s.mu.Unlock()
	frames, err := cache.Current(ctx, call.id, track)
	require.NoError(t, err)
	require.Len(t, frames, 3, "the complete keyframe group retains every frame without being discarded")
	require.True(t, frames[0].Keyframe)
	require.False(t, frames[1].Keyframe)
	require.False(t, frames[2].Keyframe)
	require.Equal(t, []framecache.Packet{{SequenceNumber: 1, Marker: true, Payload: []byte{0x10, 0}}}, frames[0].Packets)
	require.Equal(t, []framecache.Packet{{SequenceNumber: 2, Payload: []byte{0x10, 1}}, {SequenceNumber: 3, Marker: true, Payload: []byte{0, 1}}}, frames[1].Packets)
	require.Equal(t, []framecache.Packet{{SequenceNumber: 4, Marker: true, Payload: []byte{0x10, 1}}}, frames[2].Packets)
	require.EqualValues(t, 1, s.decryptFailures.Load(), "only the intentional truncated packet fails")
	require.Zero(t, s.replayRejections.Load())
	require.Zero(t, w.ReplayStats().AppendDropped)
	require.Zero(t, w.ReplayStats().AppendErrors)
}
