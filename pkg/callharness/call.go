package callharness

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// echoDrain is how long Hangup waits for the last echoed packets to arrive
// before it stops recording.
const echoDrain = 500 * time.Millisecond

const (
	opusPayloadType = 111
	vp8PayloadType  = 96
)

// CallOptions shape the call the caller makes.
type CallOptions struct {
	// InitialSequenceNumbers replaces random RTP starts for wrap tests. Nil
	// keeps Pion's random starts. Both tracks keep their normal packetizers.
	InitialSequenceNumbers *RTPSequenceNumbers

	// Video adds a VP8 video track next to the Opus audio track, on the same
	// bundled connection.
	Video bool
	// VideoData optionally supplies a VP8 IVF fixture. Nil uses the embedded
	// 320x240/30 fps fixture (natural keyframe interval one second).
	VideoData []byte

	// BrowserLikeOffer makes the caller offer what a browser offers rather
	// than just the codecs it sends: every codec Pion knows (VP8, VP9, H264,
	// H265 and AV1, each with RTX), RED, ULPFEC, telephone-event and comfort
	// noise, extra RTP header extensions, and an RTX ssrc-group for video.
	BrowserLikeOffer bool

	// Worker is the index of the media worker that takes the call (see
	// Options.Workers); 0 is the first in an owned topology.
	// In external mode the number is the literal registration name: the zero
	// value CallOptions{} pins to "0", which must exist.
	// -1 leaves placement unpinned on relay/control-plane topologies.
	Worker int
}

// RTPSequenceNumbers selects deterministic starts for the caller's tracks.
type RTPSequenceNumbers struct {
	Audio uint16
	Video uint16
}

// KeyframeRequest is an RTCP message that asks a sender for a keyframe.
type KeyframeRequest int

const (
	// PLI is a Picture Loss Indication (RFC 4585).
	PLI KeyframeRequest = iota
	// FIR is a Full Intra Request (RFC 5104).
	FIR
)

// Call is one call made by the caller: a Pion PeerConnection that sends
// pre-encoded Opus audio (and VP8 video) and records everything it observes.
type Call struct {
	harness *Harness
	pc      *webrtc.PeerConnection
	audio   *webrtc.TrackLocalStaticSample
	video   *webrtc.TrackLocalStaticSample // nil for an audio-only call
	rec     *recorder

	audioSSRC   uint32
	videoSSRC   uint32
	offer       string
	answer      AnswerFacts
	resourceURL string // the call's signaling resource, from Location
	localUfrag  string
	remoteUfrag string

	// keyframeWanted is set when the worker asks the caller for a keyframe;
	// the video sender answers it the way a browser's encoder would.
	keyframeWanted   atomic.Bool
	videoData        []byte
	initialSequences *RTPSequenceNumbers

	// socket is the caller's UDP socket, which observes its consent checks
	// (consent.go).
	socket *callerSocket

	// mu guards closing and the reader goroutines' lifecycle: a reader starts
	// only under mu while closing is false, so close's Wait never races an
	// Add.
	mu            sync.Mutex
	closing       bool
	echoVideoSSRC uint32
	haveEchoVideo bool
	firSequence   uint8
	readers       sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// Dial starts a call: it makes one WHIP-style offer/answer exchange with the
// system and waits until the caller's connection is "connected". It fails
// with ErrHarnessClosed once the harness is closing.
func (h *Harness) Dial(ctx context.Context, opts CallOptions) (call *Call, err error) {
	rec := newRecorder()

	socket, err := h.newCallerSocket(rec)
	if err != nil {
		return nil, err
	}
	api, err := newCallerAPI(rec, opts.BrowserLikeOffer, socket)
	if err != nil {
		return nil, errors.Join(err, socket.close())
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("callharness: new PeerConnection: %w", err), socket.close())
	}
	call = &Call{harness: h, pc: pc, rec: rec, socket: socket, videoData: opts.VideoData, initialSequences: opts.InitialSequenceNumbers}
	if err := h.addCall(call); err != nil {
		return nil, errors.Join(err, call.close())
	}
	defer func() {
		if err != nil {
			_ = call.close()
			call = nil
		}
	}()

	call.audio, call.audioSSRC, err = call.addTrack(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: opusSampleRate, Channels: 2}, kindAudio)
	if err != nil {
		return call, err
	}
	rec.sentAudio.SSRC = call.audioSSRC
	if opts.Video {
		call.video, call.videoSSRC, err = call.addTrack(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: vp8ClockRate}, kindVideo)
		if err != nil {
			return call, err
		}
		rec.sentVideo.SSRC = call.videoSSRC
	}

	connected := make(chan struct{})
	var connectedOnce sync.Once
	failed := make(chan webrtc.PeerConnectionState, 1)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		rec.connectionState(state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connectedOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			select {
			case failed <- state:
			default:
			}
		default:
		}
	})
	pc.OnICEConnectionStateChange(rec.iceConnectionState)
	pc.OnSignalingStateChange(rec.signalingState)
	pc.OnNegotiationNeeded(rec.negotiationNeeded)
	pc.OnTrack(call.onTrack)

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return call, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return call, err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return call, ctx.Err()
	}

	call.offer = pc.LocalDescription().SDP
	rec.offerAnswerExchange()
	answer, resourceURL, err := h.postOffer(ctx, opts.Worker, call.offer)
	if err != nil {
		return call, err
	}
	call.resourceURL = resourceURL
	if call.answer, err = parseAnswer(answer); err != nil {
		return call, fmt.Errorf("callharness: parse answer: %w", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return call, err
	}
	rec.markNegotiated()
	call.localUfrag = iceUfrag(pc.LocalDescription())
	call.remoteUfrag = iceUfrag(pc.RemoteDescription())

	select {
	case <-connected:
		return call, nil
	case state := <-failed:
		return call, fmt.Errorf("callharness: connection %s before connecting", state)
	case <-ctx.Done():
		return call, fmt.Errorf("callharness: waiting for connected: %w", ctx.Err())
	}
}

// addTrack adds one of the caller's tracks and reads the RTCP sent to it.
func (c *Call) addTrack(capability webrtc.RTPCodecCapability, kind string) (*webrtc.TrackLocalStaticSample, uint32, error) {
	options := []func(*webrtc.TrackLocalStaticRTP){webrtc.WithPayloader(func(_ webrtc.RTPCodecCapability) (rtp.Payloader, error) {
		if kind == kindVideo {
			return &codecs.VP8Payloader{EnablePictureID: true}, nil
		}
		return &codecs.OpusPayloader{}, nil
	})}
	if c.initialSequences != nil {
		sequence := c.initialSequences.Audio
		if kind == kindVideo {
			sequence = c.initialSequences.Video
		}
		options = append(options, webrtc.WithRTPSequenceNumber(sequence))
	}
	track, err := webrtc.NewTrackLocalStaticSample(capability, kind, "caller", options...)
	if err != nil {
		return nil, 0, err
	}
	sender, err := c.pc.AddTrack(track)
	if err != nil {
		return nil, 0, err
	}
	var ssrc uint32
	if encodings := sender.GetParameters().Encodings; len(encodings) > 0 {
		ssrc = uint32(encodings[0].SSRC)
	}

	c.startReader(func() {
		// Reading RTCP also runs the sender's interceptors.
		for {
			packets, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for range keyframeRequests(packets, ssrc) {
				c.rec.keyframeRequestReceived(kind)
				if kind == kindVideo {
					c.keyframeWanted.Store(true)
				}
			}
		}
	})

	return track, ssrc, nil
}

// keyframeRequests returns the PLI and FIR packets in packets that ask for a
// keyframe on ssrc.
func keyframeRequests(packets []rtcp.Packet, ssrc uint32) []rtcp.Packet {
	var requests []rtcp.Packet
	for _, packet := range packets {
		switch p := packet.(type) {
		case *rtcp.PictureLossIndication:
			if p.MediaSSRC == ssrc {
				requests = append(requests, p)
			}
		case *rtcp.FullIntraRequest:
			for _, entry := range p.FIR {
				if entry.SSRC == ssrc {
					requests = append(requests, p)

					break
				}
			}
		}
	}

	return requests
}

func (c *Call) onTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	kind := track.Kind().String()
	record := c.rec.addTrack(kind, uint32(track.SSRC()), uint8(track.PayloadType()))
	if kind == kindVideo {
		c.mu.Lock()
		c.echoVideoSSRC = uint32(track.SSRC())
		c.haveEchoVideo = true
		c.mu.Unlock()
	}

	c.startReader(func() {
		var assembler *vp8Assembler
		if kind == kindVideo {
			assembler = &vp8Assembler{}
		}
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			c.rec.packet(record, pkt, time.Now())
			if assembler == nil {
				continue
			}

			frame, incomplete := assembler.push(pkt)
			if incomplete > 0 {
				c.rec.videoIncomplete(record, incomplete)
			}
			if frame == nil {
				continue
			}
			var size image.Point
			var decodeErr error
			if frame.keyframe {
				size, decodeErr = decodeKeyframe(frame.data)
			}
			c.rec.videoFrame(record, frame, size, decodeErr, time.Now())
		}
	})
}

// startReader runs fn on a goroutine that Hangup waits for.
func (c *Call) startReader(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return
	}
	c.readers.Add(1)
	go func() {
		defer c.readers.Done()
		fn()
	}()
}

// SendMedia plays the pre-encoded media in real time, looping it, for the
// given duration: Opus audio, and VP8 video when the call has video.
func (c *Call) SendMedia(ctx context.Context, duration time.Duration) error {
	end := time.Now().Add(duration)

	var wg sync.WaitGroup
	var audioErr, videoErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		audioErr = c.sendAudio(ctx, end)
	}()
	if c.video != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			videoErr = c.sendVideo(ctx, end)
		}()
	}
	wg.Wait()

	return errors.Join(audioErr, videoErr)
}

func (c *Call) sendAudio(ctx context.Context, end time.Time) error {
	src, err := newOpusSource(callerAudio)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(opusFrameDuration)
	defer ticker.Stop()

	for time.Now().Before(end) {
		frame, frameDuration, err := src.next()
		if err != nil {
			return err
		}
		// The echo can arrive before WriteSample returns, so the frame is
		// registered first; it is counted once the write succeeds.
		c.rec.sending(kindAudio, frame)
		if err := c.audio.WriteSample(media.Sample{Data: frame, Duration: frameDuration}); err != nil {
			return fmt.Errorf("callharness: send audio: %w", err)
		}
		c.rec.sent(kindAudio, false)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	return nil
}

// sendVideo sends one VP8 frame per frame interval. When the worker asks for
// a keyframe, the caller rewinds to an already encoded keyframe on its next
// 33 ms tick, with no encoder delay. Real browser timing is #13's concern.
func (c *Call) sendVideo(ctx context.Context, end time.Time) error {
	data := c.videoData
	if data == nil {
		data = callerVideo
	}
	src, err := newVP8Source(data)
	if err != nil {
		return err
	}

	c.rec.mu.Lock()
	c.rec.keyframeInterval = src.keyframeInterval()
	c.rec.mu.Unlock()
	ticker := time.NewTicker(src.frameDuration)
	defer ticker.Stop()

	for time.Now().Before(end) {
		pliResponse := c.keyframeWanted.Swap(false)
		if pliResponse {
			if err := src.rewind(); err != nil {
				return err
			}
		}
		frame, keyframe, err := src.next()
		if err != nil {
			return err
		}
		c.rec.sendingVideo(frame, pliResponse, src.frameDuration) // records the observable PictureID before writing
		if err := c.video.WriteSample(media.Sample{Data: frame, Duration: src.frameDuration}); err != nil {
			return fmt.Errorf("callharness: send video: %w", err)
		}
		c.rec.sent(kindVideo, keyframe)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	return nil
}

// RequestKeyframe asks the system for a keyframe on the echoed video, as a
// browser's receiver does when it cannot decode. It fails until the echoed
// video track has arrived.
func (c *Call) RequestKeyframe(request KeyframeRequest) error {
	c.mu.Lock()
	echo, ok := c.echoVideoSSRC, c.haveEchoVideo
	c.firSequence++
	sequence := c.firSequence
	c.mu.Unlock()
	if !ok {
		return errors.New("callharness: no echoed video track to request a keyframe on")
	}

	var packet rtcp.Packet
	switch request {
	case PLI:
		packet = &rtcp.PictureLossIndication{SenderSSRC: c.videoSSRC, MediaSSRC: echo}
	case FIR:
		packet = &rtcp.FullIntraRequest{
			SenderSSRC: c.videoSSRC,
			FIR:        []rtcp.FIREntry{{SSRC: echo, SequenceNumber: sequence}},
		}
	default:
		return fmt.Errorf("callharness: unknown keyframe request %d", request)
	}
	if err := c.pc.WriteRTCP([]rtcp.Packet{packet}); err != nil {
		return fmt.Errorf("callharness: send keyframe request: %w", err)
	}
	c.rec.keyframeRequestSent()

	return nil
}

// Hangup ends the call and returns what the caller observed. It waits
// briefly for the last echoed packets, stops recording, sends the WHIP-style
// DELETE, closes the PeerConnection and runs the full video decode.
func (c *Call) Hangup(ctx context.Context) (*Report, error) {
	select {
	case <-time.After(echoDrain):
	case <-ctx.Done():
	}

	if iceUfrag(c.pc.CurrentLocalDescription()) != c.localUfrag ||
		iceUfrag(c.pc.CurrentRemoteDescription()) != c.remoteUfrag {
		c.rec.iceRestartObserved()
	}
	takeoverErr := c.collectTakeovers(ctx)
	c.rec.hangup()
	remoteAddr := c.selectedRemoteAddr()

	deleteErr := c.harness.deleteCall(ctx, c.resourceURL)
	closeErr := c.close()

	report := c.rec.report()
	report.Offer = c.offer
	report.Answer = c.answer
	report.RemoteAddr = remoteAddr
	for i := range report.Tracks {
		if video := report.Tracks[i].Video; video != nil {
			frames, size := c.rec.decodeInput(i)
			video.FullDecode = fullDecode(ctx, frames, size)
		}
	}

	return report, errors.Join(takeoverErr, deleteErr, closeErr)
}

// close closes the PeerConnection, waits for the reader goroutines and
// closes the caller's socket. Once is enough: Hangup, a failed Dial and
// Harness.Close may all call it.
func (c *Call) close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()

		err := c.pc.Close()
		c.readers.Wait()
		c.closeErr = errors.Join(err, c.socket.close())
		c.harness.removeCall(c)
	})

	return c.closeErr
}

// newCallerAPI builds the caller's Pion API: Opus and VP8 (or a browser-like
// codec list), Pion's default interceptors, a host candidate on the caller's
// own loopback socket (through its UDP mux, which observes consent checks),
// and a logger factory that counts SRTP decryption failures.
func newCallerAPI(rec *recorder, browserLike bool, socket *callerSocket) (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	register := registerCallerCodecs
	if browserLike {
		register = registerBrowserLikeCodecs
	}
	if err := register(mediaEngine); err != nil {
		return nil, err
	}

	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return nil, err
	}

	settings := webrtc.SettingEngine{LoggerFactory: newCallerLoggerFactory(rec)}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(socket.acceptsCandidate)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	settings.SetICEUDPMux(socket.mux)

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
		webrtc.WithSettingEngine(settings),
	), nil
}

// registerCallerCodecs registers just the codecs the caller sends.
func registerCallerCodecs(m *webrtc.MediaEngine) error {
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   opusSampleRate,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: opusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}

	return m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: vp8ClockRate,
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: vp8PayloadType,
	}, webrtc.RTPCodecTypeVideo)
}

// registerBrowserLikeCodecs registers a codec and header extension list
// shaped like Chrome's: Pion's defaults (Opus 111, VP8 96 and other video
// codecs with RTX) plus the extras Chrome offers.
func registerBrowserLikeCodecs(m *webrtc.MediaEngine) error {
	if err := m.RegisterDefaultCodecs(); err != nil {
		return err
	}

	extras := []struct {
		kind  webrtc.RTPCodecType
		codec webrtc.RTPCodecParameters
	}{
		{webrtc.RTPCodecTypeAudio, codecParameters("audio/red", 48000, 2, "111/111", 63)},
		{webrtc.RTPCodecTypeAudio, codecParameters("audio/CN", 8000, 0, "", 13)},
		{webrtc.RTPCodecTypeAudio, codecParameters("audio/telephone-event", 48000, 0, "", 110)},
		{webrtc.RTPCodecTypeAudio, codecParameters("audio/telephone-event", 8000, 0, "", 126)},
		{webrtc.RTPCodecTypeVideo, codecParameters("video/red", 90000, 0, "", 114)},
		{webrtc.RTPCodecTypeVideo, codecParameters(webrtc.MimeTypeRTX, 90000, 0, "apt=114", 115)},
		{webrtc.RTPCodecTypeVideo, codecParameters("video/ulpfec", 90000, 0, "", 118)},
	}
	for _, extra := range extras {
		if err := m.RegisterCodec(extra.codec, extra.kind); err != nil {
			return err
		}
	}

	extensions := []struct {
		kind webrtc.RTPCodecType
		uri  string
	}{
		{webrtc.RTPCodecTypeAudio, sdp.AudioLevelURI},
		{webrtc.RTPCodecTypeAudio, sdp.ABSSendTimeURI},
		{webrtc.RTPCodecTypeVideo, sdp.ABSSendTimeURI},
		{webrtc.RTPCodecTypeVideo, "urn:ietf:params:rtp-hdrext:toffset"},
		{webrtc.RTPCodecTypeVideo, "urn:3gpp:video-orientation"},
		{webrtc.RTPCodecTypeVideo, "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"},
		{webrtc.RTPCodecTypeVideo, "http://www.webrtc.org/experiments/rtp-hdrext/video-content-type"},
	}
	for _, extension := range extensions {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: extension.uri}, extension.kind); err != nil {
			return err
		}
	}

	return nil
}

func codecParameters(mimeType string, clockRate uint32, channels uint16, fmtp string, payloadType webrtc.PayloadType) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    mimeType,
			ClockRate:   clockRate,
			Channels:    channels,
			SDPFmtpLine: fmtp,
		},
		PayloadType: payloadType,
	}
}

// iceUfrag returns the ICE username fragment in a session description.
func iceUfrag(desc *webrtc.SessionDescription) string {
	if desc == nil {
		return ""
	}
	var parsed sdp.SessionDescription
	if err := parsed.UnmarshalString(desc.SDP); err != nil {
		return ""
	}
	if ufrag, ok := parsed.Attribute("ice-ufrag"); ok {
		return ufrag
	}
	for _, md := range parsed.MediaDescriptions {
		if ufrag, ok := md.Attribute("ice-ufrag"); ok {
			return ufrag
		}
	}

	return ""
}
