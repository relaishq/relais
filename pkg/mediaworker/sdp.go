package mediaworker

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

const (
	mediaAudio = "audio"
	mediaVideo = "video"

	// Every outbound track belongs to one media stream so a caller's browser
	// groups the echoed tracks together.
	answerStreamID = "relais-echo"
	answerCNAME    = "relais-echo"
)

// codecSpec is the one payload format the worker takes for a media type.
type codecSpec struct {
	name      string // rtpmap encoding name, matched case-insensitively
	clockRate int

	// feedback lists the a=rtcp-fb values the worker answers when the offer
	// has them. Video keeps keyframe requests (the worker relays them to the
	// caller) and nothing that needs the worker to retransmit or estimate
	// bandwidth: no nack, transport-cc or goog-remb.
	feedback []string
}

// supportedCodecs is what the worker echoes: Opus audio and VP8 video. Any
// other codec in an offer (RTX, RED, FEC, VP9, H264, AV1, ...) is left out
// of the answer.
var supportedCodecs = map[string]codecSpec{
	mediaAudio: {name: "opus", clockRate: 48000},
	mediaVideo: {name: "VP8", clockRate: 90000, feedback: []string{"nack pli", "ccm fir"}},
}

// remoteOffer is what the media worker takes from a caller's SDP offer.
type remoteOffer struct {
	iceUfrag string
	icePwd   string

	// fingerprintHash and fingerprint pin the caller's DTLS certificate,
	// for example "sha-256" and "AB:CD:...".
	fingerprintHash string
	fingerprint     string

	// candidates are the caller's ICE candidates (a=candidate values) from
	// the bundled transport.
	candidates []string

	// bundle lists the MIDs of the accepted m-lines in the order of the
	// offer's BUNDLE group. bundle[0] is the m-line whose transport the
	// session uses (the tagged m-line), and the only one whose answer carries
	// the worker's candidate.
	bundle []string

	media []*offeredMedia
}

// offeredMedia is one m-line of the offer.
type offeredMedia struct {
	desc *sdp.MediaDescription
	mid  string
	kind string // the m-line's media type: "audio", "video", "application", ...

	// accepted m-lines are answered; all others are rejected (port 0).
	accepted bool
	codec    codec
}

// codec is the payload format the worker picked from an offered m-line.
type codec struct {
	payloadType  uint8
	rtpmap       string   // e.g. "opus/48000/2"
	fmtp         string   // e.g. "minptime=10;useinbandfec=1"
	rtcpFeedback []string // e.g. "nack pli", answered as a=rtcp-fb
}

// parseOffer validates an SDP offer and picks the media the worker answers:
// the first sendrecv audio m-line that offers Opus and the first sendrecv
// video m-line that offers VP8. Both must be in one BUNDLE group. Every
// other m-line, and every other codec, is rejected in the answer.
func parseOffer(raw string) (*remoteOffer, error) {
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedOffer, err)
	}

	offer := &remoteOffer{}
	var first *offeredMedia // the first accepted m-line in m-line order
	for _, md := range desc.MediaDescriptions {
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		media := &offeredMedia{desc: md, mid: mid, kind: md.MediaName.Media}
		offer.media = append(offer.media, media)

		spec, supported := supportedCodecs[media.kind]
		if !supported || offer.accepted(media.kind) != nil || md.MediaName.Port.Value == 0 || !isSendRecv(md) {
			continue
		}
		picked, ok := findCodec(md, spec)
		if !ok {
			continue
		}
		if mid == "" {
			return nil, fmt.Errorf("%w: %s m-line has no a=mid", ErrUnsupportedOffer, media.kind)
		}
		if _, ok := md.Attribute(sdp.AttrKeyRTCPMux); !ok {
			return nil, fmt.Errorf("%w: %s m-line does not offer rtcp-mux", ErrUnsupportedOffer, media.kind)
		}

		media.accepted = true
		media.codec = picked
		if first == nil {
			first = media
		}
	}

	if first == nil {
		return nil, fmt.Errorf("%w: no sendrecv m-line offers Opus audio or VP8 video", ErrUnsupportedOffer)
	}

	// Every session is one bundled transport, and an answer may only bundle
	// what the offer bundled. Media outside the first accepted m-line's
	// BUNDLE group is rejected.
	group := bundleGroup(&desc, first.mid)
	if group == nil {
		return nil, fmt.Errorf("%w: m-line %q is not in an offered BUNDLE group", ErrUnsupportedOffer, first.mid)
	}
	for _, media := range offer.media {
		if media.accepted && !slices.Contains(group, media.mid) {
			media.accepted = false
		}
	}
	for _, mid := range group {
		if media := offer.byMID(mid); media != nil && media.accepted {
			offer.bundle = append(offer.bundle, mid)
		}
	}

	// The session tells its tracks apart by payload type.
	if audio, video := offer.accepted(mediaAudio), offer.accepted(mediaVideo); audio != nil && video != nil &&
		audio.codec.payloadType == video.codec.payloadType {
		return nil, fmt.Errorf("%w: Opus and VP8 share payload type %d", ErrUnsupportedOffer, audio.codec.payloadType)
	}

	transport := offer.byMID(offer.bundle[0]).desc
	offer.iceUfrag = attribute(&desc, transport, "ice-ufrag")
	offer.icePwd = attribute(&desc, transport, "ice-pwd")
	if offer.iceUfrag == "" || offer.icePwd == "" {
		return nil, fmt.Errorf("%w: missing ICE credentials", ErrUnsupportedOffer)
	}

	fingerprint := attribute(&desc, transport, "fingerprint")
	hash, value, ok := strings.Cut(fingerprint, " ")
	if !ok || hash == "" || value == "" {
		return nil, fmt.Errorf("%w: missing or malformed DTLS fingerprint", ErrUnsupportedOffer)
	}
	offer.fingerprintHash = strings.ToLower(hash)
	offer.fingerprint = strings.TrimSpace(value)

	// The worker answers a=setup:passive, so the caller must be able to act as
	// the DTLS client.
	switch setup := attribute(&desc, transport, sdp.AttrKeyConnectionSetup); setup {
	case "actpass", "active":
	default:
		return nil, fmt.Errorf("%w: a=setup:%s (want actpass or active)", ErrUnsupportedOffer, setup)
	}

	for _, attr := range transport.Attributes {
		if attr.Key == sdp.AttrKeyCandidate {
			offer.candidates = append(offer.candidates, attr.Value)
		}
	}

	return offer, nil
}

// accepted returns the accepted m-line of a media type, or nil.
func (o *remoteOffer) accepted(kind string) *offeredMedia {
	for _, media := range o.media {
		if media.accepted && media.kind == kind {
			return media
		}
	}

	return nil
}

func (o *remoteOffer) byMID(mid string) *offeredMedia {
	for _, media := range o.media {
		if media.mid == mid {
			return media
		}
	}

	return nil
}

// answerParams are the media worker's half of the answer.
type answerParams struct {
	iceUfrag    string
	icePwd      string
	fingerprint string         // sha-256 fingerprint of the session's certificate
	candidate   netip.AddrPort // the worker's socket: the single host candidate

	// tracks are the worker's outbound tracks by the MID they answer.
	tracks map[string]trackState
}

// buildAnswer writes the SDP answer for an offer: ICE-lite, BUNDLE of the
// accepted m-lines, rtcp-mux, a=setup:passive and one host candidate on the
// tagged m-line. Accepted m-lines carry exactly one codec.
func buildAnswer(offer *remoteOffer, params answerParams) (string, error) {
	desc, err := sdp.NewJSEPSessionDescription(false)
	if err != nil {
		return "", err
	}

	addressType := "IP4"
	if params.candidate.Addr().Is6() {
		addressType = "IP6"
	}

	for _, media := range offer.media {
		if !media.accepted {
			desc.WithMedia(rejectedMedia(media))

			continue
		}
		track, ok := params.tracks[media.mid]
		if !ok {
			return "", fmt.Errorf("mediaworker: no outbound track for m-line %q", media.mid)
		}

		// Bundled m-lines share the tagged m-line's port.
		md := &sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:   media.kind,
				Port:    sdp.RangedPort{Value: int(params.candidate.Port())},
				Protos:  media.desc.MediaName.Protos,
				Formats: []string{strconv.Itoa(int(media.codec.payloadType))},
			},
			ConnectionInformation: &sdp.ConnectionInformation{
				NetworkType: "IN",
				AddressType: addressType,
				Address:     &sdp.Address{Address: params.candidate.Addr().String()},
			},
		}
		md.WithValueAttribute(sdp.AttrKeyMID, media.mid).
			WithICECredentials(params.iceUfrag, params.icePwd).
			WithFingerprint("sha-256", params.fingerprint).
			WithValueAttribute(sdp.AttrKeyConnectionSetup, "passive").
			WithPropertyAttribute(sdp.AttrKeyRTCPMux).
			WithPropertyAttribute(sdp.AttrKeySendRecv).
			WithValueAttribute("rtpmap", fmt.Sprintf("%d %s", media.codec.payloadType, media.codec.rtpmap))
		for _, fb := range media.codec.rtcpFeedback {
			md.WithValueAttribute("rtcp-fb", fmt.Sprintf("%d %s", media.codec.payloadType, fb))
		}
		if media.codec.fmtp != "" {
			md.WithValueAttribute("fmtp", fmt.Sprintf("%d %s", media.codec.payloadType, media.codec.fmtp))
		}

		md.WithValueAttribute(sdp.AttrKeyMsid, answerStreamID+" "+track.ID).
			WithValueAttribute(sdp.AttrKeySSRC, fmt.Sprintf("%d cname:%s", track.SSRC, answerCNAME)).
			WithValueAttribute(sdp.AttrKeySSRC, fmt.Sprintf("%d msid:%s %s", track.SSRC, answerStreamID, track.ID))
		if media.mid == offer.bundle[0] {
			md.WithValueAttribute(sdp.AttrKeyCandidate, hostCandidate(params.candidate)).
				WithPropertyAttribute(sdp.AttrKeyEndOfCandidates)
		}

		desc.WithMedia(md)
	}

	desc.WithPropertyAttribute(sdp.AttrKeyICELite).
		WithValueAttribute(sdp.AttrKeyGroup, "BUNDLE "+strings.Join(offer.bundle, " "))

	out, err := desc.Marshal()
	if err != nil {
		return "", err
	}

	return string(out), nil
}

// rejectedMedia answers an m-line the worker does not take: port 0 and the
// offered formats, as JSEP requires.
func rejectedMedia(media *offeredMedia) *sdp.MediaDescription {
	formats := media.desc.MediaName.Formats
	if len(formats) > 1 {
		formats = formats[:1]
	}
	md := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   media.desc.MediaName.Media,
			Port:    sdp.RangedPort{Value: 0},
			Protos:  media.desc.MediaName.Protos,
			Formats: formats,
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: "0.0.0.0"},
		},
	}
	if media.mid != "" {
		md.WithValueAttribute(sdp.AttrKeyMID, media.mid)
	}

	return md.WithPropertyAttribute(sdp.AttrKeyInactive)
}

// findCodec returns the first payload format on an m-line that matches spec,
// with the offered rtcp-fb values the worker keeps for it.
func findCodec(md *sdp.MediaDescription, spec codecSpec) (codec, bool) {
	rtpmaps := map[string]string{}
	fmtps := map[string]string{}
	feedback := map[string][]string{}
	for _, attr := range md.Attributes {
		pt, value, ok := strings.Cut(attr.Value, " ")
		if !ok {
			continue
		}
		switch attr.Key {
		case "rtpmap":
			rtpmaps[pt] = value
		case "fmtp":
			fmtps[pt] = value
		case "rtcp-fb":
			feedback[pt] = append(feedback[pt], strings.TrimSpace(value))
		}
	}

	for _, format := range md.MediaName.Formats {
		rtpmap, ok := rtpmaps[format]
		if !ok {
			continue
		}
		name, rest, _ := strings.Cut(rtpmap, "/")
		clockRate, _, _ := strings.Cut(rest, "/")
		if !strings.EqualFold(name, spec.name) || clockRate != strconv.Itoa(spec.clockRate) {
			continue
		}
		pt, err := strconv.ParseUint(format, 10, 8)
		if err != nil {
			continue
		}

		picked := codec{payloadType: uint8(pt), rtpmap: rtpmap, fmtp: fmtps[format]}
		for _, fb := range spec.feedback {
			if slices.Contains(feedback[format], fb) || slices.Contains(feedback["*"], fb) {
				picked.rtcpFeedback = append(picked.rtcpFeedback, fb)
			}
		}

		return picked, true
	}

	return codec{}, false
}

// isSendRecv reports whether an offered m-line is sendrecv (the default when
// no direction attribute is present). Echo needs both directions.
func isSendRecv(md *sdp.MediaDescription) bool {
	for _, attr := range md.Attributes {
		switch attr.Key {
		case sdp.AttrKeySendOnly, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive:
			return false
		}
	}

	return true
}

// bundleGroup returns the MIDs of the offer's BUNDLE group that contains
// mid, or nil.
func bundleGroup(desc *sdp.SessionDescription, mid string) []string {
	for _, attr := range desc.Attributes {
		if attr.Key != sdp.AttrKeyGroup {
			continue
		}
		fields := strings.Fields(attr.Value)
		if len(fields) == 0 || fields[0] != "BUNDLE" {
			continue
		}
		if slices.Contains(fields[1:], mid) {
			return fields[1:]
		}
	}

	return nil
}

// attribute reads a media-level attribute, falling back to session level.
func attribute(desc *sdp.SessionDescription, md *sdp.MediaDescription, key string) string {
	if value, ok := md.Attribute(key); ok {
		return value
	}
	value, _ := desc.Attribute(key)

	return value
}
