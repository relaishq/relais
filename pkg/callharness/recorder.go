package callharness

import (
	"image"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
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
	distributed                *DistributedOptions
	armed                      map[OperationIdentity]time.Time
	audioSends, videoSends     []evidenceRef
	audioReturns, videoReturns []evidenceRef
	prepared                   *measurementIndex
	measurementMu              sync.Mutex
	measurementLockTime        time.Duration

	recording              RecordingOptions
	lastCompact            time.Duration
	historyStart           time.Duration
	savedMoves             []MoveReport
	baselineFloor          time.Duration
	firstAudio, firstVideo FreshnessReport
	consentPrefix          ConsentReport
	consentLastResponse    time.Duration
	consentBaseResponses   uint64
	freshnessPolicy        FreshnessPolicy
	start                  time.Time

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
	crypto               *cryptoObserver
	decryptFailureTimes  []time.Duration

	// Recent caller sends, by kind. Identity maps expire with the rolling
	// history; event summaries retain their finite observation horizon.
	sentAudio             SentTrack
	sentVideo             SentTrack
	sentFrames            map[string]map[string]struct{}
	firstVideoSentAt      time.Duration
	sentVideoFrames       map[uint16]sentVideoFrame
	sentAudioUnits        map[string]contentUnit
	audioIdentity         uint64
	audioSeeded           bool
	pendingAudio          string
	contentAssemblies     map[videoAssemblyKey]*contentAssembly
	contentIdentityErrors int
	repeatedAudioReturns  []contentUnit
	repeatedVideoReturns  []contentUnit
	frameInterval         time.Duration
	keyframeInterval      time.Duration
	keyframeRequests      int // PLI/FIR the caller sent for the echoed video

	tracks []*trackRecord

	// Handovers and consent checks; see handover.go.
	moves   []moveRecord
	consent []consentSample
}

type trackRecord struct {
	kind        string
	ssrc        uint32
	payloadType uint8
	seenPackets map[uint16]uint32

	packets            int
	arrivals           []time.Duration
	firstArrival       time.Duration
	lastArrival        time.Duration
	gap                time.Duration // longest interval between consecutive packets
	gapEndedAt         time.Duration
	mediaGaps          []MediaGap
	mediaGapOverflow   int
	lastSeq            uint16
	seqDiscontinuities int
	duplicatePackets   int
	outOfOrderPackets  int
	unmatchedPayloads  int
	unmatchedTimes     []time.Duration
	headers            []rtpMark // sequence number and timestamp of each arrival

	video *videoRecord // video tracks only
}

// videoRecord follows the decodability of a received video track; see
// video.go for the method.
type videoRecord struct {
	VideoReport

	chain           bool // every frame since the last decoded keyframe is decodable
	haveLast        bool
	lastSeq         uint16 // last packet of the previous complete frame
	lastTS          uint32
	lastPictureID   uint16
	lastSourceAt    time.Duration
	lastIdentity    uint64
	decodedPictures map[uint16]sentVideoFrame // recent decoded identities, not just complete receipts
	decodeOverflow  bool
	sampleBurst     int
	lastSampleEvent time.Duration
	sampleResults   []decodeSample

	// decodeInput is bounded short-call input for the decode at hangup.
	// Sampled online mode does not retain whole-call decode input.
	decodeInput []vp8Frame
	decodeBytes int

	// frames marks when each complete frame arrived and whether it decodes.
	frames []frameMark
}

func newRecorder() *recorder {
	return &recorder{
		start:             time.Now(),
		recording:         RecordingOptions{}.defaults(),
		sentVideoFrames:   make(map[uint16]sentVideoFrame),
		sentAudioUnits:    make(map[string]contentUnit),
		contentAssemblies: make(map[videoAssemblyKey]*contentAssembly),
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
	at := time.Since(r.start)
	r.decryptFailureTimes = append(r.decryptFailureTimes, at)
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
	if kind == kindVideo {
		id := uint16(track.Frames & 0x7fff)
		source := r.sentVideoFrames[id]
		source.written = true
		r.sentVideoFrames[id] = source
	} else {
		if unit, ok := r.sentAudioUnits[r.pendingAudio]; ok && r.pendingAudio != "" {
			unit.written = true
			r.sentAudioUnits[r.pendingAudio] = unit
		}
		r.pendingAudio = ""
	}
	track.Frames++
	if keyframe {
		track.Keyframes++
	}
}

// keyframeResponseRequest is nil when there is no accepted request. At=0
// remains a valid caller-clock request time.
type keyframeResponseRequest struct{ at time.Duration }

// keyframeRequestReceived records a PLI/FIR, unless the call has hung up.
func (r *recorder) keyframeRequestReceived(kind string) *keyframeResponseRequest {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return nil
	}
	if kind == kindVideo {
		r.sentVideo.KeyframeRequests++
	} else {
		r.sentAudio.KeyframeRequests++
	}
	return &keyframeResponseRequest{at: r.since(now)}
}

func (r *recorder) keyframeRequestSent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keyframeRequests++
}

func (r *recorder) addTrack(kind string, ssrc uint32, payloadType uint8) *trackRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	track := &trackRecord{kind: kind, ssrc: ssrc, payloadType: payloadType, seenPackets: make(map[uint16]uint32)}
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
	if track.seenPackets == nil {
		track.seenPackets = make(map[uint16]uint32)
	}
	prior, seen := track.seenPackets[pkt.SequenceNumber]
	duplicate := seen && prior == pkt.Timestamp
	if track.packets > 0 {
		gap := at - track.lastArrival
		if gap > 150*time.Millisecond {
			if len(track.mediaGaps) < MaxRetainedMediaGaps {
				track.mediaGaps = append(track.mediaGaps, MediaGap{EndedAt: at, Duration: gap})
			} else {
				track.mediaGapOverflow++
			}
		}
		if gap > track.gap {
			track.gap = gap
			track.gapEndedAt = at
		}
		step := pkt.SequenceNumber - track.lastSeq
		if step != 1 {
			track.seqDiscontinuities++
		}

		if step > 1<<15 && !duplicate {
			track.outOfOrderPackets++
		}
	} else {
		track.firstArrival = at
	}
	if duplicate {
		track.duplicatePackets++
	}
	track.seenPackets[pkt.SequenceNumber] = pkt.Timestamp
	track.packets++
	if len(track.arrivals) >= maxHistoryPackets {
		remove := maxHistoryPackets / 4
		r.historyStart = max(r.historyStart, track.arrivals[remove])
		track.arrivals = append(track.arrivals[:0], track.arrivals[remove:]...)
		track.headers = append(track.headers[:0], track.headers[remove:]...)
	}
	track.arrivals = append(track.arrivals, at)
	track.lastArrival = at
	track.lastSeq = pkt.SequenceNumber
	mark := rtpMark{seq: pkt.SequenceNumber, timestamp: pkt.Timestamp}
	if track.video != nil {
		var desc codecs.VP8Packet
		if _, err := desc.Unmarshal(pkt.Payload); err == nil {
			mark.pictureID, mark.havePictureID = desc.PictureID, true
		}
	}
	track.headers = append(track.headers, mark)

	if track.video != nil {
		r.returnedVideo(track, pkt, at)
	} else if unit, ok := r.sentAudioUnits[string(pkt.Payload)]; ok {
		if unit.returnedAt == 0 {
			unit.returnedAt = at
			r.sentAudioUnits[string(pkt.Payload)] = unit
			r.audioReturns = append(r.audioReturns, evidenceRef{at: at, audio: string(pkt.Payload)})
		} else {
			unit.returnedAt, unit.identity, unit.replayed = at, string(pkt.Payload), true
			r.repeatedAudioReturns = r.appendRepeat(r.repeatedAudioReturns, unit)
		}
	}

	// Opus packets are whole frames; VP8 frames are matched once reassembled.
	if track.video == nil {
		if _, ok := r.sentFrames[track.kind][string(pkt.Payload)]; !ok {
			track.unmatchedPayloads++
			r.recordUnmatched(track, at)
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
func (r *recorder) videoFrame(track *trackRecord, frame *vp8Frame, size image.Point, decodeErr error, decoded time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hungUp {
		return false
	}
	v := track.video
	if r.recordDecodedRepeat(track, frame, decoded) {
		return false
	}
	decodable := v.DecodableFrames
	restartsDecodeChain := false
	source := r.sentVideoFrames[frame.pictureID]
	if source.data != string(frame.data) {
		if source.at > 0 && source.written && !previousVP8Epoch(frame.data, source, frame.pictureID) {
			r.contentIdentityErrors++
		}
		source = sentVideoFrame{}
	}
	frame.sourceAt = source.at
	defer func() {
		receivedAt := time.Duration(0)
		if !frame.completedAt.IsZero() {
			receivedAt = r.since(frame.completedAt)
		}
		v.frames = append(v.frames, frameMark{at: r.since(decoded), decodable: v.DecodableFrames > decodable, firstArrival: r.since(frame.firstArrival), receivedAt: receivedAt, source: source, pictureID: frame.pictureID, restartsDecodeChain: restartsDecodeChain})
		// The echo can decode before the source write returns. Retain its
		// known identity now; repeat filtering still requires a confirmed
		// write in the current send map.
		if v.DecodableFrames > decodable && source.at > 0 {
			if v.decodedPictures == nil {
				v.decodedPictures = make(map[uint16]sentVideoFrame)
			}
			v.decodedPictures[frame.pictureID] = source
		}
	}()

	v.Frames++
	if frame.keyframe {
		v.Keyframes++
	}
	if _, ok := r.sentFrames[kindVideo][string(frame.data)]; !ok {
		v.UnmatchedFrames++
		at := r.since(decoded)
		if !frame.completedAt.IsZero() {
			at = r.since(frame.completedAt)
		}
		r.recordUnmatched(track, at)
	}

	// Authenticated caller identities follow the source PictureID chain even
	// when a relay changes output sequence numbers. A missing source reference
	// still breaks the decoder chain. Unknown sources retain strict RTP order.
	if v.haveLast && int32(frame.timestamp-v.lastTS) <= 0 {
		v.NonMonotonicTimestamps++
	}
	inOrder := !v.haveLast || (frame.firstSeq == v.lastSeq+1 && int32(frame.timestamp-v.lastTS) > 0)
	if v.haveLast && v.lastSourceAt > 0 && source.at > 0 {
		inOrder = (source.identity == v.lastIdentity+1 || (source.identity == 0 && frame.pictureID == (v.lastPictureID+1)&0x7fff)) && source.at > v.lastSourceAt && int32(frame.timestamp-v.lastTS) > 0
	}
	if !inOrder {
		v.FrameGaps++
	}
	v.haveLast = true
	v.lastSeq = frame.lastSeq
	v.lastTS = frame.timestamp
	v.lastPictureID, v.lastSourceAt = frame.pictureID, source.at
	v.lastIdentity = source.identity

	restartsDecodeChain = frame.keyframe && decodeErr == nil && (!v.chain || !inOrder)
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

	if v.KeyframesDecoded > 0 && r.recording.DecodeEvery == 0 {
		if len(v.decodeInput) < maxOfflineFrames && v.decodeBytes+len(frame.data) <= maxOfflineBytes {
			v.decodeBytes += len(frame.data)
			v.decodeInput = append(v.decodeInput, *frame)
		} else {
			v.decodeOverflow = true
		}
	}
	return true
}

// decodeVideoFrame rejects already decoded pictures before either decoder is
// called. The live reader then records a new frame and optionally samples it.
func (r *recorder) decodeVideoFrame(track *trackRecord, frame *vp8Frame, at time.Time) (image.Point, error, bool) {
	r.mu.Lock()
	repeated := r.hungUp || r.recordDecodedRepeat(track, frame, at)
	r.mu.Unlock()
	if repeated || !frame.keyframe {
		return image.Point{}, nil, repeated
	}
	size, err := decodeKeyframe(frame.data)
	return size, err, false
}

// recordDecodedRepeat runs under r.mu. Cache provenance remains known from
// the earlier decode, but a repeat cannot advance the decoder, satisfy decode
// sampling, or supply first new content. Only exact retained send identities
// qualify; unknown epochs and conflicting payloads still follow the old path.
func (r *recorder) recordDecodedRepeat(track *trackRecord, frame *vp8Frame, at time.Time) bool {
	source := r.sentVideoFrames[frame.pictureID]
	previous, ok := track.video.decodedPictures[frame.pictureID]
	if !ok || source.at <= 0 || !source.written || source.data != string(frame.data) || previous.identity != source.identity || previous.at != source.at || previous.data != source.data {
		return false
	}
	v := track.video
	v.Frames++
	v.RepeatedFrames++
	if frame.keyframe {
		v.Keyframes++
	}
	receivedAt := r.since(at)
	if !frame.completedAt.IsZero() {
		receivedAt = r.since(frame.completedAt)
	}
	if len(v.frames) > 0 && receivedAt-v.frames[len(v.frames)-1].receivedAt > max(50*time.Millisecond, 3*r.frameInterval) {
		v.sampleBurst = max(v.sampleBurst, minimumDecodedSamples)
	}
	v.frames = append(v.frames, frameMark{at: r.since(at), decodable: true, repeated: true, firstArrival: r.since(frame.firstArrival), receivedAt: receivedAt, source: source, pictureID: frame.pictureID})
	return true
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

	rep := &Report{
		StartedAt:            r.start,
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
	if r.crypto != nil {
		rep.SRTPDiagnostics = r.crypto.report()
	}
	for _, track := range r.tracks {
		report := TrackReport{
			Kind:                    track.kind,
			SSRC:                    track.ssrc,
			PayloadType:             track.payloadType,
			Packets:                 track.packets,
			Arrivals:                append([]time.Duration(nil), track.arrivals...),
			FirstArrival:            track.firstArrival,
			LastArrival:             track.lastArrival,
			MediaGap:                track.gap,
			MediaGapEndedAt:         track.gapEndedAt,
			MediaGaps:               append([]MediaGap(nil), track.mediaGaps...),
			MediaGapsRecorded:       true,
			MediaGapOverflow:        track.mediaGapOverflow,
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
	rep.Recording = r.memoryGauge()
	rep.Consent = r.consentReport()
	view := r
	if r.distributed != nil {
		view = r.measurementSnapshot(r.hungUpAt, len(r.moves))
	} else {
		rep.Moves = r.moveReports()
	}
	r.mu.Unlock()
	if view != r {
		view.prepareMeasurementIndex()
		rep.Moves = view.moveReports()
	}

	if r.distributed != nil {
		d := r.distributed
		rep.Distributed = &DistributedRecording{ObservedRate: d.Clock.ObservedRate(), DriftPPM: d.Clock.DriftPPM(), RunID: d.RunID, ControlOffset: d.ControlOffset, WorkerOffset: d.WorkerOffset, Probes: d.Clock.Probes()}
		rep.Distributed.Outcome, rep.Distributed.Causes = JudgeDistributedTrials(rep.Moves)
		for id := range view.armed {
			rep.Distributed.Armed = append(rep.Distributed.Armed, id)
		}
		slices.SortFunc(rep.Distributed.Armed, func(a, b OperationIdentity) int {
			if a.OperationID < b.OperationID {
				return -1
			}
			if a.OperationID > b.OperationID {
				return 1
			}
			if a.Worker < b.Worker {
				return -1
			}
			if a.Worker > b.Worker {
				return 1
			}
			if a.Epoch < b.Epoch {
				return -1
			}
			if a.Epoch > b.Epoch {
				return 1
			}
			return 0
		})
		for _, id := range rep.Distributed.Armed {
			found := false
			for _, m := range rep.Moves {
				if m.Identity != nil && *m.Identity == id {
					found = true
					break
				}
			}
			if !found {
				rep.Distributed.Missing = append(rep.Distributed.Missing, id)
				if rep.Distributed.Outcome != "fail" {
					rep.Distributed.Outcome = "inconclusive"
				}
				rep.Distributed.Causes = append(rep.Distributed.Causes, "armed event receipt missing: "+id.OperationID)
			}
		}
		for _, m := range rep.Moves {
			if m.Measurement.Failed {
				rep.Distributed.Outcome = "fail"
				rep.Distributed.Causes = append(rep.Distributed.Causes, "known caller content failure")
			}
			if m.Identity == nil || m.Measurement.Inconclusive {
				if rep.Distributed.Outcome != "fail" {
					rep.Distributed.Outcome = "inconclusive"
				}
				rep.Distributed.Causes = append(rep.Distributed.Causes, "content classifications not proven across full clock interval")
			}
		}
		if err := d.Clock.Err(); err != nil {
			rep.Distributed.ClockError = err.Error()
			if rep.Distributed.Outcome != "fail" {
				rep.Distributed.Outcome = "inconclusive"
			}
		}
	}

	return rep
}
