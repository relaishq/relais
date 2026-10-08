package callharness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// echoDrain is how long Hangup waits for the last echoed packets to arrive
// before it stops recording.
const echoDrain = 500 * time.Millisecond

const opusPayloadType = 111

// Call is one call made by the caller: a Pion PeerConnection that sends
// pre-encoded Opus audio and records everything it observes.
type Call struct {
	harness *Harness
	pc      *webrtc.PeerConnection
	audio   *webrtc.TrackLocalStaticSample
	rec     *recorder

	audioSSRC   uint32
	answer      AnswerFacts
	resourceURL string // the call's signaling resource, from Location
	localUfrag  string
	remoteUfrag string

	mu      sync.Mutex
	closing bool
	readers sync.WaitGroup
}

// Dial starts a call: it makes one WHIP-style offer/answer exchange with the
// system and waits until the caller's connection is "connected".
func (h *Harness) Dial(ctx context.Context) (call *Call, err error) {
	rec := newRecorder()

	api, err := newCallerAPI(rec)
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("callharness: new PeerConnection: %w", err)
	}
	call = &Call{harness: h, pc: pc, rec: rec}
	defer func() {
		if err != nil {
			_ = call.close()
			call = nil
		}
	}()

	call.audio, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: opusSampleRate, Channels: 2},
		"audio", "caller",
	)
	if err != nil {
		return call, err
	}
	sender, err := pc.AddTrack(call.audio)
	if err != nil {
		return call, err
	}
	if encodings := sender.GetParameters().Encodings; len(encodings) > 0 {
		call.audioSSRC = uint32(encodings[0].SSRC)
	}
	call.startReader(func() {
		// Read RTCP so the sender's interceptors run.
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	})

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

	answer, err := call.exchange(ctx, pc.LocalDescription().SDP)
	if err != nil {
		return call, err
	}
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

// exchange POSTs the offer and returns the answer: the call's one signaling
// exchange.
func (c *Call) exchange(ctx context.Context, offer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.harness.signalingURL, strings.NewReader(offer))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/sdp")

	c.rec.offerAnswerExchange()
	resp, err := c.harness.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("callharness: POST offer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("callharness: read answer: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("callharness: POST offer: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/sdp") {
		return "", fmt.Errorf("callharness: answer has Content-Type %q", ct)
	}

	if location := resp.Header.Get("Location"); location != "" {
		base, err := url.Parse(c.harness.signalingURL)
		if err != nil {
			return "", err
		}
		ref, err := url.Parse(location)
		if err != nil {
			return "", fmt.Errorf("callharness: bad Location %q: %w", location, err)
		}
		c.resourceURL = base.ResolveReference(ref).String()
	}

	return string(body), nil
}

func (c *Call) onTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	record := c.rec.addTrack(track.Kind().String(), uint32(track.SSRC()), uint8(track.PayloadType()))
	c.startReader(func() {
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			c.rec.packet(record, pkt, time.Now())
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

// SendAudio plays the pre-encoded Opus file in real time, looping it, for
// the given duration.
func (c *Call) SendAudio(ctx context.Context, duration time.Duration) error {
	src, err := newOpusSource(callerAudio)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(opusFrameDuration)
	defer ticker.Stop()

	end := time.Now().Add(duration)
	for time.Now().Before(end) {
		frame, frameDuration, err := src.next()
		if err != nil {
			return err
		}
		if err := c.audio.WriteSample(media.Sample{Data: frame, Duration: frameDuration}); err != nil {
			return fmt.Errorf("callharness: send audio: %w", err)
		}
		c.rec.sent(frame)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	return nil
}

// Hangup ends the call and returns what the caller observed. It waits
// briefly for the last echoed packets, stops recording, sends the WHIP-style
// DELETE and closes the PeerConnection.
func (c *Call) Hangup(ctx context.Context) (*Report, error) {
	select {
	case <-time.After(echoDrain):
	case <-ctx.Done():
	}

	if iceUfrag(c.pc.CurrentLocalDescription()) != c.localUfrag ||
		iceUfrag(c.pc.CurrentRemoteDescription()) != c.remoteUfrag {
		c.rec.iceRestartObserved()
	}
	c.rec.hangup()

	deleteErr := c.deleteResource(ctx)
	closeErr := c.close()

	report := c.rec.report()
	report.Answer = c.answer
	report.SentAudio.SSRC = c.audioSSRC

	return report, errors.Join(deleteErr, closeErr)
}

func (c *Call) deleteResource(ctx context.Context) error {
	if c.resourceURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.resourceURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.harness.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("callharness: DELETE call: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("callharness: DELETE call: %s", resp.Status)
	}

	return nil
}

// close closes the PeerConnection and waits for the reader goroutines.
func (c *Call) close() error {
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()

	err := c.pc.Close()
	c.readers.Wait()

	return err
}

// newCallerAPI builds the caller's Pion API: Opus only, Pion's default
// interceptors, host candidates on loopback only, and a logger factory that
// counts SRTP decryption failures.
func newCallerAPI(rec *recorder) (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   opusSampleRate,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: opusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}

	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return nil, err
	}

	settings := webrtc.SettingEngine{LoggerFactory: newCallerLoggerFactory(rec)}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
		webrtc.WithSettingEngine(settings),
	), nil
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
