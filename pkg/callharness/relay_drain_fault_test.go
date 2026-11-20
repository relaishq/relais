package callharness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/webrtc/v4"
	"github.com/relaishq/relais/internal/relaytrace"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

const (
	faultOff = iota
	// faultRace holds the caller's next audio RTP write on the old pair
	// until Pion's Agent.Restart has run (ICE state checking), then fails it
	// with io.ErrClosedPipe: the stale-pair write of #491.
	faultRace
	// faultDrop discards the caller's audio RTP to the new relay: the caller's
	// audio never reaches it, so the worker has none to send back there.
	faultDrop
)

// callerWriteFault is one caller's media socket with an armed write fault.
type callerWriteFault struct {
	net.PacketConn
	target netip.AddrPort // the new relay's public address
	call   atomic.Pointer[Call]
	mode   atomic.Int32
	held   chan struct{} // closed when faultRace holds a write
	raced  chan struct{} // closed when the held write fails after the restart
}

func (f *callerWriteFault) WriteTo(p []byte, addr net.Addr) (int, error) {
	to := addr.(*net.UDPAddr).AddrPort()
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	// SRTP keeps the RTP header in the clear; RTCP types 200-204 never match.
	if !stun.IsMessage(p) && len(p) >= 2 && p[0]>>6 == 2 && p[1]&0x7f == opusPayloadType {
		switch f.mode.Load() {
		case faultRace:
			if to != f.target && f.mode.CompareAndSwap(faultRace, faultOff) {
				close(f.held)
				pc := f.call.Load().pc
				for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
					if pc.ICEConnectionState() == webrtc.ICEConnectionStateChecking {
						close(f.raced)
						break
					}
				}
				return 0, io.ErrClosedPipe
			}
		case faultDrop:
			if to == f.target {
				return len(p), nil
			}
		}
	}
	return f.PacketConn.WriteTo(p, addr)
}

// faultTopology is relayChangeTopology with callers whose sockets can fault.
// It dials calls until want of them are on r1, hangs up the rest, and
// returns the r1 calls with their sockets.
func faultTopology(t *testing.T, ctx context.Context, store sessionstore.Store, want int) (*controlplane.Plane, *mediaworker.Worker, []*Call, []*callerWriteFault) {
	t.Helper()
	p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
	var mu sync.Mutex
	var sockets []*callerWriteFault
	faulty, err := Start(Options{External: &ExternalTopology{
		SignalingURL: h.external.SignalingURL,
		RelayAddr:    h.external.RelayAddr,
		CallerSocket: func() (net.PacketConn, error) {
			conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
			if err != nil {
				return nil, err
			}
			f := &callerWriteFault{PacketConn: conn, target: relays[1].PublicAddr(), held: make(chan struct{}), raced: make(chan struct{})}
			mu.Lock()
			sockets = append(sockets, f)
			mu.Unlock()
			return f, nil
		},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, faulty.Close()) })
	var calls, other []*Call
	var faults []*callerWriteFault
	for len(calls) < want {
		require.Less(t, len(calls)+len(other), 4*want)
		c, e := faulty.Dial(ctx, CallOptions{Video: true, RelayChange: true})
		require.NoError(t, e)
		_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
		require.NoError(t, e)
		if a.Slot == "r1" {
			mu.Lock()
			faults = append(faults, sockets[len(sockets)-1])
			mu.Unlock()
			faults[len(faults)-1].call.Store(c)
			calls = append(calls, c)
		} else {
			other = append(other, c)
		}
	}
	for _, c := range other {
		_, e := c.Hangup(ctx)
		require.NoError(t, e)
	}
	return p, w, calls, faults
}

// TestRelayDrainSurvivesCallerRestartWriteRace forces the #491 failure: an
// audio write that chose the old pair is held until the caller's ICE restart
// has closed that pair, then fails with io.ErrClosedPipe. The caller then
// stopped sending audio for good, so the worker had no audio to return on
// the new relay and the drain reported delivery_unconfirmed for MID 0 at the
// 10 s planned limit. A browser keeps sending through its ICE restart; so
// must the test caller.
func TestRelayDrainSurvivesCallerRestartWriteRace(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p, _, calls, faults := faultTopology(t, ctx, store, 1)
		watch := watchRelayAttempts(store, calls)
		sent := make(chan error, 1)
		go func() { sent <- watch.sendMedia(ctx, calls[0], 6*time.Second) }()
		time.Sleep(time.Second)
		// Hold an audio write on the old pair first, then start the drain:
		// the caller's ICE restart closes that pair under the held write.
		faults[0].mode.Store(faultRace)
		select {
		case <-faults[0].held:
		case <-time.After(time.Second):
			t.Fatal("no audio write to hold")
		}
		start := time.Now()
		err := p.DrainRelay(ctx, "r1")
		watch.stop()
		require.NoError(t, err, "drain evidence:\n%s", watch.evidence(calls, start))
		select {
		case <-faults[0].raced:
		default:
			t.Fatal("the held write must fail after the caller's ICE restart closed its pair")
		}
		require.NoError(t, <-sent, "a write that races the caller's own ICE restart drops one frame, not the track")
		_, a, err := store.(sessionstore.RelayAssignments).RelayOwner(ctx, calls[0].SessionID())
		require.NoError(t, err)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, a)
		_, err = calls[0].Hangup(ctx)
		require.NoError(t, err)
	})
}

// TestRelayDrainUnconfirmedCallEvidence drops one caller's audio on r2.
// The other call confirms there. Three failed planned attempts each roll
// back to r1; the drain identifies the one not-movable call. Control's saved
// attempts, the worker's per-MID evidence and the caller's report name audio
// (MID 0) as missing on r2. The call stays live with confirmed media on r1.
func TestRelayDrainUnconfirmedCallEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for three 10 s planned attempts and confirmed rollbacks")
	}
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		p, w, calls, faults := faultTopology(t, ctx, store, 2)
		watch := watchRelayAttempts(store, calls)
		defer watch.stop()
		media, stopMedia := context.WithCancel(ctx)
		defer stopMedia()
		sent := make(chan error, len(calls))
		for _, c := range calls {
			go func() { sent <- watch.sendMedia(media, c, 90*time.Second) }()
		}
		time.Sleep(time.Second)
		faults[0].mode.Store(faultDrop)
		missing, confirmed := calls[0].SessionID(), calls[1].SessionID()
		start := time.Now()
		drained := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			response := httptest.NewRecorder()
			p.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/relays/r1/drain", nil).WithContext(ctx))
			drained <- response
		}()
		// Capture generation 1 while it is current. Confirmed rollback
		// supersedes it, and the worker correctly rejects stale reads.
		var report mediaworker.RelayDeliveryReport
		require.Eventually(t, func() bool {
			var e error
			report, e = w.RelayDelivery(ctx, missing, 1)
			return e == nil && report.Switched && slices.Equal(report.Delivered, []string{"1"})
		}, 8*time.Second, 20*time.Millisecond)
		confirmations := store.(sessionstore.RelayConfirmations)
		require.Eventually(t, func() bool {
			a, rec, e := confirmations.RelayConfirmation(ctx, missing)
			return e == nil && a.Slot == "r2" && a.Generation == 1 && rec.Attempt != nil &&
				!rec.Attempt.CommitAt.IsZero() && time.Now().After(rec.Attempt.CommitAt.Add(9*time.Second))
		}, 10*time.Second, 20*time.Millisecond, "capture caller evidence late in generation 1, while still on r2")
		callerOnR2 := calls[0].rec.report()
		response := <-drained
		elapsed := time.Since(start)
		after, afterRec, afterErr := confirmations.RelayConfirmation(ctx, missing)
		intents, intentsErr := store.(sessionstore.ControlStore).ControlIntents(ctx)
		watch.stop()
		t.Logf("drain evidence:\n%s", watch.evidence(calls, start))
		require.Equal(t, http.StatusOK, response.Code)
		var result controlplane.RelayDrainResult
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Len(t, result.Moves, 2)
		require.ElementsMatch(t, []string{missing, confirmed}, []string{result.Moves[0].ID, result.Moves[1].ID})
		require.Equal(t, 1, result.NotMovable)
		for _, move := range result.Moves {
			if move.ID == missing {
				require.True(t, move.NotMovable)
				require.Empty(t, move.To)
				require.Contains(t, move.Error, "relay change unconfirmed: delivery_unconfirmed")
				require.Equal(t, 1, strings.Count(move.Error, "delivery_unconfirmed"))
			} else {
				require.Equal(t, confirmed, move.ID)
				require.Equal(t, "r2", move.To)
				require.Empty(t, move.Error)
				require.False(t, move.NotMovable)
			}
		}
		require.GreaterOrEqual(t, elapsed, 33*time.Second, "three planned limits plus the 1 s and 2 s backoff")

		a, rec, e := confirmations.RelayConfirmation(ctx, confirmed)
		require.NoError(t, e)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, a)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, rec.Attempt.Phase)

		watch.mu.Lock()
		failed := watch.attempts[missing][1]
		for _, generation := range []uint64{1, 3, 5} {
			attempt := watch.attempts[missing][generation]
			require.Equal(t, "r2", attempt.Slot)
			require.Equal(t, []string{"0", "1"}, attempt.Required)
			require.Equal(t, []string{"1"}, attempt.Delivered, "video was delivered; audio has no evidence")
		}
		watch.mu.Unlock()

		require.True(t, report.Switched)
		require.Equal(t, "r2", report.Slot)
		require.Equal(t, []string{"0", "1"}, report.Required)
		require.Equal(t, []string{"1"}, report.Delivered)

		require.True(t, slices.ContainsFunc(callerOnR2.Tracks, func(track TrackReport) bool { return track.Kind == kindAudio }))
		for _, track := range callerOnR2.Tracks {
			if track.Kind == kindAudio {
				require.True(t, calls[0].rec.start.Add(track.LastArrival).Before(failed.CommitAt), "the caller really received no audio on r2")
			}
		}
		// Every failed move has a higher-generation confirmed rollback.
		// Recovery is settled and the call is still live on its original slot.
		require.NoError(t, afterErr)
		require.Equal(t, sessionstore.RelayAssignment{Slot: "r1", Generation: 6}, after)
		require.Equal(t, sessionstore.RelayPhaseConfirmed, afterRec.Attempt.Phase)
		require.Equal(t, []string{"0", "1"}, afterRec.Attempt.Delivered)
		require.False(t, afterRec.Episode.Open())
		_, e = store.Get(ctx, missing)
		require.NoError(t, e, "the failed drain must not delete the live session")
		require.NoError(t, intentsErr)
		require.False(t, slices.ContainsFunc(intents, func(i sessionstore.ControlIntent) bool { return i.ID == "relay-recovery:"+missing }), "confirmed rollback settles recovery")
		require.Eventually(t, func() bool {
			now := calls[0].rec.report()
			return slices.ContainsFunc(now.Tracks, func(track TrackReport) bool {
				return track.Kind == kindAudio && calls[0].rec.start.Add(track.LastArrival).After(start.Add(elapsed))
			})
		}, 2*time.Second, 20*time.Millisecond, "audio resumes after the final rollback")
		stopMedia()
		for range calls {
			<-sent
		}
		for _, c := range calls {
			_, e := c.Hangup(ctx)
			require.NoError(t, e)
		}
	})
}

// A refused offer stays outstanding in Pion, and the rollback restart reuses
// it without calling CreateOffer. That reuse restarts nothing, so a closed
// pipe during its signaling exchange is not a restart race and still ends
// the send (#491 review).
func TestReusedRestartOfferOpensNoWriteRaceWindow(t *testing.T) {
	p, _, h, _, _ := relayChangeTopology(t, sessionstore.NewMemory(), true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	call, err := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
	require.NoError(t, err)
	sent := make(chan error, 1)
	go func() { sent <- call.SendMedia(ctx, 4*time.Second) }()
	time.Sleep(time.Second)
	type post struct {
		reused, tolerated bool
		restarts          uint64
	}
	var mu sync.Mutex
	var posts []post
	created := false // CreateOffer ran since the previous post
	stop := relaytrace.Observe(func(_ time.Time, name, _ string, _ []byte) {
		mu.Lock()
		defer mu.Unlock()
		switch name {
		case "offer-created-pair":
			created = true
		case "offer-post":
			restarts := call.iceRestarts.Load()
			posts = append(posts, post{!created, call.restartRaced(io.ErrClosedPipe, restarts), restarts})
			created = false
		}
	})
	defer stop()
	require.Error(t, p.ChangeRelay(ctx, call.SessionID(), "r2"), "the refused change rolls back")
	stop()
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, posts, 2, "the refused offer, then its reuse for the rollback")
	require.False(t, posts[0].reused)
	require.True(t, posts[1].reused, "the rollback reuses the outstanding offer")
	for _, p := range posts {
		require.Zero(t, p.restarts%2, "no restart window is open during the signaling exchange")
		require.False(t, p.tolerated, "a closed pipe there still ends the send")
	}
	require.NoError(t, <-sent)
	_, err = call.Hangup(ctx)
	require.NoError(t, err)
}

// joinedLines mirrors how Pion's WriteSample joins per-packet errors: one
// line per part, matching a target when any part does.
type joinedLines []error

func (e joinedLines) Error() string {
	var lines []string
	for _, err := range e {
		lines = append(lines, err.Error())
	}
	return strings.Join(lines, "\n")
}

func (e joinedLines) Is(target error) bool {
	return slices.ContainsFunc(e, func(err error) bool { return errors.Is(err, target) })
}

// Only a write that failed purely with closed pipes, inside the caller's own
// ICE restart, drops a frame. A closed pipe joined with any other error, or
// outside a restart, still ends the send (#491 review).
func TestRestartRacedNeedsOnlyClosedPipes(t *testing.T) {
	var c Call
	c.iceRestarts.Store(1) // CreateOffer is restarting the agent
	terminal := errors.New("srtp: terminal failure")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"closed pipe", io.ErrClosedPipe, true},
		{"joined closed pipes", joinedLines{io.ErrClosedPipe, io.ErrClosedPipe}, true},
		{"closed pipe and no candidate pairs", joinedLines{io.ErrClosedPipe, ice.ErrNoCandidatePairs}, false},
		{"closed pipe and terminal", joinedLines{terminal, io.ErrClosedPipe}, false},
		{"errors.Join closed pipe and terminal", errors.Join(io.ErrClosedPipe, terminal), false},
		{"nested joins", joinedLines{joinedLines{io.ErrClosedPipe, terminal}}, false},
		{"terminal", terminal, false},
	} {
		require.Equal(t, tc.want, c.restartRaced(tc.err, 1), tc.name)
	}
	c.iceRestarts.Store(2)
	require.False(t, c.restartRaced(io.ErrClosedPipe, 2), "outside a restart a closed pipe ends the send")
	require.True(t, c.restartRaced(io.ErrClosedPipe, 0), "a restart that began during the write")
}

// A video frame whose write failed still consumes its identity, as Pion's
// payloader still consumes its PictureID. The frame after a dropped delta
// then breaks the decode chain instead of looking consecutive (#491 review).
func TestDroppedVideoWriteConsumesIdentity(t *testing.T) {
	r := newRecorder()
	track := r.addTrack(kindVideo, 1, 96)
	src, err := newVP8Source(callerVideo)
	require.NoError(t, err)
	const dropped = 5
	for i := range 10 {
		data, key, err := src.next()
		require.NoError(t, err)
		data = r.sendingVideo(data, nil, time.Second/30)
		source := r.sentVideoFrames[uint16(i)]
		require.Equal(t, uint64(i), source.identity, "identities advance per attempted frame")
		source.at = time.Second + time.Duration(i)*time.Second/30
		r.sentVideoFrames[uint16(i)] = source
		if i == dropped {
			require.False(t, key, "the dropped frame is a delta")
			continue // its write failed: never counted, never delivered
		}
		r.sent(kindVideo, key)
		f := vp8Frame{data: data, keyframe: key, pictureID: uint16(i), timestamp: uint32(i * 3000), firstSeq: uint16(i), lastSeq: uint16(i)}
		f.completedAt = r.start.Add(source.at + 10*time.Millisecond)
		size, decodeErr, repeated := r.decodeVideoFrame(track, &f, f.completedAt)
		require.NoError(t, decodeErr)
		require.False(t, repeated)
		r.videoFrame(track, &f, size, decodeErr, f.completedAt)
	}
	v := track.video
	require.Equal(t, 9, r.sentVideo.Frames)
	require.Zero(t, r.contentIdentityErrors)
	require.Equal(t, 1, v.FrameGaps)
	require.Equal(t, dropped, v.DecodableFrames, "no frame after the dropped delta decodes before a keyframe")
	require.Equal(t, 4, v.UndecodableFrames)
}
