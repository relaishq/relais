package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// ---- logger that counts SRTP decrypt failures and ICE/DTLS warnings ----

type logEvent struct {
	at    time.Time
	scope string
	level string
	msg   string
}

type countingLogs struct {
	mu     sync.Mutex
	events []logEvent
	print  func(format string, args ...any)
}

func (c *countingLogs) add(scope, level, msg string) {
	c.mu.Lock()
	c.events = append(c.events, logEvent{time.Now(), scope, level, msg})
	c.mu.Unlock()
	c.print("client log [%s/%s] %s", scope, level, msg)
}

func (c *countingLogs) snapshot() []logEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]logEvent{}, c.events...)
}

func (c *countingLogs) NewLogger(scope string) logging.LeveledLogger {
	return &scopedLog{c: c, scope: scope}
}

type scopedLog struct {
	c     *countingLogs
	scope string
}

// srtp logs decrypt/auth/replay failures at Info; everything else we care
// about is Warn or Error.
func (l *scopedLog) Trace(string)          {}
func (l *scopedLog) Tracef(string, ...any) {}
func (l *scopedLog) Debug(string)          {}
func (l *scopedLog) Debugf(string, ...any) {}
func (l *scopedLog) Info(m string) {
	if l.scope == "srtp" {
		l.c.add(l.scope, "info", m)
	}
}

func (l *scopedLog) Infof(f string, a ...any) { l.Info(fmt.Sprintf(f, a...)) }
func (l *scopedLog) Warn(m string)            { l.c.add(l.scope, "warn", m) }
func (l *scopedLog) Warnf(f string, a ...any) { l.Warn(fmt.Sprintf(f, a...)) }
func (l *scopedLog) Error(m string)           { l.c.add(l.scope, "error", m) }
func (l *scopedLog) Errorf(f string, a ...any) {
	l.Error(fmt.Sprintf(f, a...))
}

// ---- client ----

type echoPkt struct {
	at      time.Time
	seq     uint16
	ts      uint32
	counter uint32
}

type stateEvent struct {
	at   time.Time
	kind string
	val  string
}

type consentSample struct {
	at        time.Time
	responses uint64
	requests  uint64
	state     string
}

type Client struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP
	logs  *countingLogs
	logf  func(format string, args ...any)

	mu         sync.Mutex
	echoes     []echoPkt
	states     []stateEvent
	consent    []consentSample
	srRecv     int
	negNeeded  int
	sent       uint32
	connected  chan struct{}
	connOnce   sync.Once
	dtlsClosed chan struct{}
	dtlsOnce   sync.Once
}

func NewClient(logf func(string, ...any)) (*Client, error) {
	c := &Client{logf: logf, connected: make(chan struct{}), dtlsClosed: make(chan struct{})}
	c.logs = &countingLogs{print: logf}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{LoggerFactory: c.logs}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Defaults are kept for ICE timeouts (disconnected 5s, failed 25s,
	// keepalive 2s) and SRTP/SRTCP replay windows (64).

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	c.pc = pc

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "relais")
	if err != nil {
		return nil, err
	}
	c.track = track
	sender, err := pc.AddTrack(track)
	if err != nil {
		return nil, err
	}
	go func() { // drain RTCP for the sender (required for interceptors)
		for {
			if _, _, err := sender.ReadRTCP(); err != nil {
				return
			}
		}
	}()

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		c.addState("pc", s.String())
		if s == webrtc.PeerConnectionStateConnected {
			c.connOnce.Do(func() { close(c.connected) })
		}
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) { c.addState("ice", s.String()) })
	pc.OnSignalingStateChange(func(s webrtc.SignalingState) { c.addState("signaling", s.String()) })
	pc.OnNegotiationNeeded(func() {
		c.mu.Lock()
		c.negNeeded++
		c.mu.Unlock()
		c.addState("negotiationneeded", "")
	})
	sender.Transport().OnStateChange(func(s webrtc.DTLSTransportState) {
		c.addState("dtls", s.String())
		if s == webrtc.DTLSTransportStateClosed || s == webrtc.DTLSTransportStateFailed {
			c.dtlsOnce.Do(func() { close(c.dtlsClosed) })
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
		c.logf("client: OnTrack ssrc=%d codec=%s", tr.SSRC(), tr.Codec().MimeType)
		go func() {
			for {
				pkts, _, err := rcv.ReadRTCP()
				if err != nil {
					return
				}
				for _, p := range pkts {
					if _, ok := p.(*rtcp.SenderReport); ok {
						c.mu.Lock()
						c.srRecv++
						c.mu.Unlock()
					}
				}
			}
		}()
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				c.logf("client: track read ended: %v", err)

				return
			}
			if len(p.Payload) < 7 {
				continue
			}
			c.mu.Lock()
			c.echoes = append(c.echoes, echoPkt{
				at: time.Now(), seq: p.SequenceNumber, ts: p.Timestamp,
				counter: binary.BigEndian.Uint32(p.Payload[3:7]),
			})
			c.mu.Unlock()
		}
	})

	return c, nil
}

func (c *Client) addState(kind, val string) {
	c.mu.Lock()
	c.states = append(c.states, stateEvent{time.Now(), kind, val})
	c.mu.Unlock()
	c.logf("client state: %s -> %s", kind, val)
}

func (c *Client) Offer() (string, error) {
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	done := webrtc.GatheringCompletePromise(c.pc)
	if err := c.pc.SetLocalDescription(offer); err != nil {
		return "", err
	}
	<-done

	return c.pc.LocalDescription().SDP, nil
}

func (c *Client) Answer(sdp string) error {
	return c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
}

// SendLoop sends 20 ms Opus-shaped packets (a CELT silence TOC + a counter).
func (c *Client) SendLoop(stop <-chan struct{}, seqStart uint16) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	seq, ts := seqStart, uint32(12345)
	var counter uint32
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		payload := make([]byte, 7)
		copy(payload, []byte{0xF8, 0xFF, 0xFE})
		binary.BigEndian.PutUint32(payload[3:], counter)
		_ = c.track.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts},
			Payload: payload,
		})
		seq++
		ts += 960
		counter++
		c.mu.Lock()
		c.sent = counter
		c.mu.Unlock()
	}
}

// SampleConsent polls the selected candidate pair's STUN counters.
func (c *Client) SampleConsent(stop <-chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		var s consentSample
		s.at = time.Now()
		s.state = c.pc.ConnectionState().String()
		for _, st := range c.pc.GetStats() {
			if p, ok := st.(webrtc.ICECandidatePairStats); ok && p.Nominated {
				s.responses, s.requests = p.ResponsesReceived, p.RequestsSent
			}
		}
		c.mu.Lock()
		c.consent = append(c.consent, s)
		c.mu.Unlock()
	}
}

func (c *Client) srtpFailures() []logEvent {
	var out []logEvent
	for _, e := range c.logs.snapshot() {
		if e.scope == "srtp" {
			out = append(out, e)
		}
	}

	return out
}

func (c *Client) warnings() []logEvent {
	var out []logEvent
	for _, e := range c.logs.snapshot() {
		if e.scope != "srtp" && !strings.Contains(e.msg, "srtp") {
			out = append(out, e)
		}
	}

	return out
}
