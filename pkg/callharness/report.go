package callharness

import (
	"fmt"
	"strings"
	"time"

	"github.com/pion/sdp/v3"
)

// Report is what the caller observed during one call. Times are offsets from
// the moment the caller started dialing.
type Report struct {
	// OfferAnswerExchanges counts the HTTP offer/answer exchanges the caller
	// made to start the call.
	OfferAnswerExchanges int

	// Answer describes the SDP answer the caller received.
	Answer AnswerFacts

	// ConnectedAt is when the caller's connection state first reached
	// "connected"; zero if it never did.
	ConnectedAt time.Duration
	// HungUpAt is when the caller hung up.
	HungUpAt time.Duration

	// ConnectionStates and ICEConnectionStates are the caller's
	// PeerConnection and ICE connection state changes up to hangup.
	ConnectionStates    []StateChange
	ICEConnectionStates []StateChange

	// DecryptionFailures counts SRTP and SRTCP packets the caller received
	// but could not decrypt.
	DecryptionFailures DecryptionFailures

	// Renegotiations counts offer/answer rounds (or negotiationneeded events)
	// after the initial exchange. ICERestarts counts ICE restarts: ICE going
	// back to checking after it connected, or changed ICE credentials.
	Renegotiations int
	ICERestarts    int

	// SentAudio is the caller's own audio track.
	SentAudio SentTrack

	// Tracks are the tracks the caller received, in arrival order.
	Tracks []TrackReport
}

// StateChange is one connection state transition.
type StateChange struct {
	At    time.Duration
	State string
}

// DecryptionFailures classifies packets the caller could not decrypt.
type DecryptionFailures struct {
	AuthTag int // authentication tag did not verify (wrong key, index or rollover counter)
	Replay  int // rejected by the replay window
	Other   int
}

// Total is the number of packets the caller could not decrypt.
func (d DecryptionFailures) Total() int {
	return d.AuthTag + d.Replay + d.Other
}

// SentTrack describes a track the caller sent.
type SentTrack struct {
	SSRC    uint32
	Packets int
}

// TrackReport describes a track the caller received.
type TrackReport struct {
	Kind        string
	SSRC        uint32
	PayloadType uint8

	// Packets counts packets the caller decrypted on this track, and
	// Arrivals holds the arrival time of each.
	Packets      int
	Arrivals     []time.Duration
	FirstArrival time.Duration
	LastArrival  time.Duration

	// MediaGap is the longest interval between consecutive successfully
	// decrypted packets on the track; MediaGapEndedAt is when it ended.
	MediaGap        time.Duration
	MediaGapEndedAt time.Duration

	// SequenceDiscontinuities counts packets whose sequence number does not
	// follow the previous packet's.
	SequenceDiscontinuities int
	// UnmatchedPayloads counts packets whose payload is not one the caller
	// sent, i.e. packets that are not an echo of the caller's media.
	UnmatchedPayloads int
}

// AnswerFacts is what the caller can read from the SDP answer.
type AnswerFacts struct {
	SDP        string
	ICELite    bool     // a=ice-lite at session level
	Bundle     []string // MIDs in a=group:BUNDLE
	RTCPMux    bool     // every accepted m-line has a=rtcp-mux
	Candidates []string // every a=candidate value
}

func parseAnswer(raw string) (AnswerFacts, error) {
	facts := AnswerFacts{SDP: raw}

	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(raw); err != nil {
		return facts, err
	}

	_, facts.ICELite = desc.Attribute(sdp.AttrKeyICELite)
	if group, ok := desc.Attribute(sdp.AttrKeyGroup); ok {
		if fields := strings.Fields(group); len(fields) > 0 && fields[0] == "BUNDLE" {
			facts.Bundle = fields[1:]
		}
	}

	accepted := 0
	muxed := 0
	for _, md := range desc.MediaDescriptions {
		if md.MediaName.Port.Value == 0 {
			continue
		}
		accepted++
		if _, ok := md.Attribute(sdp.AttrKeyRTCPMux); ok {
			muxed++
		}
		for _, attr := range md.Attributes {
			if attr.Key == sdp.AttrKeyCandidate {
				facts.Candidates = append(facts.Candidates, attr.Value)
			}
		}
	}
	facts.RTCPMux = accepted > 0 && muxed == accepted

	return facts, nil
}

// HostCandidates returns the answer's host candidates.
func (a AnswerFacts) HostCandidates() []string {
	var hosts []string
	for _, c := range a.Candidates {
		if strings.Contains(c, " typ host") {
			hosts = append(hosts, c)
		}
	}

	return hosts
}

// ConnectedThroughout reports whether the caller's connection reached
// "connected" and stayed there until hangup.
func (r *Report) ConnectedThroughout() bool {
	for i, change := range r.ConnectionStates {
		if change.State == "connected" {
			return i == len(r.ConnectionStates)-1
		}
	}

	return false
}

// Track returns the first received track of a kind ("audio" or "video"), or
// nil.
func (r *Report) Track(kind string) *TrackReport {
	for i := range r.Tracks {
		if r.Tracks[i].Kind == kind {
			return &r.Tracks[i]
		}
	}

	return nil
}

// Summary formats the report for a test log.
func (r *Report) Summary() string {
	var b strings.Builder

	fmt.Fprintf(&b, "call harness report\n")
	fmt.Fprintf(&b, "  signaling:        %d offer/answer exchange(s); answer ice-lite=%t bundle=%v rtcp-mux=%t host candidates=%d\n",
		r.OfferAnswerExchanges, r.Answer.ICELite, r.Answer.Bundle, r.Answer.RTCPMux, len(r.Answer.HostCandidates()))
	fmt.Fprintf(&b, "  connected at:     %s; call length %s (until hangup at %s)\n",
		ms(r.ConnectedAt), ms(r.HungUpAt-r.ConnectedAt), ms(r.HungUpAt))
	fmt.Fprintf(&b, "  connection:       %s (stayed connected: %t)\n", formatStates(r.ConnectionStates), r.ConnectedThroughout())
	fmt.Fprintf(&b, "  ICE connection:   %s\n", formatStates(r.ICEConnectionStates))
	fmt.Fprintf(&b, "  decrypt failures: %d (auth tag %d, replay %d, other %d)\n",
		r.DecryptionFailures.Total(), r.DecryptionFailures.AuthTag, r.DecryptionFailures.Replay, r.DecryptionFailures.Other)
	fmt.Fprintf(&b, "  renegotiations:   %d; ICE restarts: %d\n", r.Renegotiations, r.ICERestarts)
	fmt.Fprintf(&b, "  sent audio:       ssrc=%d packets=%d\n", r.SentAudio.SSRC, r.SentAudio.Packets)
	if len(r.Tracks) == 0 {
		fmt.Fprintf(&b, "  received:         no tracks\n")
	}
	for _, t := range r.Tracks {
		fmt.Fprintf(&b, "  received %s:   ssrc=%d pt=%d packets=%d first=%s last=%s media gap=%s (ended at %s) seq discontinuities=%d unmatched payloads=%d\n",
			t.Kind, t.SSRC, t.PayloadType, t.Packets, ms(t.FirstArrival), ms(t.LastArrival),
			ms(t.MediaGap), ms(t.MediaGapEndedAt), t.SequenceDiscontinuities, t.UnmatchedPayloads)
	}

	return b.String()
}

func formatStates(changes []StateChange) string {
	if len(changes) == 0 {
		return "none"
	}
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = c.State + "@" + ms(c.At)
	}

	return strings.Join(parts, " -> ")
}

func ms(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}
