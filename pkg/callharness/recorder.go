package callharness

import (
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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

	sentPackets  int
	sentPayloads map[string]struct{}

	tracks []*trackRecord
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
	unmatchedPayloads  int
}

func newRecorder() *recorder {
	return &recorder{
		start:        time.Now(),
		sentPayloads: make(map[string]struct{}),
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
	switch {
	case strings.Contains(msg, "failed to verify auth tag"):
		r.decryptionFailures.AuthTag++
	case strings.Contains(msg, "duplicated"):
		r.decryptionFailures.Replay++
	default:
		r.decryptionFailures.Other++
	}
}

func (r *recorder) sent(payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sentPackets++
	r.sentPayloads[string(payload)] = struct{}{}
}

func (r *recorder) addTrack(kind string, ssrc uint32, payloadType uint8) *trackRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	track := &trackRecord{kind: kind, ssrc: ssrc, payloadType: payloadType}
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
		if pkt.SequenceNumber != track.lastSeq+1 {
			track.seqDiscontinuities++
		}
	} else {
		track.firstArrival = at
	}
	track.arrivals = append(track.arrivals, at)
	track.lastArrival = at
	track.lastSeq = pkt.SequenceNumber

	if _, ok := r.sentPayloads[string(pkt.Payload)]; !ok {
		track.unmatchedPayloads++
	}
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
		SentAudio:            SentTrack{Packets: r.sentPackets},
	}
	for _, track := range r.tracks {
		rep.Tracks = append(rep.Tracks, TrackReport{
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
		})
	}

	return rep
}
