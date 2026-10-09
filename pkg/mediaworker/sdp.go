package mediaworker

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

const (
	opusClockRate = 48000

	// Every outbound track belongs to one media stream so a caller's browser
	// groups the echoed tracks together.
	answerStreamID = "relais-echo"
	answerCNAME    = "relais-echo"
)

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

	media []*offeredMedia
}

// offeredMedia is one m-line of the offer.
type offeredMedia struct {
	desc *sdp.MediaDescription
	mid  string

	// accepted m-lines are answered; all others are rejected (port 0).
	accepted bool
	codec    codec
}

// codec is the payload format the worker picked from an offered m-line.
type codec struct {
	payloadType uint8
	rtpmap      string // e.g. "opus/48000/2"
	fmtp        string // e.g. "minptime=10;useinbandfec=1"
}

// parseOffer validates an SDP offer and picks the media the worker answers.
// It accepts the first sendrecv audio m-line that offers Opus; every other
// m-line is rejected in the answer.
func parseOffer(raw string) (*remoteOffer, error) {
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedOffer, err)
	}

	offer := &remoteOffer{}
	var transport *sdp.MediaDescription

	for _, md := range desc.MediaDescriptions {
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		media := &offeredMedia{desc: md, mid: mid}
		offer.media = append(offer.media, media)

		if transport != nil || md.MediaName.Media != "audio" || md.MediaName.Port.Value == 0 {
			continue
		}
		if !isSendRecv(&desc, md) {
			continue
		}
		opus, ok := findOpus(md)
		if !ok {
			continue
		}
		if mid == "" {
			return nil, fmt.Errorf("%w: audio m-line has no a=mid", ErrUnsupportedOffer)
		}
		if _, ok := md.Attribute(sdp.AttrKeyRTCPMux); !ok {
			return nil, fmt.Errorf("%w: audio m-line does not offer rtcp-mux", ErrUnsupportedOffer)
		}

		media.accepted = true
		media.codec = opus
		transport = md
	}

	if transport == nil {
		return nil, fmt.Errorf("%w: no sendrecv audio m-line offers Opus", ErrUnsupportedOffer)
	}

	// Every session is one bundled transport, and an answer may only bundle
	// what the offer bundled.
	transportMID, _ := transport.Attribute(sdp.AttrKeyMID)
	if !offersBundle(&desc, transportMID) {
		return nil, fmt.Errorf("%w: m-line %q is not in an offered BUNDLE group", ErrUnsupportedOffer, transportMID)
	}

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

// audio returns the accepted audio m-line. parseOffer guarantees there is one.
func (o *remoteOffer) audio() *offeredMedia {
	for _, media := range o.media {
		if media.accepted {
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
	audio       trackState
}

// buildAnswer writes the SDP answer for an offer: ICE-lite, BUNDLE of the
// accepted m-lines, rtcp-mux, a=setup:passive and one host candidate.
func buildAnswer(offer *remoteOffer, params answerParams) (string, error) {
	desc, err := sdp.NewJSEPSessionDescription(false)
	if err != nil {
		return "", err
	}

	addressType := "IP4"
	if params.candidate.Addr().Is6() {
		addressType = "IP6"
	}

	var bundle []string
	for _, media := range offer.media {
		if !media.accepted {
			desc.WithMedia(rejectedMedia(media))

			continue
		}
		bundle = append(bundle, media.mid)

		md := &sdp.MediaDescription{
			MediaName: sdp.MediaName{
				Media:   media.desc.MediaName.Media,
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
		if media.codec.fmtp != "" {
			md.WithValueAttribute("fmtp", fmt.Sprintf("%d %s", media.codec.payloadType, media.codec.fmtp))
		}

		track := params.audio
		md.WithValueAttribute(sdp.AttrKeyMsid, answerStreamID+" "+track.ID).
			WithValueAttribute(sdp.AttrKeySSRC, fmt.Sprintf("%d cname:%s", track.SSRC, answerCNAME)).
			WithValueAttribute(sdp.AttrKeySSRC, fmt.Sprintf("%d msid:%s %s", track.SSRC, answerStreamID, track.ID)).
			WithValueAttribute(sdp.AttrKeyCandidate, hostCandidate(params.candidate)).
			WithPropertyAttribute(sdp.AttrKeyEndOfCandidates)

		desc.WithMedia(md)
	}

	desc.WithPropertyAttribute(sdp.AttrKeyICELite).
		WithValueAttribute(sdp.AttrKeyGroup, "BUNDLE "+strings.Join(bundle, " "))

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

// findOpus returns the first Opus/48000 payload format offered on an m-line.
func findOpus(md *sdp.MediaDescription) (codec, bool) {
	rtpmaps := map[string]string{}
	fmtps := map[string]string{}
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
		}
	}

	for _, format := range md.MediaName.Formats {
		rtpmap, ok := rtpmaps[format]
		if !ok {
			continue
		}
		name, rest, _ := strings.Cut(rtpmap, "/")
		clockRate, _, _ := strings.Cut(rest, "/")
		if !strings.EqualFold(name, "opus") || clockRate != strconv.Itoa(opusClockRate) {
			continue
		}
		pt, err := strconv.ParseUint(format, 10, 8)
		if err != nil {
			continue
		}

		return codec{payloadType: uint8(pt), rtpmap: rtpmap, fmtp: fmtps[format]}, true
	}

	return codec{}, false
}

// isSendRecv reports whether an offered m-line is sendrecv. Echo needs both
// directions.
func isSendRecv(desc *sdp.SessionDescription, md *sdp.MediaDescription) bool {
	return direction(desc, md) == sdp.AttrKeySendRecv
}

// direction resolves an offered m-line's direction (RFC 8866 section 6.7):
// its own direction attribute, else the session-level one, else sendrecv.
func direction(desc *sdp.SessionDescription, md *sdp.MediaDescription) string {
	if dir, ok := directionAttribute(md.Attributes); ok {
		return dir
	}
	if dir, ok := directionAttribute(desc.Attributes); ok {
		return dir
	}

	return sdp.AttrKeySendRecv
}

func directionAttribute(attrs []sdp.Attribute) (string, bool) {
	for _, attr := range attrs {
		switch attr.Key {
		case sdp.AttrKeySendRecv, sdp.AttrKeySendOnly, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive:
			return attr.Key, true
		}
	}

	return "", false
}

// offersBundle reports whether the offer has a BUNDLE group containing mid.
func offersBundle(desc *sdp.SessionDescription, mid string) bool {
	for _, attr := range desc.Attributes {
		if attr.Key != sdp.AttrKeyGroup {
			continue
		}
		fields := strings.Fields(attr.Value)
		if len(fields) == 0 || fields[0] != "BUNDLE" {
			continue
		}
		for _, bundled := range fields[1:] {
			if bundled == mid {
				return true
			}
		}
	}

	return false
}

// attribute reads a media-level attribute, falling back to session level.
func attribute(desc *sdp.SessionDescription, md *sdp.MediaDescription, key string) string {
	if value, ok := md.Attribute(key); ok {
		return value
	}
	value, _ := desc.Attribute(key)

	return value
}
