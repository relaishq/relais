package callharness

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relais/internal/redisendpoint"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"github.com/relais/pkg/storage"
	"github.com/stretchr/testify/require"
)

type bufferHarness struct {
	h       *Harness
	r       *relay.Relay
	workers []*mediaworker.Worker
}

// Real actors in one process, with control->relay and control->worker using
// their private HTTP APIs. External caller mode does not imply real processes.
func startBufferHarness(t *testing.T, store sessionstore.Store, enabled bool, afterTargetResume ...func()) *bufferHarness {
	t.Helper()
	disable := workerprobe.Enable()
	t.Cleanup(disable)
	frames := framecache.NewMemory(framecache.Limits{})
	cfg := relay.Config{Owners: store}
	if enabled {
		cfg.Buffer = &relay.BufferConfig{}
	}
	r, err := relay.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	rs := httptest.NewServer(r.PrivateHandler())
	t.Cleanup(rs.Close)
	remote := &controlplane.RemoteRelay{URL: rs.URL}
	plane := controlplane.NewWithConfig(remote, store, controlplane.Config{FrameCache: frames})
	sys := &bufferHarness{r: r}
	for i := range 2 {
		w, err := mediaworker.New(mediaworker.Config{FrameCache: frames, DisableFrameCache: true, Relay: &mediaworker.RelayConfig{Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), Owners: store}})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		private := w.PrivateHandler()
		if i == 1 && len(afterTargetResume) > 0 {
			inner := private
			private = http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				inner.ServeHTTP(rw, req)
				if req.URL.Path == "/resume" {
					afterTargetResume[0]()
				}
			})
		}
		ws := httptest.NewServer(private)
		t.Cleanup(ws.Close)
		require.NoError(t, remote.AddWorker(context.Background(), w.LocalAddr()))
		require.NoError(t, plane.Register(strconv.Itoa(i), w.LocalAddr(), &controlplane.RemoteWorker{URL: ws.URL}))
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

func relayBufferStores(t *testing.T, scenario func(*testing.T, sessionstore.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { scenario(t, sessionstore.NewMemory()) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("RELAIS_TEST_REDIS_ADDR")
		if addr == "" {
			if os.Getenv("RELAIS_TEST_REDIS_REQUIRE") == "1" {
				t.Fatal("required Redis endpoint is not configured")
			}
			t.Skip("set RELAIS_TEST_REDIS_ADDR to dedicated Redis")
		}
		require.NoError(t, redisendpoint.Validate(addr))
		prefix := "buffer-harness:" + rand.Text() + ":"
		store, err := sessionstore.NewRedis(context.Background(), storage.RedisConfig{Addr: addr, Prefix: prefix}, make([]byte, 32))
		require.NoError(t, err)
		t.Cleanup(func() {
			require.NoError(t, store.Close())
			client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
			defer func() { require.NoError(t, client.Close()) }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var cursor uint64
			for {
				keys, next, err := client.Scan(ctx, cursor, prefix+"*", 100).Result()
				require.NoError(t, err)
				for _, key := range keys {
					require.NoError(t, client.Del(ctx, key).Err())
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
		})
		scenario(t, store)
	})
}

type bufferObservedPacket struct {
	at     time.Time
	header rtp.Header
}
type bufferCallerObservation struct {
	mu             sync.Mutex
	sent, received map[uint8][]bufferObservedPacket
}

func (o *bufferCallerObservation) observe(raw []byte, sent bool) {
	if len(raw) < 12 || raw[0]>>6 != 2 || raw[1] >= 192 && raw[1] <= 223 {
		return
	}
	var header rtp.Header
	if _, err := header.Unmarshal(raw); err != nil {
		return
	}
	if header.PayloadType != opusPayloadType && header.PayloadType != vp8PayloadType {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	packets := o.received
	if sent {
		packets = o.sent
	}
	packets[header.PayloadType] = append(packets[header.PayloadType], bufferObservedPacket{at: time.Now(), header: header})
}

// Run the phase-1 baseline and the complete ring against the same caller,
// store, cache-off/PLI-on mode, snapshot cadence, and failure detector. All acceptance
// measurements come from the caller; worker failures are supplementary.
func TestRelayBufferTakeover(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("buffer=%t", enabled), func(t *testing.T) { testRelayBufferTakeover(t, store, enabled, false) })
		}
	})
}

// Stale checkpoint duplicates are deliberately replayed under new outbound
// indexes, while each inbound ciphertext reaches the new worker only once.
func TestRelayBufferCheckpointDuplicates(t *testing.T) {
	relayBufferStores(t, func(t *testing.T, store sessionstore.Store) { testRelayBufferTakeover(t, store, true, true) })
}

func testRelayBufferTakeover(t *testing.T, store sessionstore.Store, enabled, stale bool) {
	t.Helper()
	sys := startBufferHarness(t, store, enabled)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	call, err := sys.h.Dial(ctx, CallOptions{Video: true, InitialSequenceNumbers: &RTPSequenceNumbers{Audio: 1000, Video: 2000}})
	require.NoError(t, err)
	observation := &bufferCallerObservation{sent: make(map[uint8][]bufferObservedPacket), received: make(map[uint8][]bufferObservedPacket)}
	call.socket.observer.mu.Lock()
	call.socket.observer.inspectSend = func(raw []byte) { observation.observe(raw, true) }
	call.socket.observer.inspect = func(raw []byte) { observation.observe(raw, false) }
	call.socket.observer.mu.Unlock()
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	if stale {
		time.Sleep(700 * time.Millisecond)
		entered := make(chan struct{})
		var once sync.Once
		require.NoError(t, workerprobe.SetBeforeSnapshot(sys.workers[0].LocalAddr(), func(lifetime context.Context, id string) {
			if id != call.SessionID() {
				return
			}
			once.Do(func() { close(entered) })
			<-lifetime.Done()
		}))
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		time.Sleep(200 * time.Millisecond)
	} else {
		time.Sleep(1250 * time.Millisecond)
	}
	killed := time.Now()
	require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
	var status controlplane.Status
	require.Eventually(t, func() bool {
		var err error
		status, err = sys.h.Status(ctx)
		return err == nil && len(status.Takeovers) == 1
	}, 2*time.Second, 5*time.Millisecond)
	move := status.Takeovers[0]
	require.False(t, move.Lost)
	require.Empty(t, move.Error)
	if enabled {
		require.True(t, move.Result.RelayReplayComplete)
		require.True(t, move.Result.InputMayBeDuplicated)
		require.Equal(t, move.SnapshotAge, move.Result.InputDuplicationWindow)
		require.GreaterOrEqual(t, move.Result.InputDuplicationWindow, move.CheckpointAge)
		require.Positive(t, move.Result.RelayReplayPackets)
		require.False(t, move.Result.HoldExpired)
		require.Zero(t, sys.r.Stats().HoldDrops)
		require.Zero(t, sys.r.Stats().HoldSendFailures)
	}
	require.NoError(t, <-sent)
	workerFailures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
	require.NoError(t, err)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, workerFailures)
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.Renegotiations)
	require.Zero(t, report.ICERestarts)
	require.Zero(t, report.Track("audio").UnmatchedPayloads)
	require.Zero(t, report.Track("video").Video.UnmatchedFrames)
	decode := report.Track("video").Video.FullDecode
	duplicateFrames := 0
	if enabled {
		// #32 owns recorder/report changes. The phase-1 offline exporter feeds
		// old at-least-once pictures into the output mux a second time, causing
		// non-monotonic DTS. Decode each exact caller-observed picture once,
		// as a timestamp-aware receiver does, preserving the outage chain.
		for i, track := range report.Tracks {
			if track.Video == nil {
				continue
			}
			frames, size := call.rec.decodeInput(i)
			seen := make(map[uint16]bool)
			unique := make([]vp8Frame, 0, len(frames))
			for _, frame := range frames {
				if seen[frame.pictureID] {
					duplicateFrames++
					continue
				}
				seen[frame.pictureID] = true
				unique = append(unique, frame)
			}
			decode = fullDecode(ctx, unique, size)
			require.Equal(t, report.SentVideo.Frames, len(unique), "every original frame returns, including the outage")
		}
	}
	require.Empty(t, decode.Skipped)
	require.Empty(t, decode.Errors)
	require.Equal(t, decode.FramesIn, decode.FramesDecoded, "real decoder accepts every reassembled frame, including outage interframes")
	firstNew := ""
	firstNewDelay := time.Duration(0)
	catchup := time.Duration(0)
	start, end := killed.Sub(report.StartedAt), move.End.Sub(report.StartedAt)
	call.rec.mu.Lock()
	for _, track := range call.rec.tracks {
		if track.video == nil {
			continue
		}
		for _, f := range track.video.frames {
			if f.source.at < start || f.firstArrival < move.DetectedAt.Sub(report.StartedAt) {
				continue
			}
			if firstNew == "" {
				firstNew = "live"
				if f.source.pli {
					firstNew = "keyframe"
				} else if f.source.at <= end {
					firstNew = "outage"
				}
				firstNewDelay = f.at - start
			}
			if catchup == 0 && f.at >= end && f.at-f.source.at < 100*time.Millisecond {
				catchup = f.at - end
			}
		}
	}
	call.rec.mu.Unlock()
	if enabled {
		require.Equal(t, "outage", firstNew, "first new fully decoded content was sent while the source was dead")
		require.Zero(t, report.SentVideo.KeyframeRequests, "complete ring needs no keyframe")
		require.Positive(t, catchup, "fresh media must actually be observed after replay")
		require.Less(t, catchup, 2*time.Second)
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	for kind, pt := range map[string]uint8{"audio": opusPayloadType, "video": vp8PayloadType} {
		sends, receives := observation.sent[pt], observation.received[pt]
		require.NotEmpty(t, sends)
		require.NotEmpty(t, receives)
		offset := receives[0].header.SequenceNumber - sends[0].header.SequenceNumber
		returned := make(map[uint16]int)
		outboundSeen := make(map[uint64]bool)
		highest := uint64(receives[0].header.SequenceNumber)
		for _, p := range receives {
			index := bufferExtendedIndex(highest, p.header.SequenceNumber)
			require.False(t, outboundSeen[index], "outbound SRTP index reused on %s", kind)
			outboundSeen[index] = true
			highest = max(highest, index)
			seq := p.header.SequenceNumber - offset
			if !p.at.Before(move.DetectedAt) {
				seq -= 8192
			}
			returned[seq]++
		}
		outage, returnedOutage, lost, duplicates := 0, 0, 0, 0
		for _, p := range sends {
			if p.at.Before(killed) || p.at.After(move.End) {
				continue
			}
			outage++
			if returned[p.header.SequenceNumber] > 0 {
				returnedOutage++
			} else {
				lost++
			}
		}
		for _, count := range returned {
			if count > 1 {
				duplicates += count - 1
			}
		}
		if enabled {
			require.Positive(t, outage)
			require.Zero(t, lost, "all caller packets sent during the outage return afterwards")
			if stale {
				require.Positive(t, duplicates, "at-least-once includes packets the source already echoed after its checkpoint")
			}
		} else {
			require.Positive(t, lost)
		}
		t.Logf("RELAY_BUFFER_METRICS store=%s buffer=%t stale=%t kind=%s outage_sent=%d outage_returned=%d outage_lost=%d duplicated_content_packets=%d first_new_decoded=%s first_new_after_kill=%s gap=%s replay_packets=%d replay_duration=%s catchup_after_replay=%s worker_decrypt_failures=%d caller_decrypt_failures=%d decoded_unique_frames=%d duplicate_frames_filtered=%d", t.Name(), enabled, stale, kind, outage, returnedOutage, lost, duplicates, firstNew, firstNewDelay, report.Track(kind).MediaGap, move.Result.RelayReplayPackets, move.Result.RelayReplayDuration, catchup, workerFailures, report.DecryptionFailures.Total(), decode.FramesDecoded, duplicateFrames)
	}
}

func bufferExtendedIndex(highest uint64, seq uint16) uint64 {
	roc, last := highest>>16, uint16(highest) //nolint:gosec // low 16 bits
	if last < 1<<15 {
		if seq > last && seq-last > 1<<15 && roc > 0 {
			roc--
		}
	} else if last-(1<<15) > seq {
		roc++
	}
	return roc<<16 | uint64(seq)
}

// The target is actually adopted while its HTTP reply waits. A 100-packet
// outage prefix and 100 newer packets overlap at that precise boundary.
// Without the relay gate, the newer packets push the restored 64-packet
// SRTP receive window past the replay prefix and decryption fails.
func TestRelayBufferReplayWindow(t *testing.T) {
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sys := startBufferHarness(t, sessionstore.NewMemory(), true, func() {
		once.Do(func() { close(ready) })
		select {
		case <-release:
		case <-time.After(time.Second):
		}
	})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := sys.h.Dial(ctx, CallOptions{})
	require.NoError(t, err)
	require.NoError(t, call.SendMedia(ctx, 500*time.Millisecond))
	require.NoError(t, workerprobe.Kill(sys.workers[0].LocalAddr()))
	source, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	burst := func() {
		for range 100 {
			frame, duration, err := source.next()
			require.NoError(t, err)
			call.rec.sending(kindAudio, frame)
			require.NoError(t, call.audio.WriteSample(media.Sample{Data: frame, Duration: duration}))
			call.rec.sent(kindAudio, false)
		}
	}
	burst()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	burst()
	// Let the caller's UDP writes reach the live target/gate before allowing
	// the resume reply to return. Assertions below concern returned media,
	// rather than whether the relay's internal queue happens to be populated.
	time.Sleep(50 * time.Millisecond)
	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, func() bool { status, err := sys.h.Status(ctx); return err == nil && len(status.Takeovers) == 1 }, 2*time.Second, time.Millisecond)
	require.NoError(t, call.SendMedia(ctx, 500*time.Millisecond))
	failures, err := sys.workers[1].SessionDecryptFailures(call.SessionID())
	require.NoError(t, err)
	report, err := call.Hangup(ctx)
	require.NoError(t, err)
	require.Zero(t, failures)
	require.Zero(t, report.DecryptionFailures.Total())
	require.GreaterOrEqual(t, report.Track("audio").Packets, report.SentAudio.Frames)
	require.GreaterOrEqual(t, sys.r.Stats().ReplayPackets, uint64(200))
	require.Zero(t, sys.r.Stats().HoldDrops)
	t.Logf("RELAY_BUFFER_WINDOW replayed=%d received=%d sent=%d inbound_window=64 worker_decrypt_failures=%d caller_decrypt_failures=%d", sys.r.Stats().ReplayPackets, report.Track("audio").Packets, report.SentAudio.Frames, failures, report.DecryptionFailures.Total())
}
