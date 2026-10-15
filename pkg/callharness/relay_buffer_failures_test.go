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
	holdTimeout     time.Duration
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
	relayConfig := relay.Config{Owners: store, Buffer: &cfg.buffer, HoldTimeout: cfg.holdTimeout}
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
	moves    []relay.RouteRequest
	dropped  bool
}

func (f *bufferResumeReplyFault) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/move") {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(data))
		var move relay.RouteRequest
		if err := json.Unmarshal(data, &move); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.moves = append(f.moves, move)
		f.mu.Unlock()
	}

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
	var statusErr error
	start := time.Now()
	deadline := start.Add(3 * time.Second)
	for {
		time.Sleep(5 * time.Millisecond)
		status, statusErr = h.Status(ctx)
		if statusErr == nil && len(status.Takeovers) == n {
			return status
		}
		if !time.Now().Before(deadline) {
			break
		}
	}
	// #114 diagnostics: record the window's last view, then keep polling to
	// show whether the takeover is merely late, lost or never completes.
	rec := diag114Record{Event: "takeover-timeout", Want: n, WaitStart: start, Status: &status}
	if statusErr != nil {
		rec.StatusError = statusErr.Error()
	}
	diag114Write(t, rec)
	lateAt, late, lateErr := diag114Late(h, n, 8*time.Second)
	rec = diag114Record{Event: "takeover-late", Want: n, WaitStart: start, LateAt: lateAt, LateCount: len(late.Takeovers), Status: &late}
	if lateErr != nil {
		rec.StatusError = lateErr.Error()
	}
	diag114Write(t, rec)
	t.Fatalf("DIAG114 takeover window: want %d takeovers within 3s, got %d (err %v); late_at=%v late_count=%d",
		n, len(status.Takeovers), statusErr, lateAt.Sub(start), len(late.Takeovers))
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
		for _, path := range []string{"failed-adoption", "failed-adoption-loss", "retried-target", "retried-replay", "second-crash-during", "second-crash-after", "second-crash-canceled", "sequence-wrap", "frame-cache-complete", "incomplete-ring-cache-on", "overflow", "expiry", "relay-restart"} {
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
	case "second-crash-during", "second-crash-after", "second-crash-canceled":
		// #114 experiment: DIAG114_AFTER_DEFAULT_PACING=1 keeps the relay's
		// default replay pacing for the after path only.
		if path != "second-crash-after" || os.Getenv("DIAG114_AFTER_DEFAULT_PACING") != "1" {
			cfg.buffer.ReplayBatchPackets = 1
			cfg.buffer.ReplayInterval = 5 * time.Millisecond
		}
	case "frame-cache-complete":
		cfg.cache = true
	case "incomplete-ring-cache-on":
		cfg.cache = true
		// Keep actual video datagrams as well as audio, while truncating
		// enough of the checkpoint window to break codec continuity.
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
	cancelReplay := make(chan context.CancelFunc, 1)
	var canceledReplay atomic.Bool
	if path == "second-crash-canceled" {
		cfg.relayHandler = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/replay") && canceledReplay.CompareAndSwap(false, true) {
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()
					cancelReplay <- cancel
					next.ServeHTTP(w, r.WithContext(ctx))
					close(replayDone)
					<-replayDoneAck
					return
				}
				next.ServeHTTP(w, r)
			})
		}
	}
	// #114 experiment: DIAG114_REPLAY_EXTRA lengthens B's tenure after the
	// first replay, as a starved replay would, before the second kill.
	replayExtra, _ := time.ParseDuration(os.Getenv("DIAG114_REPLAY_EXTRA"))
	if path == "second-crash-after" {
		cfg.relayHandler = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r)
				if strings.HasSuffix(r.URL.Path, "/replay") {
					time.Sleep(replayExtra)
					replayOnce.Do(func() { close(replayDone) })
					<-replayDoneAck
				}
			})
		}
	}
	sys := startBufferFailureSystem(t, store, cfg)
	var dm diag114Marks
	var diagStatus *controlplane.Status
	if strings.HasPrefix(path, "second-crash") {
		t.Cleanup(func() {
			stats := sys.r.Stats()
			diag114Write(t, diag114Record{Event: "outcome", Failed: t.Failed(), Marks: dm.snapshot(), Status: diagStatus, Relay: &stats})
		})
	}
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
	if path == "second-crash-during" || path == "second-crash-canceled" {
		require.NoError(t, workerprobe.SetAfterEcho(sys.workers[1].LocalAddr(), func(_ context.Context, id string, _ []byte) {
			if id == call.SessionID() {
				echoedOnce.Do(func() {
					if path == "second-crash-canceled" {
						(<-cancelReplay)()
					}
					close(echoed)
				})
			}
		}))
	}
	var processedMu sync.Mutex
	type inboundPacket struct {
		ssrc  uint32
		index uint64
	}
	processedB, processedC := make(map[inboundPacket]bool), make(map[inboundPacket]bool)
	if path == "second-crash-after" || path == "second-crash-canceled" {
		// Prevent B from checkpointing its drained input before the second kill.
		require.NoError(t, workerprobe.SetBeforeSnapshot(sys.workers[1].LocalAddr(), func(ctx context.Context, _ string) { <-ctx.Done() }))
		for i, packets := range map[int]map[inboundPacket]bool{1: processedB, 2: processedC} {
			require.NoError(t, workerprobe.SetAfterDecrypt(sys.workers[i].LocalAddr(), func(id string, ssrc uint32, index uint64) {
				if id == call.SessionID() {
					processedMu.Lock()
					packets[inboundPacket{ssrc, index}] = true
					processedMu.Unlock()
				}
			}))
		}
	}
	killed := time.Now()
	dm.mark("kill_a", killed)
	sys.h.RecordProcessKill("0", killed)
	require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
	expected := 1
	if path == "second-crash-during" || path == "second-crash-after" || path == "second-crash-canceled" {
		boundary := echoed
		if path == "second-crash-after" || path == "second-crash-canceled" {
			boundary = replayDone
		}
		select {
		case <-boundary:
			dm.mark("boundary", time.Now())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if path == "second-crash-during" || path == "second-crash-canceled" {
			require.Equal(t, 1, sys.r.Stats().Holds, "second kill occurs while replay still gates live input")
		}
		failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		dm.mark("kill_b", time.Now())
		sys.h.RecordProcessKill("1", time.Now())
		require.NoError(t, workerprobe.Kill(sys.workers[1].LocalAddr()))
		if path == "second-crash-after" || path == "second-crash-canceled" {
			replayAckOnce.Do(func() { close(replayDoneAck) })
		}
		expected = 2
		if path == "second-crash-canceled" {
			expected = 1
		}
	}
	dm.mark("wait_start", time.Now())
	status := waitBufferTakeovers(t, ctx, sys.h, expected)
	dm.mark("wait_done", time.Now())
	diagStatus = &status
	lostCall := path == "failed-adoption-loss"
	require.Equal(t, lostCall, status.Takeovers[len(status.Takeovers)-1].Lost)
	require.EqualValues(t, map[bool]int{false: 0, true: 1}[lostCall], status.LostCount)
	require.Zero(t, sys.r.Stats().Holds, "no terminal path retains a gate")
	require.Zero(t, sys.r.Stats().HeldBytes)
	if lostCall {
		require.Empty(t, status.Calls)
		require.EqualValues(t, 2, rejected.Load())
		_, err := store.Get(ctx, call.SessionID())
		require.ErrorIs(t, err, sessionstore.ErrEnded)
	} else {
		require.Len(t, status.Calls, 1)
		target, err := strconv.Atoi(status.Calls[0].Owner)
		require.NoError(t, err)
		time.Sleep(500 * time.Millisecond)
		failures, err := sys.workers[target].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
	}
	if path == "second-crash-after" || path == "second-crash-canceled" {
		require.Equal(t, "2", status.Calls[0].Owner)
		processedMu.Lock()
		count := len(processedB)
		var missing []inboundPacket
		for packet := range processedB {
			if !processedC[packet] {
				missing = append(missing, packet)
			}
		}
		processedMu.Unlock()
		require.Positive(t, count, "B decrypted uncheckpointed input before the kill")
		require.Empty(t, missing, "C must decrypt all of B's uncheckpointed indexes")
	}
	if adoption != nil {
		require.True(t, adoption.committed.Load())
		require.Nil(t, adoption.killErr.Load())
		require.Equal(t, "2", status.Calls[0].Owner)
	}
	if reply != nil && path == "retried-replay" {
		lease, err := store.Get(ctx, call.SessionID())
		require.NoError(t, err)
		reply.mu.Lock()
		require.Len(t, reply.moves, 2)
		for _, move := range reply.moves {
			require.Equal(t, lease.Epoch, move.Epoch, "HTTP move confirmation preserves the transferred receiver tenure")
		}
		require.Len(t, reply.requests, 2, "confirmation reads the completed replay receipt")
		require.Equal(t, reply.requests[0], reply.requests[1])
		reply.mu.Unlock()
		require.False(t, status.Takeovers[0].Result.HoldExpired)
		require.True(t, status.Takeovers[0].Result.RelayReplayComplete, "the retry confirms the completed replay receipt")
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
	fallback := path == "incomplete-ring-cache-on" || path == "overflow" || path == "expiry" || path == "relay-restart"
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
		if path == "overflow" || path == "incomplete-ring-cache-on" {
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
		if path == "incomplete-ring-cache-on" {
			require.Greater(t, event.Result.RelayReplayPackets, 1, "the incomplete relay ring actually delivers retained media")
			require.True(t, event.Result.RelayReplayRecoveryPLI, "incomplete retained input requests a keyframe after drain")
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

// Execute export but discard its reply: recovery must release the existing
// planned hold to the adopted owner, even with the default buffer enabled.
func TestRelayBufferUncertainExport(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		var exported atomic.Bool
		sys := startBufferFailureSystem(t, store, bufferFailureConfig{workers: 2, workerHandler: func(i int, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if i == 1 && r.URL.Path == "/resume" {
					time.Sleep(100 * time.Millisecond)
				}
				if i == 0 && strings.HasSuffix(r.URL.Path, "/export") && exported.CompareAndSwap(false, true) {
					next.ServeHTTP(httptest.NewRecorder(), r)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				next.ServeHTTP(w, r)
			})
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{Video: true, Worker: 0})
		require.NoError(t, err)
		o := observeBufferCall(call)
		mediaCtx, stop := context.WithCancel(ctx)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(mediaCtx, 7*time.Second) }()
		defer func() { stop(); require.ErrorIs(t, <-sent, context.Canceled) }()
		time.Sleep(650 * time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sys.h.external.SignalingURL+"/"+call.SessionID()+"/move?to=1", nil)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode, "uncertain export must recover before the planned hold expires")
		var move controlplane.MoveResult
		require.NoError(t, json.NewDecoder(response.Body).Decode(&move))
		require.Positive(t, move.Result.HeldPackets)
		require.False(t, move.Result.HoldExpired)
		require.True(t, exported.Load())
		require.Zero(t, sys.r.Stats().Holds)
		require.Zero(t, sys.r.Stats().HoldTimeouts)
		time.Sleep(300 * time.Millisecond)
		failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures)
		sentDuring, lost := bufferPacketLoss(t, o, move.Start, move.End)
		require.Positive(t, sentDuring)
		require.Zero(t, lost, "held caller input must reach the adopted worker")
		stop()
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		require.True(t, report.ConnectedThroughout())
	})
}

// Lose the first target reply until the planned hold expires, either before
// adoption or after adoption and successful decryption of ungated live input.
type bufferExpiredResumeFault struct {
	expired         func() bool
	adopted         bool
	liveAfterExpiry func() uint64
	once            atomic.Bool
}

func (f *bufferExpiredResumeFault) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/resume") && f.once.CompareAndSwap(false, true) {
		defer func() { _ = req.Body.Close() }()
		if f.adopted {
			response, err := http.DefaultTransport.RoundTrip(req)
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("injected adoption failed: status=%d error=%v", response.StatusCode, err)
			}
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for !f.expired() {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-tick.C:
			}
		}
		if f.adopted {
			// Keep B live beyond the initial held-queue flush before confirmation.
			before, until := f.liveAfterExpiry(), time.Now().Add(150*time.Millisecond)
			for time.Now().Before(until) || f.liveAfterExpiry() < before+5 {
				select {
				case <-req.Context().Done():
					return nil, req.Context().Err()
				case <-tick.C:
				}
			}
		}
		return nil, errors.New("injected lost resume reply after hold expiry")
	}
	return http.DefaultTransport.RoundTrip(req)
}
func TestRelayBufferUncertainExportAfterHoldExpiry(t *testing.T) {
	testRelayBufferUncertainExportAfterHoldExpiry(t, false)
}

func TestRelayBufferUncertainExportAdoptedAfterHoldExpiry(t *testing.T) {
	testRelayBufferUncertainExportAfterHoldExpiry(t, true)
}

func testRelayBufferUncertainExportAfterHoldExpiry(t *testing.T, adopted bool) {
	t.Helper()
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		var sys *bufferFailureSystem
		var liveAfterExpiry atomic.Uint64
		fault := &bufferExpiredResumeFault{expired: func() bool { return sys.r.Stats().HoldTimeouts > 0 }, adopted: adopted, liveAfterExpiry: liveAfterExpiry.Load}
		var exported atomic.Bool
		var mu sync.Mutex
		var begins []relay.ReplayRequest
		sys = startBufferFailureSystem(t, store, bufferFailureConfig{
			workers: 2, holdTimeout: 500 * time.Millisecond, targetTransport: fault,
			workerHandler: func(i int, next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if i == 0 && strings.HasSuffix(r.URL.Path, "/export") && exported.CompareAndSwap(false, true) {
						next.ServeHTTP(httptest.NewRecorder(), r)
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					next.ServeHTTP(w, r)
				})
			},
			relayHandler: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/begin-replay") {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
						var begin relay.ReplayRequest
						if err = json.Unmarshal(body, &begin); err != nil {
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						mu.Lock()
						begins = append(begins, begin)
						mu.Unlock()
					}
					next.ServeHTTP(w, r)
				})
			},
		})
		require.NoError(t, workerprobe.SetAfterDecrypt(sys.workers[1].LocalAddr(), func(_ string, _ uint32, _ uint64) {
			if sys.r.Stats().HoldTimeouts > 0 {
				liveAfterExpiry.Add(1)
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		call, err := sys.h.Dial(ctx, CallOptions{Video: true, Worker: 0})
		require.NoError(t, err)
		o := observeBufferCall(call)
		mediaCtx, stop := context.WithCancel(ctx)
		sent := make(chan error, 1)
		go func() { sent <- call.SendMedia(mediaCtx, 7*time.Second) }()
		defer func() { stop(); require.ErrorIs(t, <-sent, context.Canceled) }()
		time.Sleep(650 * time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, sys.h.external.SignalingURL+"/"+call.SessionID()+"/move?to=1", nil)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = response.Body.Close()
		require.Equal(t, http.StatusInternalServerError, response.StatusCode, "first adoption remains uncertain after the original hold expires")
		liveBeforeConfirmation := liveAfterExpiry.Load()
		if adopted {
			require.GreaterOrEqual(t, liveBeforeConfirmation, uint64(5), "B authenticated ungated input before confirmation")
		}
		status := waitBufferTakeovers(t, ctx, sys.h, 1)
		time.Sleep(300 * time.Millisecond)
		failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
		require.NoError(t, err)
		require.Zero(t, failures, "confirmation must not replay packets to a possibly live B")
		require.False(t, status.Takeovers[0].Lost)
		require.False(t, status.Takeovers[0].Result.RelayReplayComplete)
		require.Zero(t, status.Takeovers[0].Result.RelayReplayPackets)
		require.True(t, status.Takeovers[0].Result.HoldExpired)
		require.True(t, status.Takeovers[0].Result.RelayReplayRecoveryPLI, "expired confirmation requests fresh recovery after idempotent resume")
		require.Equal(t, "1", status.Calls[0].Owner)
		require.EqualValues(t, 1, sys.r.Stats().HoldTimeouts, "recovery must not wait for a second hold expiry")
		require.Zero(t, sys.r.Stats().Holds)
		mu.Lock()
		observed := append([]relay.ReplayRequest(nil), begins...)
		mu.Unlock()
		require.Len(t, observed, 1, "same-target confirmation must not create a replay gate")
		for _, begin := range observed {
			require.Equal(t, sys.workers[0].LocalAddr(), begin.From, "gate creation and checkpoint preparation use the same original source")
		}
		sentDuring, lost := bufferPacketLoss(t, o, status.Takeovers[0].Start, status.Takeovers[0].End)
		require.Positive(t, sentDuring)
		if !adopted {
			require.Positive(t, lost, "expired hold cannot recover input lost before adoption")
		}
		stop()
		report, err := call.Hangup(ctx)
		require.NoError(t, err)
		require.Zero(t, report.DecryptionFailures.Total())
		require.True(t, report.ConnectedThroughout())
		require.Positive(t, report.SentVideo.KeyframeRequests, "legacy confirmation recovers any gap with a keyframe")
		for _, kind := range []string{"audio", "video"} {
			track := report.Track(kind)
			require.NotNil(t, track)
			require.Greater(t, track.LastArrival, status.Takeovers[0].End.Sub(report.StartedAt), "media continues after confirmation")
		}
		t.Logf("BUFFER_EXPIRY adopted=%t live_before_confirmation=%d hold_timeouts=%d outage_lost=%d worker_decrypt=%d gate_released=true", adopted, liveBeforeConfirmation, sys.r.Stats().HoldTimeouts, lost, failures)
	})
}
