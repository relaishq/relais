package callharness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relais/internal/privateapi"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type bufferFailureConfig struct {
	buffer          relay.BufferConfig
	cache           bool
	workers         int
	workerHandler   func(int, http.Handler) http.Handler
	relayHandler    func(http.Handler) http.Handler
	targetTransport http.RoundTripper
	relayTransport  http.RoundTripper
	persist         bool
}

type bufferFailureSystem struct {
	*bufferHarness
	switcher *bufferRelaySwitcher
}

type bufferRelaySwitcher struct {
	mu      sync.RWMutex
	handler http.Handler
}

func (s *bufferRelaySwitcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	handler := s.handler
	s.mu.RUnlock()
	handler.ServeHTTP(w, r)
}
func (s *bufferRelaySwitcher) set(h http.Handler) { s.mu.Lock(); s.handler = h; s.mu.Unlock() }

func startBufferFailureSystem(t *testing.T, store sessionstore.Store, cfg bufferFailureConfig) *bufferFailureSystem {
	t.Helper()
	t.Cleanup(workerprobe.Enable())
	var frames framecache.Store = framecache.NewMemory(framecache.Limits{})
	if cfg.cache {
		if _, ok := store.(*sessionstore.Redis); ok {
			redisFrames, err := framecache.NewRedis(context.Background(), storage.RedisConfig{Addr: os.Getenv("RELAIS_TEST_REDIS_ADDR"), Prefix: "buffer-failure-frames:" + rand.Text() + ":"}, make([]byte, 32), framecache.Limits{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, redisFrames.Close()) })
			frames = redisFrames
		}
	}
	relayConfig := relay.Config{Owners: store, Buffer: &cfg.buffer}
	if cfg.persist {
		relayConfig.Routes = store.(sessionstore.Routes)
	}
	r, err := relay.New(relayConfig)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	handler := r.PrivateHandler()
	if cfg.relayHandler != nil {
		handler = cfg.relayHandler(handler)
	}
	switcher := &bufferRelaySwitcher{handler: handler}
	rs := httptest.NewServer(switcher)
	t.Cleanup(rs.Close)
	remote := &controlplane.RemoteRelay{URL: rs.URL}
	if cfg.relayTransport != nil {
		remote.Client = &http.Client{Transport: cfg.relayTransport, Timeout: 2 * time.Second}
	}
	plane := controlplane.NewWithConfig(remote, store, controlplane.Config{FrameCache: frames})
	sys := &bufferFailureSystem{bufferHarness: &bufferHarness{r: r}, switcher: switcher}
	count := cfg.workers
	if count == 0 {
		count = 3
	}
	for i := range count {
		w, err := mediaworker.New(mediaworker.Config{FrameCache: frames, DisableFrameCache: !cfg.cache, Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		handler := w.PrivateHandler()
		if cfg.workerHandler != nil {
			handler = cfg.workerHandler(i, handler)
		}
		ws := httptest.NewServer(handler)
		t.Cleanup(ws.Close)
		rw := &controlplane.RemoteWorker{URL: ws.URL}
		if i == 1 && cfg.targetTransport != nil {
			rw.Client = &http.Client{Transport: cfg.targetTransport, Timeout: 2 * time.Second}
		}
		require.NoError(t, remote.AddWorker(context.Background(), w.LocalAddr()))
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), rw))
		w.StartHeartbeats(plane)
		sys.workers = append(sys.workers, w)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- plane.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	ps := httptest.NewServer(plane.Handler())
	t.Cleanup(ps.Close)
	h, err := Start(Options{External: &ExternalTopology{SignalingURL: ps.URL + "/calls", RelayAddr: r.PublicAddr()}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	sys.h = h
	return sys
}

// This fault kills the real target after its adjusted counters reach the real
// store, but before adoption. The next target must reload those counters.
type bufferAdoptionFault struct {
	sessionstore.Store
	target    netip.AddrPort
	armed     atomic.Bool
	committed atomic.Bool
	killErr   atomic.Pointer[error]
}

func (s *bufferAdoptionFault) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	if err := s.Store.PutState(ctx, lease, data); err != nil {
		return err
	}
	if lease.Worker == s.target && s.armed.CompareAndSwap(true, false) {
		s.committed.Store(true)
		if err := workerprobe.Kill(s.target); err != nil {
			s.killErr.Store(&err)
		}
	}
	return nil
}

type bufferResumeReplyFault struct {
	path     string
	mu       sync.Mutex
	requests [][]byte
	dropped  bool
}

func (f *bufferResumeReplyFault) RoundTrip(req *http.Request) (*http.Response, error) {
	path := f.path
	if path == "" {
		path = "/resume"
	}
	if !strings.HasSuffix(req.URL.Path, path) {
		return http.DefaultTransport.RoundTrip(req)
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	f.mu.Lock()
	f.requests = append(f.requests, data)
	drop := !f.dropped
	f.dropped = true
	f.mu.Unlock()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && drop {
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err == nil {
			err = errors.New("injected lost adopted resume reply")
		}
		return nil, err
	}
	return resp, err
}

func observeBufferCall(call *Call) *bufferCallerObservation {
	o := &bufferCallerObservation{sent: make(map[uint8][]bufferObservedPacket), received: make(map[uint8][]bufferObservedPacket)}
	call.socket.observer.mu.Lock()
	call.socket.observer.inspectSend = func(raw []byte) { o.observe(raw, true) }
	call.socket.observer.inspect = func(raw []byte) { o.observe(raw, false) }
	call.socket.observer.mu.Unlock()
	return o
}

func waitBufferTakeovers(t *testing.T, ctx context.Context, h *Harness, n int) controlplane.Status {
	t.Helper()
	var status controlplane.Status
	require.Eventually(t, func() bool {
		var err error
		status, err = h.Status(ctx)
		return err == nil && len(status.Takeovers) == n
	}, 3*time.Second, 5*time.Millisecond)
	return status
}

// Match the clear RTP source identity (timestamp plus packet position), after
// checking that the caller decrypted every arrival. Audio timestamps and video
// frame markers anchor each tenure's sequence offset. Retry reservations can
// spend more than the one margin reported in a terminal takeover event.
func bufferPacketLoss(t *testing.T, o *bufferCallerObservation, killed, end time.Time, payloadTypes ...uint8) (sent, lost int) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(payloadTypes) == 0 {
		payloadTypes = []uint8{opusPayloadType, vp8PayloadType}
	}
	for _, pt := range payloadTypes {
		sends, receives := o.sent[pt], o.received[pt]
		require.NotEmpty(t, sends)
		require.NotEmpty(t, receives)
		timestampOffset := receives[0].header.Timestamp - sends[0].header.Timestamp
		sources := map[uint16]bufferObservedPacket{}
		anchors := map[uint32]uint16{}
		for _, p := range sends {
			sources[p.header.SequenceNumber] = p
			if pt == opusPayloadType || p.header.Marker {
				anchors[p.header.Timestamp] = p.header.SequenceNumber
			}
		}
		offsets := map[uint16]bool{}
		for _, p := range receives {
			if pt == opusPayloadType || p.header.Marker {
				if source, ok := anchors[p.header.Timestamp-timestampOffset]; ok {
					offsets[p.header.SequenceNumber-source] = true
				}
			}
		}
		returned := map[uint16]bool{}
		indexes := map[uint64]bool{}
		highest := uint64(receives[0].header.SequenceNumber)
		for _, p := range receives {
			index := bufferExtendedIndex(highest, p.header.SequenceNumber)
			require.False(t, indexes[index], "outbound SRTP index reused, payload type %d", pt)
			indexes[index] = true
			highest = max(highest, index)
			for offset := range offsets {
				seq := p.header.SequenceNumber - offset
				if source, ok := sources[seq]; ok && source.header.Timestamp+timestampOffset == p.header.Timestamp && source.header.Marker == p.header.Marker {
					returned[seq] = true
				}
			}
		}
		for _, p := range sends {
			if !p.at.Before(killed) && !p.at.After(end) {
				sent++
				if !returned[p.header.SequenceNumber] {
					lost++
				}
			}
		}
	}
	return sent, lost
}

// The existing phase-1 full-decode exporter repeats old PictureIDs and their
// DTS. Decode each caller-observed source picture once without changing #32's
// recorder/report files. Bounded trials never wrap the 15-bit PictureID.
func decodeBufferUnique(t *testing.T, ctx context.Context, call *Call, report *Report, all bool) FullDecode {
	t.Helper()
	var decode FullDecode
	for i, track := range report.Tracks {
		if track.Video == nil {
			continue
		}
		frames, size := call.rec.decodeInput(i)
		seen := map[uint16]bool{}
		unique := make([]vp8Frame, 0, len(frames))
		for _, frame := range frames {
			if !seen[frame.pictureID] {
				seen[frame.pictureID] = true
				unique = append(unique, frame)
			}
		}
		decode = fullDecode(ctx, unique, size)
		require.Empty(t, decode.Skipped)
		require.Empty(t, decode.Errors)
		require.Equal(t, decode.FramesIn, decode.FramesDecoded)
		if all {
			require.Equal(t, report.SentVideo.Frames, len(unique), "all sent original pictures must return")
		}
	}
	require.Positive(t, decode.FramesDecoded)
	return decode
}

func firstBufferContent(call *Call, report *Report, killed, end time.Time) (string, time.Duration) {
	call.rec.mu.Lock()
	defer call.rec.mu.Unlock()
	start := killed.Sub(report.StartedAt)
	for _, track := range call.rec.tracks {
		if track.video == nil {
			continue
		}
		for _, f := range track.video.frames {
			if f.source.at < start || f.firstArrival < start {
				continue
			}
			path := "Live"
			if f.source.pli {
				path = "Keyframe"
			} else if f.source.at <= end.Sub(report.StartedAt) {
				path = "Outage"
			}
			return path, f.at - start
		}
	}
	return "", 0
}

func TestRelayBufferFailurePaths(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		for _, path := range []string{"failed-adoption", "failed-adoption-loss", "retried-target", "retried-replay", "second-crash-during", "second-crash-after", "sequence-wrap", "frame-cache-complete", "frame-cache-replay", "overflow", "expiry", "relay-restart"} {
			t.Run(path, func(t *testing.T) { testBufferFailure(t, store, path) })
		}
	})
}

func testBufferFailure(t *testing.T, store sessionstore.Store, path string) {
	t.Helper()
	cfg := bufferFailureConfig{}
	var adoption *bufferAdoptionFault
	var reply *bufferResumeReplyFault
	var rejected atomic.Int32
	switch path {
	case "failed-adoption":
		adoption = &bufferAdoptionFault{Store: store}
		store = adoption
	case "failed-adoption-loss":
		cfg.workerHandler = func(i int, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if i > 0 && r.URL.Path == "/resume" {
					rejected.Add(1)
					privateapi.Error(w, mediaworker.ErrClosed, mediaworker.RemoteErrors)
					return
				}
				next.ServeHTTP(w, r)
			})
		}
	case "retried-target":
		reply = &bufferResumeReplyFault{}
		cfg.targetTransport = reply
	case "retried-replay":
		reply = &bufferResumeReplyFault{path: "/replay"}
		cfg.relayTransport = reply
	case "second-crash-during", "second-crash-after":
		cfg.buffer.ReplayBatchPackets = 1
		cfg.buffer.ReplayInterval = 5 * time.Millisecond
	case "frame-cache-complete":
		cfg.cache = true
	case "frame-cache-replay":
		cfg.cache = true
		// Keep actual video datagrams as well as audio, while truncating
		// enough of the checkpoint window to select cached-picture replay.
		cfg.buffer.MaxSessionBytes = 4 << 10
	case "overflow":
		cfg.buffer.MaxSessionBytes = 300
	case "expiry":
		cfg.buffer.Window = 10 * time.Millisecond
	case "relay-restart":
		cfg.persist = true
	}
	replayDoneAck := make(chan struct{})
	var replayAckOnce sync.Once
	defer replayAckOnce.Do(func() { close(replayDoneAck) })
	replayDone := make(chan struct{})
	var replayOnce sync.Once
	if path == "second-crash-after" {
		cfg.relayHandler = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r)
				if strings.HasSuffix(r.URL.Path, "/replay") {
					replayOnce.Do(func() { close(replayDone) })
					<-replayDoneAck
				}
			})
		}
	}
	sys := startBufferFailureSystem(t, store, cfg)
	if adoption != nil {
		adoption.target = sys.workers[1].LocalAddr()
		adoption.armed.Store(true)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	initial := RTPSequenceNumbers{Audio: 1000, Video: 2000}
	if path == "sequence-wrap" {
		initial = RTPSequenceNumbers{Audio: 65490, Video: 65490}
	}
	call, err := sys.h.Dial(ctx, CallOptions{Video: true, Worker: 0, InitialSequenceNumbers: &initial})
	require.NoError(t, err)
	o := observeBufferCall(call)
	mediaCtx, stop := context.WithCancel(ctx)
	sentDone := make(chan error, 1)
	go func() { sentDone <- call.SendMedia(mediaCtx, 10*time.Second) }()
	defer func() { stop(); require.ErrorIs(t, <-sentDone, context.Canceled) }()
	time.Sleep(650 * time.Millisecond)
	if path == "relay-restart" {
		require.Eventually(t, func() bool { return sys.r.Stats().RouteWrites > 0 }, time.Second, time.Millisecond)
		public, leg := sys.r.PublicAddr(), sys.r.WorkerAddr()
		require.NoError(t, sys.r.Close())
		replacement, err := relay.New(relay.Config{PublicAddr: public.String(), WorkerAddr: leg.String(), Owners: store, Routes: store.(sessionstore.Routes), Buffer: &cfg.buffer})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, replacement.Close()) })
		for _, w := range sys.workers {
			replacement.AddWorker(w.LocalAddr())
		}
		sys.switcher.set(replacement.PrivateHandler())
		sys.r = replacement
		require.Positive(t, replacement.Stats().RoutesRestored)
		// Kill immediately: the replacement has none of the checkpoint's tail.
	}

	echoed := make(chan struct{})
	var echoedOnce sync.Once
	if path == "second-crash-during" {
		require.NoError(t, workerprobe.SetAfterEcho(sys.workers[1].LocalAddr(), func(_ context.Context, id string, _ []byte) {
			if id == call.SessionID() {
				echoedOnce.Do(func() { close(echoed) })
			}
		}))
	}
	killed := time.Now()
	sys.h.RecordProcessKill("0", killed)
	require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
	expected := 1
	if path == "second-crash-during" || path == "second-crash-after" {
		boundary := echoed
		if path == "second-crash-after" {
			boundary = replayDone
		}
		select {
		case <-boundary:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if path == "second-crash-during" {
			require.Equal(t, 1, sys.r.Stats().Holds, "second kill occurs while replay still gates live input")
		}
		failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		sys.h.RecordProcessKill("1", time.Now())
		require.NoError(t, workerprobe.Kill(sys.workers[1].LocalAddr()))
		if path == "second-crash-after" {
			replayAckOnce.Do(func() { close(replayDoneAck) })
		}
		expected = 2
	}
	status := waitBufferTakeovers(t, ctx, sys.h, expected)
	lostCall := path == "failed-adoption-loss"
	require.Equal(t, lostCall, status.Takeovers[len(status.Takeovers)-1].Lost)
	require.EqualValues(t, map[bool]int{false: 0, true: 1}[lostCall], status.LostCount)
	require.Zero(t, sys.r.Stats().Holds, "no terminal path retains a gate")
	require.Zero(t, sys.r.Stats().HeldBytes)
	if lostCall {
		require.Empty(t, status.Calls)
		require.EqualValues(t, 2, rejected.Load())
		_, err := store.Get(ctx, call.SessionID())
		require.ErrorIs(t, err, sessionstore.ErrNotFound)
	} else {
		require.Len(t, status.Calls, 1)
		target, err := strconv.Atoi(status.Calls[0].Owner)
		require.NoError(t, err)
		time.Sleep(500 * time.Millisecond)
		failures, err := sys.workers[target].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
	}
	if adoption != nil {
		require.True(t, adoption.committed.Load())
		require.Nil(t, adoption.killErr.Load())
		require.Equal(t, "2", status.Calls[0].Owner)
	}
	if reply != nil && path == "retried-replay" {
		reply.mu.Lock()
		require.Len(t, reply.requests, 2, "confirmation rechecks replay without recreating the released gate")
		require.Equal(t, reply.requests[0], reply.requests[1])
		reply.mu.Unlock()
		require.True(t, status.Takeovers[0].Result.HoldExpired)
		require.False(t, status.Takeovers[0].Result.RelayReplayComplete, "an uncertain reply must not claim confirmed complete replay")
		require.EqualValues(t, 1, sys.r.Stats().Replays, "a lost reply cannot duplicate the replay")
	}
	if reply != nil && path == "retried-target" {
		reply.mu.Lock()
		require.Len(t, reply.requests, 2)
		var firstRequest, retryRequest mediaworker.ResumeRequest
		require.NoError(t, json.Unmarshal(reply.requests[0], &firstRequest))
		require.NoError(t, json.Unmarshal(reply.requests[1], &retryRequest))
		require.True(t, bytes.Equal(firstRequest.State, retryRequest.State), "retry confirms the original transport state")
		require.Equal(t, firstRequest.SequenceMargin, retryRequest.SequenceMargin)
		require.Equal(t, firstRequest.Lease.Epoch, retryRequest.Lease.Epoch)
		reply.mu.Unlock()
	}
	stop()
	report, err := call.Hangup(ctx)
	if lostCall {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Renegotiations)
	require.True(t, report.ConnectedThroughout())
	fallback := path == "frame-cache-replay" || path == "overflow" || path == "expiry" || path == "relay-restart"
	event := status.Takeovers[len(status.Takeovers)-1]
	first, delay := firstBufferContent(call, report, killed, event.End)
	lossSent, loss := bufferPacketLoss(t, o, killed, event.End)
	if !lostCall {
		for _, kind := range []string{"audio", "video"} {
			track := report.Track(kind)
			require.NotNil(t, track)
			require.Greater(t, track.LastArrival, event.End.Sub(report.StartedAt), "media continues after terminal replay")
		}
		if !fallback {
			require.Positive(t, lossSent)
			require.Zero(t, loss)
			require.Zero(t, sys.r.Stats().HoldTimeouts)
			require.Zero(t, sys.r.Stats().HoldDrops)
			require.Zero(t, report.SentVideo.KeyframeRequests)
			require.Equal(t, "Outage", first)
			decodeBufferUnique(t, ctx, call, report, true)
		}
		if path == "overflow" || path == "frame-cache-replay" {
			require.Positive(t, sys.r.Stats().BufferDrops)
		}
		if path == "expiry" {
			require.Positive(t, sys.r.Stats().BufferExpired)
		}
		if fallback {
			require.False(t, event.Result.RelayReplayComplete)
			require.Positive(t, report.SentVideo.KeyframeRequests)
			require.NotEmpty(t, first)
		}
		if path == "frame-cache-replay" {
			require.Greater(t, event.Result.RelayReplayPackets, 1, "the relay ring actually replays alongside the cached group")
			require.Equal(t, "Cache", report.Moves[0].Recovery.Path)
			require.Positive(t, report.Moves[0].Recovery.ReplayPackets, "frame cache actually replayed alongside the incomplete relay ring")
		}
		if path == "sequence-wrap" {
			o.mu.Lock()
			for _, pt := range []uint8{opusPayloadType, vp8PayloadType} {
				var seqs []uint16
				for _, p := range o.sent[pt] {
					seqs = append(seqs, p.header.SequenceNumber)
				}
				assertObservedWrap(t, fmt.Sprint(pt), seqs, true)
			}
			o.mu.Unlock()
		}
	}
	t.Logf("BUFFER_FAILURE path=%s outcome=%s loss_count=%d caller_decrypt=0 gate_released=true first_content=%s first_content_ms=%.1f outage_sent=%d outage_lost=%d replay_packets=%d", path, map[bool]string{false: "continued", true: "clean-loss"}[lostCall], status.LostCount, first, float64(delay)/float64(time.Millisecond), lossSent, loss, event.Result.RelayReplayPackets)
}

// Compile-time guard: the failure wrapper retains the store's checkpoint
// metadata capability, needed by the age envelope in a real takeover.
var _ sessionstore.Store = (*bufferAdoptionFault)(nil)
