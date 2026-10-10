package mediaworker

import (
	"context"
	"errors"
	"fmt"
	"github.com/pion/srtp/v3"
	"github.com/relais/pkg/relay"
	"github.com/relais/pkg/sessionstore"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/relais/pkg/framecache"
	"github.com/stretchr/testify/require"
)

func TestFrameCollectorOnlyCompleteFrames(t *testing.T) {
	track := framecache.Track{Kind: "video", SSRC: 1}
	packet := func(seq uint16, ts uint32, marker bool, data []byte) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: marker}, Payload: data}
	}
	for _, test := range []struct {
		name     string
		packets  []*rtp.Packet
		complete bool
	}{
		{"complete-keyframe", []*rtp.Packet{packet(1, 1, false, []byte{0x10, 0}), packet(2, 1, true, []byte{0, 1})}, true},
		{"missing-start", []*rtp.Packet{packet(1, 1, true, []byte{0, 0})}, false},
		{"missing-packet", []*rtp.Packet{packet(1, 1, false, []byte{0x10, 0}), packet(3, 1, true, []byte{0, 1})}, false},
		{"missing-marker", []*rtp.Packet{packet(1, 1, false, []byte{0x10, 0}), packet(2, 2, true, []byte{0, 1})}, false},
		{"malformed", []*rtp.Packet{packet(1, 1, true, []byte{0x90})}, false},
		{"complete-interframe", []*rtp.Packet{packet(1, 1, true, []byte{0x10, 1})}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var c frameCollector
			var frame *framecache.Frame
			for _, p := range test.packets {
				frame = c.push(p, track)
			}
			if !test.complete {
				require.Nil(t, frame)
				return
			}
			require.NotNil(t, frame)
			require.Len(t, frame.Packets, len(test.packets))
			require.Equal(t, track, frame.Track)
			require.Equal(t, test.packets[0].Payload[1]&1 == 0, frame.Keyframe)
			test.packets[0].Payload[1] = 77
			require.NotEqual(t, byte(77), frame.Packets[0].Payload[1])
		})
	}
}

func TestIncompleteKeyframeKeepsPreviousCurrentGroup(t *testing.T) {
	ctx := context.Background()
	cache := framecache.NewMemory(framecache.Limits{})
	track := framecache.Track{Kind: "video", SSRC: 1}
	var c frameCollector
	push := func(seq uint16, ts uint32, marker bool, data []byte) {
		f := c.push(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: marker}, Payload: data}, track)
		if f != nil {
			require.NoError(t, cache.Append(ctx, "s", *f))
		}
	}
	push(1, 1, true, []byte{0x10, 0})
	push(2, 2, true, []byte{0x10, 1})
	push(3, 3, false, []byte{0x10, 0})
	push(5, 3, true, []byte{0, 1})
	frames, err := cache.Current(ctx, "s", track)
	require.NoError(t, err)
	require.Len(t, frames, 2)
	require.Equal(t, uint32(1), frames[0].Timestamp)
	require.True(t, frames[0].Keyframe)
}

type blockedFrameCache struct {
	framecache.Store
	entered chan struct{}
}

func (b *blockedFrameCache) Append(ctx context.Context, _ string, _ framecache.Frame) error {
	close(b.entered)
	<-ctx.Done()
	return ctx.Err()
}

// Exercise the real decrypted-packet path while cache I/O stalls. The counter
// lock must remain available and hangup must cancel the append before deletion.
func TestFrameCacheIOReleasesSessionLock(t *testing.T) {
	w := newTestWorker(t)
	cache := &blockedFrameCache{Store: framecache.NewMemory(framecache.Limits{}), entered: make(chan struct{})}
	w.cfg.FrameCache = cache
	sess := sessionFromState(w, sessionState{ID: "cache-io", ICE: iceState{RemoteAddr: w.LocalAddr()}, SRTP: srtpState{Inbound: make(map[uint32]uint64)}, Video: trackState{MID: "1", ID: "video", SSRC: 123, PayloadType: 96, InitialSeq: 1200}})
	defer sess.close()
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	packet := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 456, SequenceNumber: 1, Timestamp: 3000, Marker: true}, Payload: []byte{0x10, 0}}
	raw, err := packet.Marshal()
	require.NoError(t, err)
	encrypted, err := caller.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { sess.handleRTP(encrypted); close(done) }()
	select {
	case <-cache.entered:
	case <-time.After(time.Second):
		t.Fatal("cache append did not start")
	}
	unlocked := make(chan uint64, 1)
	go func() { sess.mu.Lock(); packets := sess.state.Video.Packets; sess.mu.Unlock(); unlocked <- packets }()
	select {
	case packets := <-unlocked:
		require.EqualValues(t, 1, packets)
	case <-time.After(time.Second):
		sess.cancel()
		t.Fatal("cache I/O held the session lock")
	}
	sess.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("hangup did not cancel append")
	}
}

func TestHangupDeletesFramesAndExportPreservesThem(t *testing.T) {
	for _, export := range []bool{false, true} {
		t.Run(fmt.Sprint(export), func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.FrameCache = framecache.NewMemory(framecache.Limits{})
			call, _ := dialDTLSCaller(t, w)
			track := framecache.Track{Kind: "video", SSRC: w.session(call.id).state.Video.SSRC}
			require.NoError(t, w.cfg.FrameCache.Append(context.Background(), call.id, framecache.Frame{Track: track, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: []byte{0}}}}))
			if export {
				_, err := w.ExportSession(call.id)
				require.NoError(t, err)
			} else {
				require.NoError(t, w.EndSession(call.id))
			}
			frames, err := w.cfg.FrameCache.Current(context.Background(), call.id, track)
			require.NoError(t, err)
			if export {
				require.Len(t, frames, 1)
			} else {
				require.Empty(t, frames)
			}
		})
	}
}

func TestReplayPersistsOffsetsBeforeSource(t *testing.T) {
	w := newTestWorker(t)
	sess := sessionFromState(w, sessionState{ID: "replay-offsets", ICE: iceState{RemoteAddr: w.LocalAddr()}, Video: trackState{MID: "1", ID: "video", SSRC: 123, PayloadType: 96, Anchored: true, InboundSSRC: 987, Packets: 10, HighestSentIndex: 10192, SeqOffset: 10191, TSOffset: 6000, LastTimestamp: 9000}})
	defer sess.close()
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	sess.replay = []framecache.Frame{{Track: framecache.Track{Kind: "video", SSRC: 123}, Timestamp: 3000, SourceSSRC: 987, EchoTimestamp: 9000, Arrival: time.Now().Add(-500 * time.Millisecond), Keyframe: true, Packets: []framecache.Packet{{SequenceNumber: 1, Marker: true, Payload: []byte{0x10, 0}}}}}
	frames := sess.replay
	sess.replay = nil
	require.True(t, sess.reserveReplay(frames, 8192))
	packets, err := sess.encodeReplay()
	require.NoError(t, err)
	sess.sendReplay(packets)
	// This is exactly the track value in a snapshot taken before live media.
	// No runtime afterReplay flag is available to a subsequent cache-off resume.
	persisted := sess.state.Video
	in := rtp.Header{SSRC: 987, SequenceNumber: 2, Timestamp: 6000}
	header, ok := persisted.rewrite(&in)
	require.True(t, ok)
	require.Positive(t, int32(header.Timestamp-persisted.LastTimestamp))
	require.GreaterOrEqual(t, int16(header.SequenceNumber-uint16(persisted.ReplayFloor)), int16(2))
	header, ok = sess.state.Video.rewrite(&in)
	require.True(t, ok)
	sess.continueAfterReplay(&rtp.Packet{Header: in}, &header)
	require.Positive(t, int32(header.Timestamp-sess.state.Video.LastTimestamp))
	next, ok := sess.state.Video.rewrite(&rtp.Header{SSRC: 987, SequenceNumber: 3, Timestamp: 9000})
	require.True(t, ok)
	require.EqualValues(t, 3000, next.Timestamp-header.Timestamp)
}

// Restore the real inbound replay window, replay ten packets, then process
// fresh and delayed encrypted caller traffic through the production RTP path.
func TestReplayLatePacketsNeverReuseIndex(t *testing.T) {
	for _, planned := range []bool{false, true} {
		t.Run(fmt.Sprint(planned), func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.DisableResumePLI = true
			receiver := listenTestUDP(t)
			profile := srtp.ProtectionProfileAeadAes128Gcm
			keys := testSessionKeys(t, profile)
			state := sessionState{ID: "late", ICE: iceState{RemoteAddr: receiver.LocalAddr().(*net.UDPAddr).AddrPort()}, SRTP: srtpState{Inbound: map[uint32]uint64{456: 1000}}, Video: trackState{MID: "1", ID: "video", SSRC: 123, PayloadType: 96, Anchored: true, InboundSSRC: 456, Packets: 10, HighestSentIndex: 6000, SeqOffset: 5000, LastTimestamp: 9000}}
			sess := sessionFromState(w, state)
			defer sess.close()
			sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			require.NoError(t, restoreInboundIndex(sess.srtpIn, keys, profile, 456, 1000))
			require.NoError(t, sess.resumeTrack(&sess.state.Video, ResumeOptions{SequenceMargin: 8192}))
			var frames []framecache.Frame
			for i := range 10 {
				frames = append(frames, framecache.Frame{Timestamp: uint32(3000 + i*3000), EchoTimestamp: 9000, SourceSSRC: 456, Arrival: time.Now(), Keyframe: i == 0, Packets: []framecache.Packet{{SequenceNumber: uint16(991 + i), Marker: true, Payload: []byte{0x10, 0}}}})
			}
			require.True(t, sess.reserveReplay(frames, 8192))
			burst, err := sess.encodeReplay()
			require.NoError(t, err)
			sess.sendReplay(burst)
			callerRx := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
			read := func() uint16 {
				require.NoError(t, receiver.SetReadDeadline(time.Now().Add(time.Second)))
				buf := make([]byte, 2048)
				n, _, err := receiver.ReadFromUDP(buf)
				require.NoError(t, err)
				var h rtp.Header
				_, err = callerRx.DecryptRTP(nil, buf[:n], &h)
				require.NoError(t, err, "caller must never see duplicate SRTP indexes")
				return h.SequenceNumber
			}
			for i := range 10 {
				require.EqualValues(t, 14193+i, read())
			}
			floor := sess.state.Video.ReplayFloor

			callerTx := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
			send := func(seq uint16) {
				raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 456, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, Marker: true}, Payload: []byte{0x10, 1}}).Marshal()
				require.NoError(t, err)
				encrypted, err := callerTx.EncryptRTP(nil, raw, nil)
				require.NoError(t, err)
				sess.handleRTP(encrypted)
			}
			send(1020)
			require.Greater(t, uint64(read()), floor)
			if planned {
				// Exactly the state a subsequent margin-zero resume receives.
				persisted := sess.state
				sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
				sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
				sess.state = persisted
				sess.afterReplay = false
				require.NoError(t, restoreInboundIndex(sess.srtpIn, keys, profile, 456, 1020))
				require.NoError(t, sess.resumeTrack(&sess.state.Video, ResumeOptions{}))
			}
			beforeLate := sess.state.Video.Packets
			for seq := uint16(999); seq >= 937; seq-- {
				send(seq)
				if seq >= 957 && !planned {
					require.Greater(t, uint64(read()), floor)
				}
			}
			if planned {
				require.Equal(t, beforeLate, sess.state.Video.Packets, "planned resume drops all indexes at or below snapshot high water mark")
			}
			// Older than the new inbound 64-packet window: rejected, never echoed.
			require.EqualValues(t, 20, sess.decryptFailures.Load())
		})
	}
}

func TestReplayUnanchoredTimestampAndCaps(t *testing.T) {
	w := newTestWorker(t)
	frame := framecache.Frame{Timestamp: 5000, EchoTimestamp: 0x9000a000, SourceSSRC: 456, Arrival: time.Now(), Keyframe: true, Packets: []framecache.Packet{{SequenceNumber: 1, Marker: true, Payload: []byte{0x10, 0}}}}
	sess := sessionFromState(w, sessionState{ID: "anchor", Video: trackState{SSRC: 123, InitialSeq: 1000}})
	defer sess.close()
	require.True(t, sess.reserveReplay([]framecache.Frame{frame}, 8192))
	require.Equal(t, uint32(0x9000a000+3000), sess.replayTimestamp)
	// Whole groups over duration, bytes or packet budget are skipped.
	for _, kind := range []string{"duration", "bytes", "packets", "burst-duration"} {
		t.Run(kind, func(t *testing.T) {
			frames := []framecache.Frame{frame}
			switch kind {
			case "duration":
				f := frame
				f.Timestamp += 180000
				frames = append(frames, f)
			case "bytes":
				frames[0].Packets = []framecache.Packet{{Payload: make([]byte, 256<<10+1)}}
			case "packets":
				frames[0].Packets = make([]framecache.Packet, 8300)
			case "burst-duration":
				frames[0].Packets = []framecache.Packet{{Payload: make([]byte, 42_000)}}
			}
			before := sess.state.Video
			require.False(t, sess.reserveReplay(frames, 8192))
			require.Equal(t, before, sess.state.Video)
		})
	}
}

type replaySnapshotStore struct {
	sessionstore.Store
	mu              sync.Mutex
	worker          netip.AddrPort
	puts            int
	failReservation bool
}

func (s *replaySnapshotStore) PutState(ctx context.Context, lease sessionstore.Lease, data []byte) error {
	s.mu.Lock()
	targeted := lease.Worker == s.worker
	if targeted {
		s.puts++
	}
	put, fail := s.puts, s.failReservation
	s.mu.Unlock()
	if targeted && put == 2 && fail {
		return errors.New("reservation unavailable")
	}
	if targeted && put > 2 {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.Store.PutState(ctx, lease, data)
}

type stalledCurrent struct{ framecache.Store }

func (s stalledCurrent) Current(ctx context.Context, _ string, _ framecache.Track) ([]framecache.Frame, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Production ResumeSession must durably reserve before sending, and kill must
// leave that reservation intact even when every later snapshot is canceled.
func TestReplayReservationSurvivesImmediateKill(t *testing.T) {
	testReplayResume(t, "kill")
}
func TestReplayReservationFailureFallsBack(t *testing.T) { testReplayResume(t, "failure") }
func TestReplayCacheReadTimeoutIsMiss(t *testing.T)      { testReplayResume(t, "timeout") }
func TestReplayResumeReturnsBeforeBurst(t *testing.T)    { testReplayResume(t, "async") }
func testReplayResume(t *testing.T, mode string) {
	store := &replaySnapshotStore{Store: sessionstore.NewMemory(), failReservation: mode == "failure"}
	cache := framecache.NewMemory(framecache.Limits{})
	r, err := relay.New(relay.Config{PublicAddr: "127.0.0.1:0", WorkerAddr: "127.0.0.1:0", Owners: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	worker := func() *Worker {
		w, err := New(Config{FrameCache: cache, SnapshotInterval: time.Hour, CacheReadTimeout: 25 * time.Millisecond, Relay: &RelayConfig{Owners: store, Addr: r.WorkerAddr(), PublicAddr: r.PublicAddr(), LeaseTTL: time.Minute}})
		require.NoError(t, err)
		r.AddWorker(w.LocalAddr())
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		return w
	}
	a, b, c := worker(), worker(), worker()
	call, _ := dialDTLSCaller(t, a)
	sink := listenTestUDP(t)
	require.True(t, (&testEndpoint{call: call, conn: sink}).check(t, true))
	old := a.session(call.id)
	old.mu.Lock()
	old.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
	old.state.Video.Anchored = true
	old.state.Video.InboundSSRC = 456
	old.state.Video.Packets = 10
	old.state.Video.HighestSentIndex = 6000
	old.state.Video.SeqOffset = 5000
	old.state.Video.LastTimestamp = 9000
	old.state.SRTP.Inbound[456] = 1000
	track := framecache.Track{Kind: old.state.Video.ID, SSRC: old.state.Video.SSRC}
	old.mu.Unlock()
	f := framecache.Frame{Track: track, Timestamp: 3000, EchoTimestamp: 9000, SourceSSRC: 456, Arrival: time.Now(), Keyframe: true}
	for i := range 300 {
		f.Packets = append(f.Packets, framecache.Packet{SequenceNumber: uint16(701 + i), Marker: i == 299, Payload: []byte{0x10, 0}})
	}
	require.NoError(t, cache.Append(context.Background(), call.id, f))
	snapshot, err := a.ExportSession(call.id)
	require.NoError(t, err)
	lease, err := store.Get(context.Background(), call.id)
	require.NoError(t, err)
	lease, err = store.Transfer(context.Background(), lease, b.LocalAddr(), time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.MoveSession(call.id, a.LocalAddr(), b.LocalAddr()))
	store.mu.Lock()
	store.worker = b.LocalAddr()
	store.mu.Unlock()
	if mode == "async" {
		b.cfg.ReplayBitrate = 1_000_000
		b.cfg.ReplayMaxBurstDuration = 300 * time.Millisecond
	}
	if mode == "timeout" {
		b.cfg.FrameCache = stalledCurrent{Store: cache}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = b.ResumeSession(snapshot, ResumeOptions{Context: ctx, Lease: lease, SequenceMargin: 8192})
	require.NoError(t, err)
	require.Less(t, time.Since(start), time.Second, "cache timeout must not consume takeover deadline")
	stored, err := store.GetState(context.Background(), call.id)
	require.NoError(t, err)
	snap, err := decodeSnapshot(stored)
	require.NoError(t, err)
	if mode == "failure" || mode == "timeout" {
		require.Zero(t, snap.State.Video.ReplayFloor)
		reason := "reservation-failure"
		if mode == "timeout" {
			reason = "cache-timeout"
		}
		require.EqualValues(t, 1, b.ReplayStats().Skipped[reason])
		require.False(t, b.session(call.id).replaying)
		require.False(t, b.session(call.id).needsKeyframe, "PLI fallback was sent")
		return
	}
	if mode == "async" {
		sess := b.session(call.id)
		sess.mu.Lock()
		gated := sess.replaying
		sess.mu.Unlock()
		require.True(t, gated, "ResumeSession returns while its paced burst is in flight")
		require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
		buf := make([]byte, 2048)
		for {
			n, _, err := sink.ReadFromUDP(buf)
			require.NoError(t, err)
			if isRTP(buf[:n]) {
				break
			}
		}
		require.NoError(t, b.Close())
		sess.mu.Lock()
		gated = sess.replaying
		sess.mu.Unlock()
		require.False(t, gated, "shutdown waits for the tracked replay to release its gate")
		require.Nil(t, b.session(call.id))
		return
	}
	floor := uint64(14192 + 300)
	require.Equal(t, floor, snap.State.Video.ReplayFloor)
	require.Greater(t, snap.State.Video.HighestSentIndex, floor)
	// Observe actual burst ciphertext through the relay, before killing.
	var h rtp.Header
	for range 300 {
		require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
		buf := make([]byte, 2048)
		n, _, err := sink.ReadFromUDP(buf)
		require.NoError(t, err)
		if isRTCP(buf[:n]) {
			n, _, err = sink.ReadFromUDP(buf)
			require.NoError(t, err)
		}
		_, err = h.Unmarshal(buf[:n])
		require.NoError(t, err)
		require.LessOrEqual(t, uint64(h.SequenceNumber), floor)
	}
	require.EqualValues(t, floor, h.SequenceNumber)
	b.session(call.id).wantSnapshot()
	require.Eventually(t, func() bool { store.mu.Lock(); defer store.mu.Unlock(); return store.puts > 2 }, time.Second, time.Millisecond)
	require.NoError(t, b.kill())
	lease, err = store.Transfer(context.Background(), lease, c.LocalAddr(), time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.MoveSession(call.id, b.LocalAddr(), c.LocalAddr()))
	c.cfg.DisableFrameCache = true
	_, err = c.ResumeSession(stored, ResumeOptions{Lease: lease, SequenceMargin: 8192})
	require.NoError(t, err)
	resumed := c.session(call.id)
	resumed.mu.Lock()
	next, ok := resumed.state.Video.rewrite(&rtp.Header{SSRC: 456, SequenceNumber: 1020, Timestamp: 6000})
	index := extendIndex(resumed.state.Video.HighestSentIndex, next.SequenceNumber)
	resumed.mu.Unlock()
	require.True(t, ok)
	require.Greater(t, index, floor, "second takeover starts above every replayed index")
}

func TestStaleOwnerClosePreservesSuccessorCache(t *testing.T) {
	store := sessionstore.NewMemory()
	w := newTestWorker(t)
	cache := framecache.NewMemory(framecache.Limits{})
	w.cfg.FrameCache = cache
	// No media needs to be established to exercise consent/Worker.Close's
	// shared close path; the token has already transferred without fencing.
	sess := sessionFromState(w, sessionState{ID: "stale-cache"})
	lease, err := store.Claim(context.Background(), sess.id, w.LocalAddr(), time.Minute)
	require.NoError(t, err)
	sess.lease = lease
	w.cfg.Relay = &RelayConfig{Owners: store}
	successor := netip.MustParseAddrPort("127.0.0.1:12345")
	_, err = store.Transfer(context.Background(), lease, successor, time.Minute)
	require.NoError(t, err)
	track := framecache.Track{Kind: "video", SSRC: 1}
	require.NoError(t, cache.Append(context.Background(), sess.id, framecache.Frame{Track: track, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: []byte{1}}}}))
	sess.consentExpired()
	frames, err := cache.Current(context.Background(), sess.id, track)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	owner, err := store.Owner(context.Background(), sess.id)
	require.NoError(t, err)
	require.Equal(t, successor, owner)
}

func TestReplayFloorAndLiveClock(t *testing.T) {
	w := newTestWorker(t)
	sess := sessionFromState(w, sessionState{ID: "floor", SRTP: srtpState{Inbound: make(map[uint32]uint64)}, Video: trackState{MID: "1", ID: "video", SSRC: 123, PayloadType: 96, Anchored: true, InboundSSRC: 456, Packets: 10, HighestSentIndex: 14267, ReplayFloor: 14202, SeqOffset: 13192, LastTimestamp: 9000}})
	defer sess.close()
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 456, PayloadType: 96, SequenceNumber: 1010, Timestamp: 3000}, Payload: []byte{0x10, 1}}).Marshal()
	require.NoError(t, err)
	packet, err := caller.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)
	sess.handleRTP(packet)
	require.EqualValues(t, 10, sess.state.Video.Packets, "persistent floor rejects before encrypt even without runtime replay flags")
	require.Zero(t, sess.decryptFailures.Load(), "caller packet authenticated successfully")
	sess.replayAnchorAt = time.Now().Add(-3 * time.Second)
	header := rtp.Header{}
	sess.continueAfterReplay(&rtp.Packet{Header: rtp.Header{Timestamp: 12000}}, &header)
	require.GreaterOrEqual(t, header.Timestamp, uint32(279000), "first live clock includes the whole wait, beyond the cache arrival-age clamp")
}

func TestReplayPreservesRemainingRetries(t *testing.T) {
	for _, margin := range []uint16{8192, 10700, 10850, 11000} {
		t.Run(fmt.Sprint(margin), func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.ReplayMaxBurstDuration = 100 * time.Millisecond
			sess := sessionFromState(w, sessionState{ID: "retry-budget", Video: trackState{MID: "1", Packets: 10, Anchored: true, AdvanceSinceSend: uint32(margin)}})
			defer sess.close()
			frame := framecache.Frame{Keyframe: true, Arrival: time.Now(), Packets: make([]framecache.Packet, 1024)}
			before := sess.state.Video
			attempts := (maxRetainedSequenceAdvance - int(before.AdvanceSinceSend)) / int(margin)
			replayed := sess.reserveReplay([]framecache.Frame{frame}, margin)
			require.Equal(t, attempts, (maxRetainedSequenceAdvance-int(sess.state.Video.AdvanceSinceSend))/int(margin))
			require.Equal(t, margin <= 10700, replayed)
			if !replayed {
				require.Equal(t, before, sess.state.Video)
			}
		})
	}
}

func TestReplayPacesEncryptedBytes(t *testing.T) {
	for _, rate := range []int{0, 1_000_000} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.ReplayBitrate = rate
			w.cfg.ReplayMaxBurstDuration = 200 * time.Millisecond
			w.cfg.DisableResumePLI = true
			sink := listenTestUDP(t)
			sess := sessionFromState(w, sessionState{ID: "paced", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()}})
			defer sess.close()
			packets := make([][]byte, 10)
			for i := range packets {
				packets[i] = make([]byte, 1000)
			}
			start := time.Now()
			sess.sendReplay(packets)
			if rate == 0 {
				rate = 10_000_000
			}
			require.GreaterOrEqual(t, time.Since(start), time.Duration(9*1000)*8*time.Second/time.Duration(rate)-time.Millisecond)
		})
	}
}

func TestSharedSocketMarginDefersRecoveryUntilRouted(t *testing.T) {
	socket, a, b := newTestSocket(t, testConsentTimeout)
	cache := framecache.NewMemory(framecache.Limits{})
	a.cfg.FrameCache, b.cfg.FrameCache = cache, cache
	call, _ := dialDTLSCaller(t, a)
	old := a.session(call.id)
	old.mu.Lock()
	old.state.Video.Anchored = true
	old.state.Video.InboundSSRC = 456
	old.state.Video.Packets = 1
	old.state.Video.HighestSentIndex = 6000
	old.state.Video.SeqOffset = 5000
	track := framecache.Track{Kind: old.state.Video.ID, SSRC: old.state.Video.SSRC}
	old.mu.Unlock()
	require.NoError(t, cache.Append(context.Background(), call.id, framecache.Frame{Track: track, Keyframe: true, SourceSSRC: 456, Arrival: time.Now(), Packets: []framecache.Packet{{SequenceNumber: 1000, Marker: true, Payload: []byte{0x10, 0}}}}))
	_, err := socket.Handover(call.id, b, ResumeOptions{SequenceMargin: 8192})
	require.NoError(t, err)
	require.Same(t, b, socket.Owner(call.id))
	sess := b.session(call.id)
	sess.mu.Lock()
	pending, floor := sess.needsKeyframe, sess.state.Video.ReplayFloor
	sess.mu.Unlock()
	require.True(t, pending, "adoption must not clear a request dropped by the socket fence")
	require.Zero(t, floor, "shared-socket handovers skip replay")
	// Send a real encrypted source packet after the route switch. The regular
	// packet handler must send the deferred PLI through the now-owned socket.
	keys := testSessionKeys(t, srtp.ProtectionProfileAeadAes128Gcm)
	sess.mu.Lock()
	sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, srtp.ProtectionProfileAeadAes128Gcm)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, srtp.ProtectionProfileAeadAes128Gcm)
	sess.mu.Unlock()
	sink := listenTestUDP(t)
	addr := sink.LocalAddr().(*net.UDPAddr).AddrPort()
	socket.mu.Lock()
	socket.flows[addr] = call.id
	socket.mu.Unlock()
	sess.mu.Lock()
	sess.state.ICE.RemoteAddr = addr
	sess.mu.Unlock()
	caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, srtp.ProtectionProfileAeadAes128Gcm)
	raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: sess.state.Video.PayloadType, SSRC: 456, SequenceNumber: 1001, Marker: true}, Payload: []byte{0x10, 0}}).Marshal()
	require.NoError(t, err)
	encrypted, err := caller.EncryptRTP(nil, raw, nil)
	require.NoError(t, err)
	sess.handleRTP(encrypted)
	sess.mu.Lock()
	pending, index := sess.needsKeyframe, sess.state.Video.SRTCPIndex
	sess.mu.Unlock()
	require.False(t, pending)
	require.Positive(t, index)
	require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
	buf := make([]byte, 2048)
	n, _, err := sink.ReadFromUDP(buf)
	require.NoError(t, err)
	require.False(t, isRTCP(buf[:n]))
	n, _, err = sink.ReadFromUDP(buf)
	require.NoError(t, err)
	require.True(t, isRTCP(buf[:n]), "routed caller receives the deferred PLI")
}

// Every production sendReplay exit must release the gate. Exercise a real UDP
// send, then interrupt pacing; move uses ExportSession's real fence and close.
func TestReplayInterruptionsReleaseGate(t *testing.T) {
	for _, action := range []string{"fence", "cancel", "close", "move", "error", "deadline", "complete"} {
		t.Run(action, func(t *testing.T) {
			w := newTestWorker(t)
			w.cfg.DisableResumePLI = true
			w.cfg.ReplayBitrate = 80_000
			w.cfg.ReplayMaxBurstDuration = 150 * time.Millisecond
			if action == "deadline" {
				w.cfg.ReplayMaxBurstDuration = 20 * time.Millisecond
			}
			call, _ := dialDTLSCaller(t, w)
			sess := w.session(call.id)
			sink := listenTestUDP(t)
			sess.mu.Lock()
			sess.state.ICE.RemoteAddr = sink.LocalAddr().(*net.UDPAddr).AddrPort()
			if action == "error" {
				sess.state.ICE.RemoteAddr = netip.AddrPort{}
			}
			sess.replaying = true
			sess.mu.Unlock()
			done := make(chan struct{})
			first, second := make([]byte, 1000), make([]byte, 1000)
			first[1], second[1] = 0x80, 0x80 // separate complete frames
			go func() { sess.sendReplay([][]byte{first, second}); close(done) }()
			if action != "error" {
				require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
				_, _, err := sink.ReadFromUDP(make([]byte, 2048))
				require.NoError(t, err, "first burst packet leaves before interruption")
			}
			switch action {
			case "fence":
				sess.mu.Lock()
				sess.fenced.Store(true)
				sess.mu.Unlock()
			case "cancel":
				sess.cancel()
			case "close":
				sess.close()
			case "move":
				snapshot, err := w.ExportSession(call.id)
				require.NoError(t, err)
				target := newTestWorker(t)
				_, err = target.ResumeSession(snapshot, ResumeOptions{})
				require.NoError(t, err)
			}
			select {
			case <-done:
			case <-time.After(300 * time.Millisecond):
				t.Fatal("replay did not stop")
			}
			sess.mu.Lock()
			gated := sess.replaying
			sess.mu.Unlock()
			require.False(t, gated)
			require.NoError(t, sink.SetReadDeadline(time.Now().Add(30*time.Millisecond)))
			remaining := 0
			buf := make([]byte, 2048)
			for {
				n, _, err := sink.ReadFromUDP(buf)
				if isTimeout(err) {
					break
				}
				require.NoError(t, err)
				if isDTLS(buf[:n]) {
					continue
				} // close_notify is not replay media
				remaining++
			}
			expected := 0
			if action == "complete" {
				expected = 1
			}
			require.Equal(t, expected, remaining, "no burst packets leave after interruption")
		})
	}
}

func TestReplayBurstDurationBudgetAndHardCaps(t *testing.T) {
	w := newTestWorker(t)
	sess := sessionFromState(w, sessionState{ID: "duration-budget", Video: trackState{Packets: 1}})
	defer sess.close()
	frame := framecache.Frame{Keyframe: true, Arrival: time.Now(), Packets: []framecache.Packet{{Payload: make([]byte, 20_805)}}}
	require.True(t, sess.reserveReplay([]framecache.Frame{frame}, 8192), "20,833 wire bytes fit half a 30 fps interval at 10 Mbps")
	before := sess.state.Video
	frame.Packets[0].Payload = make([]byte, 20_806)
	require.False(t, sess.reserveReplay([]framecache.Frame{frame}, 8192), "RTP/SRTP overhead is part of the time budget")
	require.Equal(t, before, sess.state.Video)
	w.cfg.ReplayMaxBurstDuration = 100 * time.Millisecond
	require.True(t, sess.reserveReplay([]framecache.Frame{frame}, 8192), "explicit duration permits a larger burst")
	w.cfg.ReplayMaxBurstDuration = time.Second
	w.cfg.ReplayMaxBytes = 1 << 20
	frame.Packets[0].Payload = make([]byte, 256<<10+1)
	require.False(t, sess.reserveReplay([]framecache.Frame{frame}, 8192), "256 KiB stays a hard ceiling")
	frame.Packets = make([]framecache.Packet, 1025)
	require.False(t, sess.reserveReplay([]framecache.Frame{frame}, 8192), "1024 packets stay a hard ceiling")
}

func TestReplayNinetyPercentSkippedWithCounter(t *testing.T) {
	w := newTestWorker(t)
	sess := sessionFromState(w, sessionState{ID: "ninety", Video: trackState{Packets: 1}})
	defer sess.close()
	frame := framecache.Frame{Keyframe: true, Arrival: time.Now(), Packets: make([]framecache.Packet, 30)}
	// 37,500 wire bytes are 90% of the 41,666-byte deadline budget.
	for i := range frame.Packets {
		frame.Packets[i] = framecache.Packet{SequenceNumber: uint16(i), Marker: i == 29, Payload: make([]byte, 1222)}
	}
	before := sess.state.Video
	require.False(t, sess.reserveReplay([]framecache.Frame{frame}, 8192))
	require.Equal(t, before, sess.state.Video, "skipped group consumes no reservation")
	stats := w.ReplayStats()
	require.EqualValues(t, 1, stats.Skipped["burst-duration-budget"])
	require.Empty(t, stats.Truncated)
	stats.Skipped["burst-duration-budget"] = 99
	require.EqualValues(t, 1, w.ReplayStats().Skipped["burst-duration-budget"], "snapshots are copied")
}

func TestReplayDeadlineFinishesFrameAndCounts(t *testing.T) {
	w := newTestWorker(t)
	w.cfg.ReplayBitrate = 80_000
	w.cfg.ReplayMaxBurstDuration = time.Second
	w.cfg.DisableResumePLI = true
	sink := listenTestUDP(t)
	sess := sessionFromState(w, sessionState{ID: "boundary", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()}, Video: trackState{SSRC: 123, PayloadType: 96, InitialSeq: 1000}})
	defer sess.close()
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	frames := []framecache.Frame{
		{Keyframe: true, SourceSSRC: 456, Arrival: time.Now(), Packets: []framecache.Packet{{SequenceNumber: 1, Payload: make([]byte, 972)}, {SequenceNumber: 2, Marker: true, Payload: make([]byte, 972)}}},
		{Timestamp: 3000, SourceSSRC: 456, Arrival: time.Now(), Packets: []framecache.Packet{{SequenceNumber: 3, Marker: true, Payload: make([]byte, 972)}}},
	}
	require.True(t, sess.reserveReplay(frames, 8192))
	packets, err := sess.encodeReplay()
	require.NoError(t, err)
	// Force expiration inside the first two-packet keyframe. Its second packet
	// must still leave; truncation may only discard the following frame.
	sess.replayBurstDuration = 20 * time.Millisecond
	sess.replaying = true
	sess.sendReplay(packets)
	caller := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	for i := range 2 {
		require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
		buf := make([]byte, 2048)
		n, _, err := sink.ReadFromUDP(buf)
		require.NoError(t, err)
		var header rtp.Header
		_, err = caller.DecryptRTP(nil, buf[:n], &header)
		require.NoError(t, err)
		require.EqualValues(t, 1000+i, header.SequenceNumber)
		require.Equal(t, i == 1, header.Marker)
	}
	require.NoError(t, sink.SetReadDeadline(time.Now().Add(30*time.Millisecond)))
	_, _, err = sink.ReadFromUDP(make([]byte, 2048))
	require.True(t, isTimeout(err), "next frame is never started")
	require.False(t, sess.replaying)
	require.EqualValues(t, 1, w.ReplayStats().Truncated["deadline"])
	require.Empty(t, w.ReplayStats().Skipped)
}

func TestReplaySkipReasonCounters(t *testing.T) {
	w := newTestWorker(t)
	sess := sessionFromState(w, sessionState{ID: "skip-reasons", Video: trackState{Packets: 1}})
	defer sess.close()
	base := framecache.Frame{Keyframe: true, Arrival: time.Now(), Packets: []framecache.Packet{{Marker: true, Payload: []byte{0}}}}
	tests := []struct {
		reason string
		frames []framecache.Frame
		margin uint16
	}{
		{"cache-miss", nil, 8192},
		{"missing-keyframe", []framecache.Frame{{Packets: base.Packets}}, 8192},
		{"invalid-group", []framecache.Frame{{Keyframe: true}}, 8192},
		{"sequence-budget", []framecache.Frame{base}, 0},
		{"packet-cap", []framecache.Frame{{Keyframe: true, Packets: make([]framecache.Packet, 1025)}}, 8192},
		{"byte-cap", []framecache.Frame{{Keyframe: true, Packets: []framecache.Packet{{Payload: make([]byte, 256<<10+1)}}}}, 8192},
		{"media-duration-cap", []framecache.Frame{base, {Timestamp: 180000, Packets: base.Packets}}, 8192},
	}
	for _, test := range tests {
		require.False(t, sess.reserveReplay(test.frames, test.margin))
		require.EqualValues(t, 1, w.ReplayStats().Skipped[test.reason])
	}
	require.Len(t, w.ReplayStats().Skipped, len(tests))
	require.Empty(t, w.ReplayStats().Truncated)
}

// Exercise queue saturation through decrypted packet handling: the reader must
// return and echo every frame even while the production Store.Append stalls.
func TestFrameCacheQueueBoundsDoNotBlockPacketPath(t *testing.T) {
	w := newTestWorker(t)
	cache := &blockedFrameCache{Store: framecache.NewMemory(framecache.Limits{}), entered: make(chan struct{})}
	w.cfg.FrameCache = cache
	sink := listenTestUDP(t)
	sess := sessionFromState(w, sessionState{ID: "queue", ICE: iceState{RemoteAddr: sink.LocalAddr().(*net.UDPAddr).AddrPort()}, SRTP: srtpState{Inbound: make(map[uint32]uint64)}, Video: trackState{MID: "1", ID: "video", SSRC: 123, PayloadType: 96, InitialSeq: 1200}})
	defer sess.close()
	profile := srtp.ProtectionProfileAeadAes128Gcm
	keys := testSessionKeys(t, profile)
	sess.srtpIn = testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	sess.srtpOut = testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	caller := testContext(t, keys.RemoteMasterKey, keys.RemoteMasterSalt, profile)
	send := func(seq uint16) {
		raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 456, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, Marker: true}, Payload: []byte{0x10, 0}}).Marshal()
		require.NoError(t, err)
		encrypted, err := caller.EncryptRTP(nil, raw, nil)
		require.NoError(t, err)
		sess.handleRTP(encrypted)
	}
	send(1)
	select {
	case <-cache.entered:
	case <-time.After(time.Second):
		t.Fatal("append did not start")
	}
	start := time.Now()
	for seq := uint16(2); seq <= 65; seq++ {
		send(seq)
	}
	require.Less(t, time.Since(start), 500*time.Millisecond, "cache must not hold the reader")
	stats := w.ReplayStats()
	require.Equal(t, cacheQueueFrames, stats.AppendPending)
	require.EqualValues(t, 33, stats.AppendDropped)
	sess.mu.Lock()
	packets := sess.state.Video.Packets
	sess.mu.Unlock()
	require.EqualValues(t, 65, packets, "echo continues under cache load")
	// Observe all 65 echoes at the UDP receiver, rather than trusting counters.
	callerRx := testContext(t, keys.LocalMasterKey, keys.LocalMasterSalt, profile)
	require.NoError(t, sink.SetReadDeadline(time.Now().Add(time.Second)))
	for i := range 65 {
		raw := make([]byte, 2048)
		n, _, err := sink.ReadFromUDP(raw)
		require.NoError(t, err)
		var header rtp.Header
		_, err = callerRx.DecryptRTP(nil, raw[:n], &header)
		require.NoError(t, err)
		require.EqualValues(t, 1200+i, header.SequenceNumber)
	}
	// An oversized new append is also counted while the queue is saturated.
	large := framecache.Frame{Track: framecache.Track{Kind: "video", SSRC: 123}, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: make([]byte, cacheQueueBytes)}}}
	sess.appendFrame(&large)
	require.EqualValues(t, 34, w.ReplayStats().AppendDropped)
	require.LessOrEqual(t, w.ReplayStats().AppendBytes, cacheQueueBytes)
	sess.close() // cancels the in-flight append; queued appends must be discarded
	require.Eventually(t, func() bool { return w.ReplayStats().AppendPending == 0 }, time.Second, time.Millisecond)
	frames, err := cache.Current(context.Background(), sess.id, large.Track)
	require.NoError(t, err)
	require.Empty(t, frames, "queued work cannot resurrect hangup")
}

// Byte pressure drops work before the frame-count cap, counting the active
// append in the budget. This independently exercises payload admission.
func TestFrameCacheQueuePayloadBound(t *testing.T) {
	w := newTestWorker(t)
	cache := &blockedFrameCache{Store: framecache.NewMemory(framecache.Limits{}), entered: make(chan struct{})}
	w.cfg.FrameCache = cache
	sess := sessionFromState(w, sessionState{ID: "byte-bound"})
	defer sess.close()
	f := framecache.Frame{Track: framecache.Track{Kind: "video", SSRC: 123}, Keyframe: true, Packets: []framecache.Packet{{Marker: true, Payload: make([]byte, 1<<20)}}}
	sess.appendFrame(&f)
	select {
	case <-cache.entered:
	case <-time.After(time.Second):
		t.Fatal("append did not start")
	}
	for range 7 {
		sess.appendFrame(&f)
	}
	require.Equal(t, 8, w.ReplayStats().AppendPending)
	require.Equal(t, cacheQueueBytes, w.ReplayStats().AppendBytes)
	sess.appendFrame(&f)
	require.EqualValues(t, 1, w.ReplayStats().AppendDropped)
	require.Equal(t, 8, w.ReplayStats().AppendPending)
	sess.close()
	require.Eventually(t, func() bool { return w.ReplayStats().AppendPending == 0 }, time.Second, time.Millisecond)
}
