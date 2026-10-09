package callharness

import (
	"image"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	kindAudio = "audio"
	kindVideo = "video"
)

// recorder collects what the caller observes during a call. Times are
// offsets from the moment the caller started dialing. Nothing is recorded
// after hangup, so tearing the call down does not show up as a failure.
type recorder struct {
	start time.Time

	mu sync.Mutex

	hungUp      bool
	hungUpAt    time.Duration
	negotiated  bool // the initial offer/answer exchange has completed
	connected   bool
	connectedAt time.Duration

	offerAnswerExchanges int
	connectionStates     []StateChange
	iceStates            []StateChange
	iceConnected         bool
	renegotiations       int
	iceRestarts          int
	decryptionFailures   DecryptionFailures
	decryptFailureTimes  []time.Duration

	// What the caller sent, by kind. sentFrames holds every distinct frame
	// (an Opus packet or a VP8 frame) so echoes can be matched to it.
	sentAudio        SentTrack
	sentVideo        SentTrack
	sentFrames       map[string]map[string]struct{}
	firstVideoSentAt time.Duration
	keyframeRequests int // PLI/FIR the caller sent for the echoed video

	tracks []*trackRecord

	// Handovers and consent checks; see handover.go.
	moves   []moveRecord
	consent []consentSample
}

type trackRecord struct {
	kind        string
	ssrc        uint32
	payloadType uint8

	arrivals           []time.Duration
	firstArrival       time.Duration
	lastArrival        time.Duration
	gap                time.Duration // longest interval between consecutive packets
	gapEndedAt         time.Duration
	lastSeq            uint16
	seqDiscontinuities int
	duplicatePackets   int
	outOfOrderPackets  int
	unmatchedPayloads  int
	headers            []rtpMark // sequence number and timestamp of each arrival

	video *videoRecord // video tracks only
}

// videoRecord follows the decodability of a received video track; see
// video.go for the method.
type videoRecord struct {
	VideoReport

	chain    bool // every frame since the last decoded keyframe is decodable
	haveLast bool
	lastSeq  uint16 // last packet of the previous complete frame
	lastTS   uint32

	// decodeInput is every complete frame from the first decoded keyframe on,
	// for the full decode at hangup.
	decodeInput []vp8Frame

	// frames marks when each complete frame arrived and whether it decodes.
	frames []frameMark
}

func newRecorder() *recorder {
	return &recorder{
		start: time.Now(),
		sentFrames: map[string]map[string]struct{}{
			kindAudio: {},
			kindVideo: {},
		},
	}
}

func (r *recorder) since(t time.Time) time.Duration {
	return t.Sub(r.start)
}

func (r *recorder) offerAnswerExchange() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offerAnswerExchanges++
}

func (r *recorder) markNegotiated() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.negotiated = true
}

func (r *recorder) hangup() {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hungUp = true
	r.hungUpAt = r.since(now)
}

func (r *recorder) connectionState(state webrtc.PeerConnectionState) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}
	at := r.since(now)
	r.connectionStates = append(r.connectionStates, StateChange{At: at, State: state.String()})
	if state == webrtc.PeerConnectionStateConnected && !r.connected {
		r.connected = true
		r.connectedAt = at
	}
}

func (r *recorder) iceConnectionState(state webrtc.ICEConnectionState) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}
	r.iceStates = append(r.iceStates, StateChange{At: r.since(now), State: state.String()})

	switch state {
	case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
		r.iceConnected = true
	case webrtc.ICEConnectionStateChecking, webrtc.ICEConnectionStateNew:
		// ICE only goes back to checking after it has connected when it
		// restarts.
		if r.iceConnected {
			r.iceRestarts++
			r.iceConnected = false
		}
	default:
	}
}

func (r *recorder) signalingState(state webrtc.SignalingState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp || !r.negotiated {
		return
	}
	// After the initial exchange, leaving "stable" means a new offer/answer
	// round, i.e. a renegotiation.
	if state != webrtc.SignalingStateStable {
		r.renegotiations++
	}
}

func (r *recorder) negotiationNeeded() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp || !r.negotiated {
		return
	}
	r.renegotiations++
}

func (r *recorder) iceRestartObserved() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.iceRestarts == 0 {
		r.iceRestarts = 1
	}
}

// srtpError classifies an SRTP or SRTCP packet the caller could not decrypt.
func (r *recorder) srtpError(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}
	r.decryptFailureTimes = append(r.decryptFailureTimes, time.Since(r.start))
	switch {
	case strings.Contains(msg, "failed to verify auth tag"):
		r.decryptionFailures.AuthTag++
	case strings.Contains(msg, "duplicated"):
		r.decryptionFailures.Replay++
	default:
		r.decryptionFailures.Other++
	}
}

// sending registers a frame the caller is about to send (an Opus packet or a
// VP8 frame), so its echo is matched even when it arrives before the write
// returns. The first video frame's send time is taken here for the same
// reason: the echo's decode time is measured against it.
func (r *recorder) sending(kind string, frame []byte) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sentFrames[kind][string(frame)] = struct{}{}
	if kind == kindVideo && r.sentVideo.Frames == 0 {
		r.firstVideoSentAt = r.since(now)
	}
}

// sent counts a frame the caller has written.
func (r *recorder) sent(kind string, keyframe bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	track := &r.sentAudio
	if kind == kindVideo {
		track = &r.sentVideo
	}
	track.Frames++
	if keyframe {
		track.Keyframes++
	}
}

// keyframeRequestReceived records a PLI or FIR for one of the caller's own
// tracks.
func (r *recorder) keyframeRequestReceived(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}
	if kind == kindVideo {
		r.sentVideo.KeyframeRequests++
	} else {
		r.sentAudio.KeyframeRequests++
	}
}

func (r *recorder) keyframeRequestSent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keyframeRequests++
}

func (r *recorder) addTrack(kind string, ssrc uint32, payloadType uint8) *trackRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	track := &trackRecord{kind: kind, ssrc: ssrc, payloadType: payloadType}
	if kind == kindVideo {
		track.video = &videoRecord{}
	}
	r.tracks = append(r.tracks, track)

	return track
}

// packet records a packet the caller decrypted on a track.
func (r *recorder) packet(track *trackRecord, pkt *rtp.Packet, arrived time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}

	at := r.since(arrived)
	if n := len(track.arrivals); n > 0 {
		if gap := at - track.lastArrival; gap > track.gap {
			track.gap = gap
			track.gapEndedAt = at
		}
		step := pkt.SequenceNumber - track.lastSeq
		if step != 1 {
			track.seqDiscontinuities++
		}
		if step == 0 {
			track.duplicatePackets++
		}
		if step > 1<<15 {
			track.outOfOrderPackets++
		}
	} else {
		track.firstArrival = at
	}
	track.arrivals = append(track.arrivals, at)
	track.lastArrival = at
	track.lastSeq = pkt.SequenceNumber
	track.headers = append(track.headers, rtpMark{seq: pkt.SequenceNumber, timestamp: pkt.Timestamp})

	// Opus packets are whole frames; VP8 frames are matched once reassembled.
	if track.video == nil {
		if _, ok := r.sentFrames[track.kind][string(pkt.Payload)]; !ok {
			track.unmatchedPayloads++
		}
	}
}

// videoIncomplete records frames that lost packets on a video track.
func (r *recorder) videoIncomplete(track *trackRecord, frames int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}
	track.video.IncompleteFrames += frames
}

// videoFrame records a complete frame on a video track. For a keyframe,
// size and decodeErr are the result of decoding it in Go; decoded is when
// that finished.
func (r *recorder) videoFrame(track *trackRecord, frame *vp8Frame, size image.Point, decodeErr error, decoded time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return
	}
	v := track.video
	decodable := v.DecodableFrames
	defer func() {
		v.frames = append(v.frames, frameMark{at: r.since(decoded), decodable: v.DecodableFrames > decodable})
	}()

	v.Frames++
	if frame.keyframe {
		v.Keyframes++
	}
	if _, ok := r.sentFrames[kindVideo][string(frame.data)]; !ok {
		v.UnmatchedFrames++
	}

	// A frame is in order when its first packet directly follows the previous
	// complete frame's last packet: no frame was lost or reordered between.
	inOrder := !v.haveLast || (frame.firstSeq == v.lastSeq+1 && int32(frame.timestamp-v.lastTS) > 0)
	if !inOrder {
		v.FrameGaps++
	}
	v.haveLast = true
	v.lastSeq = frame.lastSeq
	v.lastTS = frame.timestamp

	switch {
	case frame.keyframe && decodeErr == nil:
		v.KeyframesDecoded++
		v.DecodableFrames++
		v.chain = true
		if v.FirstDecodedFrameAt == 0 {
			v.FirstDecodedFrameAt = r.since(decoded)
			v.Width, v.Height = size.X, size.Y
		}
	case frame.keyframe:
		v.KeyframeDecodeErrors++
		v.LastDecodeError = decodeErr.Error()
		v.chain = false
	case v.chain && inOrder:
		v.DecodableFrames++
	case v.KeyframesDecoded == 0:
		v.FramesBeforeFirstKeyframe++
	default:
		v.UndecodableFrames++
		v.chain = false
	}

	if v.KeyframesDecoded > 0 {
		v.decodeInput = append(v.decodeInput, *frame)
	}
}

// decodeInput returns the frames of the i-th received track for the full
// decode, and the size of its first decoded keyframe.
func (r *recorder) decodeInput(i int) ([]vp8Frame, image.Point) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.tracks[i].video
	if v == nil {
		return nil, image.Point{}
	}

	return v.decodeInput, image.Point{X: v.Width, Y: v.Height}
}

// report returns the measurements. Call it after hangup.
func (r *recorder) report() *Report {
	r.mu.Lock()
	defer r.mu.Unlock()

	rep := &Report{
		OfferAnswerExchanges: r.offerAnswerExchanges,
		ConnectedAt:          r.connectedAt,
		HungUpAt:             r.hungUpAt,
		ConnectionStates:     append([]StateChange(nil), r.connectionStates...),
		ICEConnectionStates:  append([]StateChange(nil), r.iceStates...),
		DecryptionFailures:   r.decryptionFailures,
		Renegotiations:       r.renegotiations,
		ICERestarts:          r.iceRestarts,
		SentAudio:            r.sentAudio,
		SentVideo:            r.sentVideo,
		FirstVideoSentAt:     r.firstVideoSentAt,
		KeyframeRequestsSent: r.keyframeRequests,
	}
	for _, track := range r.tracks {
		report := TrackReport{
			Kind:                    track.kind,
			SSRC:                    track.ssrc,
			PayloadType:             track.payloadType,
			Packets:                 len(track.arrivals),
			Arrivals:                append([]time.Duration(nil), track.arrivals...),
			FirstArrival:            track.firstArrival,
			LastArrival:             track.lastArrival,
			MediaGap:                track.gap,
			MediaGapEndedAt:         track.gapEndedAt,
			SequenceDiscontinuities: track.seqDiscontinuities,
			UnmatchedPayloads:       track.unmatchedPayloads,
			DuplicatePackets:        track.duplicatePackets,
			OutOfOrderPackets:       track.outOfOrderPackets,
		}
		if track.video != nil {
			video := track.video.VideoReport
			report.Video = &video
		}
		rep.Tracks = append(rep.Tracks, report)
	}
	rep.Moves = r.moveReports()
	rep.Consent = r.consentReport()

	return rep
}
