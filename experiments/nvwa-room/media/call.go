// Package media makes an independent Pion caller through relais-signal.
package media

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
	"github.com/relaishq/relais/experiments/nvwa-room/client"
)

type Signal struct {
	URL, TokenFile, CertFile string
	http                     *http.Client
	token                    string
}

func (s *Signal) Init() error {
	pem, err := os.ReadFile(s.CertFile)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return errors.New("invalid signal CA")
	}
	token, err := os.ReadFile(s.TokenFile)
	if err != nil {
		return err
	}
	s.token = strings.TrimSpace(string(token))
	s.http = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return nil
}
func (s *Signal) request(ctx context.Context, method, path string, b []byte) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.URL+path, bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/sdp")
	}
	res, err := s.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil {
		return nil, "", err
	}
	if res.StatusCode != 201 && res.StatusCode != 200 {
		return nil, "", fmt.Errorf("signal %s %s: %s %s", method, path, res.Status, data)
	}
	return data, res.Header.Get("Location"), nil
}
func (s *Signal) Offer(ctx context.Context, offer string) (string, string, error) {
	b, path, err := s.request(ctx, "POST", "/calls", []byte(offer))
	return string(b), path, err
}
func (s *Signal) Delete(ctx context.Context, path string) error {
	if !strings.HasPrefix(path, "/calls/") || strings.Contains(path[7:], "/") {
		return errors.New("invalid call location")
	}
	_, _, err := s.request(ctx, "DELETE", path, nil)
	return err
}

type AudioReport struct {
	EvidencePackets [][]byte `json:"evidence_packets,omitempty"`
	SessionID       string   `json:"session_id"`
	Received        int      `json:"received"`
	PeerPayload     int      `json:"peer_payload"`
	SelfPayload     int      `json:"self_payload"`
	UnknownPayload  int      `json:"unknown_payload"`
	MaxGapMS        float64  `json:"max_gap_ms"`
	ArrivalsNS      []int64  `json:"arrivals_ns"`
}
type Call struct {
	PC        *webrtc.PeerConnection
	ID, Path  string
	signal    *Signal
	track     *webrtc.TrackLocalStaticSample
	mu        sync.Mutex
	report    AudioReport
	own, peer map[string]bool
	stop      chan struct{}
	once      sync.Once
	dc        *DC
}

func Dial(ctx context.Context, signal *Signal, worker string, samples, peer [][]byte, data bool) (*Call, error) {
	return DialPolicy(ctx, signal, worker, samples, peer, data, false)
}

func DialPolicy(ctx context.Context, signal *Signal, worker string, samples, peer [][]byte, data, reliableState bool) (*Call, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{BundlePolicy: webrtc.BundlePolicyMaxBundle, RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire})
	if err != nil {
		return nil, err
	}
	c := &Call{PC: pc, signal: signal, stop: make(chan struct{}), own: map[string]bool{}, peer: map[string]bool{}}
	for _, p := range samples {
		c.own[string(p)] = true
	}
	for _, p := range peer {
		c.peer[string(p)] = true
	}
	c.track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "nvwa-spike")
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	sender, err := pc.AddTrack(c.track)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			now := time.Now().UnixNano()
			c.mu.Lock()
			r := &c.report
			if len(r.ArrivalsNS) > 0 {
				gap := float64(now-r.ArrivalsNS[len(r.ArrivalsNS)-1]) / 1e6
				if gap > r.MaxGapMS {
					r.MaxGapMS = gap
				}
			}
			if len(r.ArrivalsNS) < 10000 {
				r.ArrivalsNS = append(r.ArrivalsNS, now)
			}
			r.Received++
			if len(r.EvidencePackets) < 8 {
				r.EvidencePackets = append(r.EvidencePackets, append([]byte(nil), p.Payload...))
			}
			if c.peer[string(p.Payload)] {
				r.PeerPayload++
			} else if c.own[string(p.Payload)] {
				r.SelfPayload++
			} else {
				r.UnknownPayload++
			}
			c.mu.Unlock()
		}
	})
	if data {
		c.dc, err = NewDCPolicy(pc, reliableState)
		if err != nil {
			_ = pc.Close()
			return nil, err
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, err
	}
	select {
	case <-gather:
	case <-ctx.Done():
		_ = pc.Close()
		return nil, ctx.Err()
	}
	path := "/calls"
	if worker != "" {
		path += "?worker=" + worker
	}
	b, location, err := signal.request(ctx, "POST", path, []byte(pc.LocalDescription().SDP))
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	c.Path = location
	c.ID = strings.TrimPrefix(location, "/calls/")
	c.report.SessionID = c.ID
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(b)}); err != nil {
		c.Close()
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				if len(samples) > 0 {
					_ = c.track.WriteSample(media.Sample{Data: samples[i%len(samples)], Duration: 20 * time.Millisecond})
					i++
				}
			}
		}
	}()
	return c, nil
}
func (c *Call) Close() {
	c.once.Do(func() {
		close(c.stop)
		_ = c.PC.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.signal.Delete(ctx, c.Path)
	})
}
func (c *Call) Report() AudioReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.report
	v.ArrivalsNS = append([]int64(nil), v.ArrivalsNS...)
	return v
}
func (c *Call) DataConn() client.MessageConn { return c.dc }

// LoadOpus requires one packet per Ogg page (ffmpeg -page_duration 20000).
func LoadOpus(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, _, err := oggreader.NewWith(f)
	if err != nil {
		return nil, err
	}
	var packets [][]byte
	for {
		b, _, err := r.ParseNextPage()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if bytes.HasPrefix(b, []byte("OpusTags")) {
			continue
		}
		packets = append(packets, b)
	}
	if len(packets) == 0 {
		return nil, errors.New("no Opus packets")
	}
	return packets, nil
}
