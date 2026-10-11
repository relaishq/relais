package callharness

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

// These trials start each wrapping audio stream at its last ROC-zero packet.
// Video leaves enough space for the seeded keyframe, then wraps in seconds.
// All continuity, gaps, crypto and decode assertions use caller observations.
var nearWrap = RTPSequenceNumbers{Audio: 65535, Video: 65500}
var lowSequence = RTPSequenceNumbers{Audio: 1200, Video: 1200}

func TestRelayPlannedWrap(t *testing.T) {
	forWrapSessionStores(t, testRelayPlannedWrap)
}

func testRelayPlannedWrap(t *testing.T, store sessionstore.Store) {
	for _, direction := range []string{"inbound", "outbound", "both"} {
		t.Run(direction, func(t *testing.T) {
			ctx, h, call, capture := startWrapCall(t, direction, store)
			seedWrapMedia(t, call)
			// Three moves of one transport: the first export is before both wraps,
			// and later exports preserve advanced ROCs. SSRCs and keys stay fixed.
			for i, to := range []int{1, 0, 1} {
				require.NoError(t, call.Handover(HandoverOptions{To: to}))
				duration := 300 * time.Millisecond
				if i == 2 {
					duration = 2200 * time.Millisecond
				}
				require.NoError(t, call.SendMedia(ctx, duration))
			}
			failures, err := h.workers.list[1].SessionDecryptFailures(call.SessionID())
			require.NoError(t, err)
			require.Zero(t, failures)
			report, err := call.Hangup(ctx)
			require.NoError(t, err)
			require.Len(t, report.Moves, 3)
			assertWrapReport(t, call, report, capture, direction, false)
			for i, move := range report.Moves {
				require.Equal(t, [2]int{[]int{0, 1, 0}[i], []int{1, 0, 1}[i]}, [2]int{move.From, move.To})
				for _, track := range move.Tracks {
					require.Zero(t, track.SkippedSequenceNumbers)
				}
			}
		})
	}
}

func TestRelayTakeoverWrap(t *testing.T) {
	forWrapSessionStores(t, testRelayTakeoverWrap)
}

func testRelayTakeoverWrap(t *testing.T, store sessionstore.Store) {
	for _, direction := range []string{"inbound", "outbound", "both", "outbound-primer"} {
		for _, afterWrap := range []bool{false, true} {
			if direction == "outbound-primer" && afterWrap {
				continue
			}
			timing := "kill-before-wrap"
			if afterWrap {
				timing = "stale-snapshot-kill-after-wrap"
			}
			t.Run(direction+"/"+timing, func(t *testing.T) {
				ctx, h, call, capture := startWrapCall(t, direction, store)
				old := h.workers.list[0]
				require.NoError(t, h.WaitForSnapshot(ctx, 0, call.SessionID()))
				// Gate all periodic/first-packet/ROC wakeups. A single explicit copy
				// below is the deterministic pre-wrap state used after the hard kill.
				snapshotGate := make(chan struct{})
				var snapshotOnce sync.Once
				require.NoError(t, workerprobe.SetBeforeSnapshot(old.LocalAddr(), func(lifetime context.Context, id string) {
					if id != call.SessionID() {
						return
					}
					snapshotOnce.Do(func() { close(snapshotGate) })
					<-lifetime.Done()
				}))
				seedWrapMedia(t, call)
				select {
				case <-snapshotGate:
				case <-ctx.Done():
					t.Fatal("snapshot gate not reached")
				}
				state, err := old.SnapshotSession(call.SessionID())
				require.NoError(t, err)
				// Check the fixture timing, never print exported keys or transport bytes.
				var snap struct {
					State struct {
						SRTP         struct{ Inbound map[uint32]uint64 }
						Audio, Video struct{ HighestSentIndex uint64 }
					}
				}
				require.NoError(t, json.Unmarshal(state, &snap))
				for _, index := range snap.State.SRTP.Inbound {
					require.Less(t, index, uint64(1<<16))
				}
				require.Less(t, snap.State.Audio.HighestSentIndex, uint64(1<<16))
				if direction == "outbound-primer" {
					require.Equal(t, uint64(65535), snap.State.Audio.HighestSentIndex+8192)
				}
				require.Less(t, snap.State.Video.HighestSentIndex, uint64(1<<16))
				owners := h.workers.relay.owners
				lease, err := owners.Get(ctx, call.SessionID())
				require.NoError(t, err)
				require.NoError(t, owners.PutState(ctx, lease, state))
				t.Logf("WRAP_SNAPSHOT direction=%s kill_after_wrap=%t inbound_audio=%d inbound_video=%d outbound_audio=%d outbound_video=%d", direction, afterWrap, snap.State.SRTP.Inbound[call.audioSSRC], snap.State.SRTP.Inbound[call.videoSSRC], snap.State.Audio.HighestSentIndex, snap.State.Video.HighestSentIndex)

				sent := make(chan error, 1)
				if afterWrap {
					wrapped := make(chan struct{})
					crossed := map[uint8]bool{}
					inbound, outbound := wrapSequences(direction)
					target := inbound
					if direction == "outbound" || direction == "outbound-primer" {
						target = outbound
					}
					require.NoError(t, workerprobe.SetAfterEcho(old.LocalAddr(), func(lifetime context.Context, id string, raw []byte) {
						if id != call.SessionID() {
							return
						}
						var header rtp.Header
						if _, err := header.Unmarshal(raw); err != nil {
							return
						}
						start, edge := outbound.Audio, target.Audio
						if header.PayloadType == vp8PayloadType {
							start, edge = outbound.Video, target.Video
						}
						distance := uint32(header.SequenceNumber - start)
						if uint32(edge)+distance >= 1<<16 {
							crossed[header.PayloadType] = true
						}
						if crossed[opusPayloadType] && crossed[vp8PayloadType] {
							close(wrapped)
							<-lifetime.Done() // Kill cancels this while discarding, never exports.
						}
					}))
					go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
					select {
					case <-wrapped:
					case <-ctx.Done():
						t.Fatal("both tracks did not cross the wrap")
					}
					require.NoError(t, h.Kill(0))
				} else {
					// While the worker is dead the caller wraps; first resumed ingress
					// must derive ROC 1 from the pre-wrap primer, and the outbound margin
					// jumps across the echoed wrap independently.
					require.NoError(t, h.Kill(0))
					if direction == "outbound-primer" {
						// Pause caller media during recovery so the first resumed audio
						// packet is exactly 65535 -> 0, without an outage-source jump.
						require.Eventually(t, func() bool {
							status, err := h.Status(ctx)
							return err == nil && len(status.Takeovers) == 1 && len(status.Calls) == 1 && status.Calls[0].Owner == "1"
						}, 2*time.Second, 5*time.Millisecond)
					}
					go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
				}
				require.Eventually(t, func() bool {
					status, err := h.Status(ctx)
					return err == nil && len(status.Takeovers) == 1 && len(status.Calls) == 1 && status.Calls[0].Owner == "1" && !status.Takeovers[0].Lost && !status.Workers[0].Recovering
				}, 2*time.Second, 5*time.Millisecond)
				require.NoError(t, <-sent)
				failures, err := h.workers.list[1].SessionDecryptFailures(call.SessionID())
				require.NoError(t, err)
				require.Zero(t, failures, "resumed ingress decrypts across the wrap")
				report, err := call.Hangup(ctx)
				require.NoError(t, err)
				require.Len(t, report.Moves, 1)
				assertWrapReport(t, call, report, capture, direction, true)

			})
		}
	}
}

type wrapCapture struct {
	sync.Mutex
	sent map[uint32][]uint16
}

func wrapSequences(direction string) (inbound, outbound RTPSequenceNumbers) {
	inbound, outbound = lowSequence, lowSequence
	if direction == "inbound" || direction == "both" {
		inbound = nearWrap
	}
	if direction != "inbound" {
		outbound = nearWrap
	}
	if direction == "outbound-primer" {
		// One seeded audio packet leaves H + 8192 = 65535. The very first
		// resumed echo must wrap, so SetROC alone cannot restore this context.
		outbound = RTPSequenceNumbers{Audio: 57343, Video: 57310}
	}
	return
}

func startWrapCall(t *testing.T, direction string, store sessionstore.Store) (context.Context, *Harness, *Call, *wrapCapture) {
	t.Helper()
	h, err := Start(Options{Relay: true, Workers: 2, SnapshotInterval: time.Hour, SessionStore: store})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	inbound, outbound := wrapSequences(direction)
	for _, w := range h.workers.list {
		require.NoError(t, workerprobe.SetInitialSequences(w.LocalAddr(), outbound.Audio, outbound.Video))
	}
	call, err := h.Dial(ctx, CallOptions{Video: true, InitialSequenceNumbers: &inbound})
	require.NoError(t, err)
	capture := &wrapCapture{sent: make(map[uint32][]uint16)}
	call.socket.observer.mu.Lock()
	call.socket.observer.inspectSend = func(raw []byte) {
		if len(raw) < 12 || raw[0]>>6 != 2 || raw[1] >= 192 && raw[1] <= 223 {
			return
		}
		var header rtp.Header
		if _, err := header.Unmarshal(raw); err != nil {
			return
		}
		if header.SSRC != call.audioSSRC && header.SSRC != call.videoSSRC {
			return
		}
		capture.Lock()
		capture.sent[header.SSRC] = append(capture.sent[header.SSRC], header.SequenceNumber)
		capture.Unlock()
	}
	call.socket.observer.mu.Unlock()
	return ctx, h, call, capture
}

func seedWrapMedia(t *testing.T, call *Call) {
	t.Helper()
	audio, err := newOpusSource(callerAudio)
	require.NoError(t, err)
	frame, duration, err := audio.next()
	require.NoError(t, err)
	call.rec.sending(kindAudio, frame)
	require.NoError(t, call.audio.WriteSample(media.Sample{Data: frame, Duration: duration}))
	call.rec.sent(kindAudio, false)
	video, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	frame, keyframe, err := video.next()
	require.NoError(t, err)
	call.rec.sendingVideo(frame, nil, video.frameDuration)
	require.NoError(t, call.video.WriteSample(media.Sample{Data: frame, Duration: video.frameDuration}))
	call.rec.sent(kindVideo, keyframe)
	require.Eventually(t, func() bool {
		call.rec.mu.Lock()
		defer call.rec.mu.Unlock()
		return len(call.rec.tracks) == 2 && len(call.rec.tracks[0].headers) > 0 && len(call.rec.tracks[1].headers) > 0 && call.rec.tracks[0].kind != call.rec.tracks[1].kind &&
			((call.rec.tracks[0].video != nil && call.rec.tracks[0].video.Frames == 1) || (call.rec.tracks[1].video != nil && call.rec.tracks[1].video.Frames == 1))
	}, time.Second, time.Millisecond, "seeded keyframe echoed before the move")
	// Keep this timing guarantee if the encoded fixture or packetizer changes:
	// neither direction may have wrapped while sending the seed keyframe.
	call.rec.mu.Lock()
	defer call.rec.mu.Unlock()
	for _, track := range call.rec.tracks {
		initial := call.initialSequences.Audio
		if track.kind == kindVideo {
			initial = call.initialSequences.Video
		}
		lastInbound := uint32(initial) + uint32(len(track.headers)) - 1               //nolint:gosec // one seeded frame, bounded by the fixture
		lastOutbound := uint32(track.headers[0].seq) + uint32(len(track.headers)) - 1 //nolint:gosec // one seeded frame, bounded by the fixture
		require.LessOrEqual(t, lastInbound, uint32(0xffff), "%s seed precedes inbound wrap", track.kind)
		require.LessOrEqual(t, lastOutbound, uint32(0xffff), "%s seed precedes outbound wrap", track.kind)
	}
}

func assertWrapReport(t *testing.T, call *Call, report *Report, capture *wrapCapture, direction string, takeover bool) {
	t.Helper()
	require.True(t, report.ConnectedThroughout())
	require.Zero(t, report.DecryptionFailures.Total())
	require.Zero(t, report.Renegotiations)
	require.Zero(t, report.ICERestarts)
	require.Positive(t, report.Consent.ResponsesAfter)
	require.Len(t, report.Tracks, 2)
	inbound, outbound := wrapSequences(direction)
	for _, track := range report.Tracks {
		require.Positive(t, track.Packets)
		require.Zero(t, track.DuplicatePackets)
		require.Zero(t, track.OutOfOrderPackets)
		require.Zero(t, track.UnmatchedPayloads)
		ssrc, startIn, startOut := call.audioSSRC, inbound.Audio, outbound.Audio
		if track.Kind == kindVideo {
			ssrc, startIn, startOut = call.videoSSRC, inbound.Video, outbound.Video
		}
		capture.Lock()
		sent := append([]uint16(nil), capture.sent[ssrc]...)
		capture.Unlock()
		require.NotEmpty(t, sent)
		require.Equal(t, startIn, sent[0])
		if direction == "inbound" || direction == "both" {
			assertObservedWrap(t, "sent "+track.Kind, sent, true)
		}
		call.rec.mu.Lock()
		var received []uint16
		for _, r := range call.rec.tracks {
			if r.kind == track.Kind {
				for _, h := range r.headers {
					received = append(received, h.seq)
				}
			}
		}
		call.rec.mu.Unlock()
		require.NotEmpty(t, received)
		require.Equal(t, startOut, received[0])
		if direction == "outbound-primer" && track.Kind == kindAudio {
			require.Greater(t, len(received), 1)
			require.Zero(t, received[1], "first resumed packet itself wraps")
		}
		if direction != "inbound" {
			assertObservedWrap(t, "received "+track.Kind, received, !takeover)
		}
		if !takeover {
			require.Zero(t, track.SequenceDiscontinuities)
		}
	}
	for i, move := range report.Moves {
		require.Empty(t, move.Error)
		require.Zero(t, move.DecryptionFailuresAfterResume)
		for _, track := range move.Tracks {
			require.Positive(t, track.PacketsAfter)
			limit := 100 * time.Millisecond
			if wrapRaceDetector {
				limit = 300 * time.Millisecond
			}
			if takeover {
				limit = 2 * time.Second
			}
			require.Less(t, track.Gap, limit)
			if takeover {
				skipped := track.SkippedSequenceNumbers
				if track.Kind == kindVideo && move.Recovery.ReplayPackets > 0 {
					skipped = move.Recovery.SourceVideoSkippedSequenceNumbers
				}
				require.GreaterOrEqual(t, skipped, 8192, "source media retains the full takeover margin")
			}
			if track.Kind == kindVideo {
				require.Positive(t, track.FirstDecodableFrameAfter)
				if takeover {
					require.Less(t, track.FirstDecodableFrameAfter, 500*time.Millisecond)
				}
			}
			t.Logf("WRAP_MOVE direction=%s takeover=%t move=%d track=%s gap=%s skipped=%d decoded_after=%s failures_after_resume=%d", direction, takeover, i+1, track.Kind, track.Gap, track.SkippedSequenceNumbers, track.FirstDecodableFrameAfter, move.DecryptionFailuresAfterResume)
		}
	}
	video := report.Track(kindVideo).Video
	require.Positive(t, video.KeyframesDecoded)
	require.Zero(t, video.KeyframeDecodeErrors)
	require.Zero(t, video.UnmatchedFrames)
	if os.Getenv("RELAIS_HARNESS_REQUIRE_FFMPEG") != "" {
		require.True(t, video.FullDecode.Ran())
	}
	if video.FullDecode.Ran() {
		require.Empty(t, video.FullDecode.Errors)
		require.Equal(t, video.FullDecode.FramesIn, video.FullDecode.FramesDecoded)
	}
	t.Logf("WRAP_METRICS direction=%s takeover=%t decryption_failures=%d decoded=%d/%d consent_after=%d", direction, takeover, report.DecryptionFailures.Total(), video.FullDecode.FramesDecoded, video.FullDecode.FramesIn, report.Consent.ResponsesAfter)
}

func assertObservedWrap(t *testing.T, track string, seq []uint16, adjacent bool) {
	t.Helper()
	for i := 1; i < len(seq); i++ {
		if seq[i-1] > 1<<15 && seq[i] < 1<<15 {
			if adjacent {
				require.Equal(t, uint16(65535), seq[i-1])
				require.Zero(t, seq[i])
			}
			t.Logf("WRAP_SEQUENCE track=%s before=%d after=%d skipped=%d", track, seq[i-1], seq[i], int(uint16(seq[i]-seq[i-1]))-1)
			return
		}
	}
	t.Fatalf("%s: no observed 16-bit wrap", track)
}
