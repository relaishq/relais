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
	Recording RecordingReport
	// StartedAt aligns packet observations across calls sharing a worker.
	StartedAt time.Time
	// OfferAnswerExchanges counts the HTTP offer/answer exchanges the caller
	// made to start the call.
	OfferAnswerExchanges int

	// Offer is the caller's SDP offer, and Answer describes the SDP answer
	// the caller received.
	Offer  string
	Answer AnswerFacts

	// RemoteAddr is the remote end ("ip:port") of the caller's selected ICE
	// candidate pair at hangup: the one address the caller sent to and
	// accepted media from.
	RemoteAddr string

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

	// SentAudio and SentVideo are the caller's own tracks. SentVideo is the
	// zero value for a call without video. FirstVideoSentAt is when the
	// caller sent its first video frame.
	SentAudio        SentTrack
	SentVideo        SentTrack
	FirstVideoSentAt time.Duration

	// KeyframeRequestsSent counts the PLI and FIR packets the caller sent for
	// the echoed video (see Call.RequestKeyframe).
	KeyframeRequestsSent int

	// Tracks are the tracks the caller received, in arrival order.
	Tracks []TrackReport

	// Moves are planned handovers and automatic crash takeovers during the call,
	// as the caller observed them. Consent is what the caller observed of
	// its ICE consent checks. See handover.go.
	Moves   []MoveReport
	Consent ConsentReport
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
	SSRC uint32
	// Frames counts the frames sent: Opus packets for audio (one frame per
	// packet), VP8 frames for video. Keyframes counts VP8 keyframes.
	Frames    int
	Keyframes int
	// KeyframeRequests counts the PLI and FIR packets the caller received
	// for this track's SSRC.
	KeyframeRequests int
}

// TrackReport describes a track the caller received.
type TrackReport struct {
	Kind        string
	SSRC        uint32
	PayloadType uint8

	// Packets counts all packets the caller decrypted on this track.
	// Arrivals holds the rolling history named by Report.Recording.
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

	// DuplicatePackets and OutOfOrderPackets distinguish stale sender traffic
	// from forward sequence gaps caused by lost packets.
	DuplicatePackets  int
	OutOfOrderPackets int
	// UnmatchedPayloads counts audio packets whose payload is not one the
	// caller sent, i.e. packets that are not an echo of the caller's media.
	// Video is matched frame by frame instead (VideoReport.UnmatchedFrames).
	UnmatchedPayloads int

	// Video describes the frames of a video track; nil for audio.
	Video *VideoReport
}

// VideoReport is what the caller could do with a received VP8 track: the
// frames it reassembled and whether they decode. See video.go for the
// method.
type VideoReport struct {
	OnlineDecode OnlineDecodeReport
	// Frames counts complete frames (every packet, in order, ending with the
	// marker bit). IncompleteFrames counts frames that lost packets.
	Frames           int
	Keyframes        int
	IncompleteFrames int
	// FrameGaps counts complete frames that did not directly follow the
	// previous complete frame: frames were lost or reordered between them.
	FrameGaps int
	// NonMonotonicTimestamps counts complete frames whose RTP timestamp did
	// not advance from the previous complete frame (modulo the RTP clock).
	NonMonotonicTimestamps int
	// UnmatchedFrames counts frames that are not byte for byte a frame the
	// caller sent.
	UnmatchedFrames int

	// KeyframesDecoded counts keyframes decoded in pure Go;
	// KeyframeDecodeErrors counts keyframes that failed, the last with
	// LastDecodeError.
	KeyframesDecoded     int
	KeyframeDecodeErrors int
	LastDecodeError      string

	// DecodableFrames counts frames that decode: decoded keyframes, and
	// interframes in an unbroken run of complete frames since one.
	// FramesBeforeFirstKeyframe counts frames that arrived before any
	// keyframe decoded (a receiver waits for a keyframe). UndecodableFrames
	// counts later frames whose references were lost.
	DecodableFrames           int
	FramesBeforeFirstKeyframe int
	UndecodableFrames         int

	// FirstDecodedFrameAt is when the first frame decoded; zero if none did.
	// Width and Height are that keyframe's size.
	FirstDecodedFrameAt time.Duration
	Width, Height       int

	// FullDecode is ffmpeg's complete short-call check. Long calls select
	// OnlineDecode; offline history is capped and a truncated check is skipped.
	FullDecode FullDecode
}

// FullDecode is the result of decoding the received video with ffmpeg.
type FullDecode struct {
	// Skipped says why the decode did not run (for example, ffmpeg is not
	// on PATH); empty when it ran.
	Skipped string

	Decoder       string // the ffmpeg binary
	FramesIn      int    // frames given to the decoder
	FramesDecoded int    // frames the decoder output
	Errors        string // the decoder's error output; empty when clean
}

// Ran reports whether the full decode ran.
func (f FullDecode) Ran() bool {
	return f.Skipped == ""
}

// AnswerFacts is what the caller can read from the SDP answer.
type AnswerFacts struct {
	SDP        string
	ICELite    bool     // a=ice-lite at session level
	Bundle     []string // MIDs in a=group:BUNDLE
	RTCPMux    bool     // every accepted m-line has a=rtcp-mux
	Candidates []string // every a=candidate value

	// Media are the answer's m-lines in order.
	Media []AnswerMedia
}

// AnswerMedia is one m-line of the answer.
type AnswerMedia struct {
	Kind string // "audio", "video", "application", ...
	MID  string
	Port int // 0 when the answer rejects the m-line

	// Formats are the payload types; Codecs are their a=rtpmap values (for
	// example "opus/48000/2"), and RTCPFeedback the a=rtcp-fb values.
	Formats      []string
	Codecs       []string
	RTCPFeedback []string
}

// Accepted reports whether the answer accepts the m-line.
func (m AnswerMedia) Accepted() bool {
	return m.Port != 0
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
		media := AnswerMedia{
			Kind:    md.MediaName.Media,
			Port:    md.MediaName.Port.Value,
			Formats: md.MediaName.Formats,
		}
		media.MID, _ = md.Attribute(sdp.AttrKeyMID)
		for _, attr := range md.Attributes {
			switch attr.Key {
			case "rtpmap":
				if _, codec, ok := strings.Cut(attr.Value, " "); ok {
					media.Codecs = append(media.Codecs, codec)
				}
			case "rtcp-fb":
				media.RTCPFeedback = append(media.RTCPFeedback, attr.Value)
			}
		}
		facts.Media = append(facts.Media, media)

		if !media.Accepted() {
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

// AcceptedMedia returns the first accepted m-line of a kind, or nil.
func (a AnswerFacts) AcceptedMedia(kind string) *AnswerMedia {
	for i := range a.Media {
		if a.Media[i].Kind == kind && a.Media[i].Accepted() {
			return &a.Media[i]
		}
	}

	return nil
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

// TimeToFirstDecodedFrame is how long after the connection reached
// "connected" the first echoed video frame decoded; zero if none did.
func (r *Report) TimeToFirstDecodedFrame() time.Duration {
	video := r.Track(kindVideo)
	if video == nil || video.Video == nil || video.Video.FirstDecodedFrameAt == 0 {
		return 0
	}

	return video.Video.FirstDecodedFrameAt - r.ConnectedAt
}

// Summary formats the report for a test log.
func (r *Report) Summary() string {
	var b strings.Builder

	fmt.Fprintf(&b, "call harness report\n")
	fmt.Fprintf(&b, "  recording: history=%s packets=%d units=%d frames=%d payload_bytes=%d event_summaries=%d\n", r.Recording.History, r.Recording.Packets, r.Recording.ContentUnits, r.Recording.FrameMarks, r.Recording.HistoryBytes, r.Recording.EventSummaries)
	fmt.Fprintf(&b, "  signaling:        %d offer/answer exchange(s); answer ice-lite=%t bundle=%v rtcp-mux=%t host candidates=%d\n",
		r.OfferAnswerExchanges, r.Answer.ICELite, r.Answer.Bundle, r.Answer.RTCPMux, len(r.Answer.HostCandidates()))
	fmt.Fprintf(&b, "  answer media:     %s\n", formatAnswerMedia(r.Answer.Media))
	fmt.Fprintf(&b, "  remote address:   %s (the caller's selected candidate pair)\n", r.RemoteAddr)
	fmt.Fprintf(&b, "  connected at:     %s; call length %s (until hangup at %s)\n",
		ms(r.ConnectedAt), ms(r.HungUpAt-r.ConnectedAt), ms(r.HungUpAt))
	fmt.Fprintf(&b, "  connection:       %s (stayed connected: %t)\n", formatStates(r.ConnectionStates), r.ConnectedThroughout())
	fmt.Fprintf(&b, "  ICE connection:   %s\n", formatStates(r.ICEConnectionStates))
	fmt.Fprintf(&b, "  decrypt failures: %d (auth tag %d, replay %d, other %d)\n",
		r.DecryptionFailures.Total(), r.DecryptionFailures.AuthTag, r.DecryptionFailures.Replay, r.DecryptionFailures.Other)
	fmt.Fprintf(&b, "  renegotiations:   %d; ICE restarts: %d\n", r.Renegotiations, r.ICERestarts)
	fmt.Fprintf(&b, "  sent audio:       ssrc=%d frames=%d keyframe requests received=%d\n",
		r.SentAudio.SSRC, r.SentAudio.Frames, r.SentAudio.KeyframeRequests)
	if r.SentVideo.Frames > 0 {
		fmt.Fprintf(&b, "  sent video:       ssrc=%d frames=%d keyframes=%d first at %s; keyframe requests received=%d (caller sent %d)\n",
			r.SentVideo.SSRC, r.SentVideo.Frames, r.SentVideo.Keyframes, ms(r.FirstVideoSentAt),
			r.SentVideo.KeyframeRequests, r.KeyframeRequestsSent)
	}
	if len(r.Tracks) == 0 {
		fmt.Fprintf(&b, "  received:         no tracks\n")
	}
	for _, t := range r.Tracks {
		fmt.Fprintf(&b, "  received %s:   ssrc=%d pt=%d packets=%d first=%s last=%s media gap=%s (ended at %s) seq discontinuities=%d",
			t.Kind, t.SSRC, t.PayloadType, t.Packets, ms(t.FirstArrival), ms(t.LastArrival),
			ms(t.MediaGap), ms(t.MediaGapEndedAt), t.SequenceDiscontinuities)
		if t.Video == nil {
			fmt.Fprintf(&b, " unmatched payloads=%d\n", t.UnmatchedPayloads)

			continue
		}
		b.WriteString("\n")
		writeVideoSummary(&b, r, t.Video)
	}
	writeHandoverSummary(&b, r)

	return b.String()
}

func writeVideoSummary(b *strings.Builder, r *Report, v *VideoReport) {
	fmt.Fprintf(b, "    frames:         %d complete (%d keyframes), %d incomplete, %d gaps, %d unmatched\n",
		v.Frames, v.Keyframes, v.IncompleteFrames, v.FrameGaps, v.UnmatchedFrames)
	if v.FirstDecodedFrameAt == 0 {
		fmt.Fprintf(b, "    first decoded:  none\n")
	} else {
		fmt.Fprintf(b, "    first decoded:  at %s = %s after connected, %s after the first video frame was sent (%dx%d)\n",
			ms(v.FirstDecodedFrameAt), ms(r.TimeToFirstDecodedFrame()), ms(v.FirstDecodedFrameAt-r.FirstVideoSentAt),
			v.Width, v.Height)
	}
	fmt.Fprintf(b, "    decodable:      %d/%d frames; keyframes decoded in Go %d/%d; %d before first keyframe, %d undecodable",
		v.DecodableFrames, v.Frames, v.KeyframesDecoded, v.Keyframes, v.FramesBeforeFirstKeyframe, v.UndecodableFrames)
	if v.LastDecodeError != "" {
		fmt.Fprintf(b, " (last keyframe error: %s)", v.LastDecodeError)
	}
	b.WriteString("\n")
	if v.OnlineDecode.Every > 0 {
		fmt.Fprintf(b, "    online decode: every=%d sampled=%d decoded=%d rate=%.4f dropped=%d errors=%d conclusive=%t\n", v.OnlineDecode.Every, v.OnlineDecode.Sampled, v.OnlineDecode.Decoded, v.OnlineDecode.SampleRate(v.Frames), v.OnlineDecode.Dropped, v.OnlineDecode.Errors, v.OnlineDecode.Conclusive())
	}
	switch full := v.FullDecode; {
	case !full.Ran() && v.OnlineDecode.Every > 0:
		fmt.Fprintf(b, "    whole-call decode: SKIPPED (%s)\n", full.Skipped)
	case !full.Ran():
		fmt.Fprintf(b, "    full decode:    SKIPPED (%s); interframes checked for completeness and order only\n", full.Skipped)
	case full.Errors != "":
		fmt.Fprintf(b, "    full decode:    ffmpeg decoded %d/%d frames with errors: %s\n", full.FramesDecoded, full.FramesIn, full.Errors)
	default:
		fmt.Fprintf(b, "    full decode:    ffmpeg decoded %d/%d frames, no errors\n", full.FramesDecoded, full.FramesIn)
	}
}

func formatAnswerMedia(media []AnswerMedia) string {
	if len(media) == 0 {
		return "none"
	}
	parts := make([]string, len(media))
	for i, m := range media {
		if !m.Accepted() {
			parts[i] = fmt.Sprintf("%s mid=%s rejected", m.Kind, m.MID)

			continue
		}
		parts[i] = fmt.Sprintf("%s mid=%s %v", m.Kind, m.MID, m.Codecs)
	}

	return strings.Join(parts, "; ")
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
