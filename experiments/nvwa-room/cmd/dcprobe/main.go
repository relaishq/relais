// dcprobe is a transport diagnostic, not a room/game client. The configured
// upstream must echo its binary numbered payloads via the frozen op-0 adapter.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

type channelReport struct {
	Label                                       string `json:"label"`
	Sent, Received, Loss, Duplicates, Reordered int
	MinRTT, MedianRTT, MaxRTT                   time.Duration
}
type receipt struct {
	mu                    sync.Mutex
	label                 string
	sent                  map[uint32]time.Time
	seen                  map[uint32]bool
	rtt                   []time.Duration
	duplicates, reordered int
	highest               uint32
}

func (r *receipt) receive(data []byte) {
	if len(data) != 4 {
		return
	}
	seq := binary.BigEndian.Uint32(data)
	r.mu.Lock()
	defer r.mu.Unlock()
	sent, ok := r.sent[seq]
	if !ok {
		return
	}
	if r.seen[seq] {
		r.duplicates++
		return
	}
	if len(r.seen) > 0 && seq < r.highest {
		r.reordered++
	}
	r.highest = max(r.highest, seq)
	r.seen[seq] = true
	r.rtt = append(r.rtt, time.Since(sent))
}
func (r *receipt) report() channelReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	sort.Slice(r.rtt, func(i, j int) bool { return r.rtt[i] < r.rtt[j] })
	report := channelReport{Label: r.label, Sent: len(r.sent), Received: len(r.seen), Loss: len(r.sent) - len(r.seen), Duplicates: r.duplicates, Reordered: r.reordered}
	if len(r.rtt) > 0 {
		report.MinRTT = r.rtt[0]
		report.MedianRTT = r.rtt[len(r.rtt)/2]
		report.MaxRTT = r.rtt[len(r.rtt)-1]
	}
	return report
}
func run() error {
	signal := flag.String("signal", "https://127.0.0.1:9443", "relais-signal base URL")
	tokenEnv := flag.String("token-env", "RELAIS_SIGNAL_TOKEN", "environment variable containing the signal bearer token")
	ca := flag.String("ca", "", "PEM CA certificate for a private signal server")
	n := flag.Int("n", 1000, "numbered messages per channel")
	interval := flag.Duration("interval", 5*time.Millisecond, "interval between numbered pairs")
	timeout := flag.Duration("timeout", 30*time.Second, "total connect/send/receive deadline")
	settle := flag.Duration("settle", 2*time.Second, "receive window after the last send")
	flag.Parse()
	if *n < 1 || *n > 100000 || *interval < 0 || *timeout <= 0 || *settle < 0 {
		return errors.New("invalid bounded probe settings")
	}
	base, err := url.Parse(*signal)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return errors.New("-signal must be an http(s) URL")
	}
	token := os.Getenv(*tokenEnv)
	if token == "" {
		return fmt.Errorf("%s is required", *tokenEnv)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return errors.New("invalid PEM CA")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer pc.Close()
	var channels [2]*webrtc.DataChannel
	var receipts [2]*receipt
	opened := make(chan struct{}, 2)
	for i, label := range []string{"control", "state"} {
		opts := &webrtc.DataChannelInit{}
		if i == 1 {
			ordered := false
			retransmits := uint16(0)
			opts.Ordered = &ordered
			opts.MaxRetransmits = &retransmits
		}
		dc, err := pc.CreateDataChannel(label, opts)
		if err != nil {
			return err
		}
		channels[i] = dc
		r := &receipt{label: label, sent: make(map[uint32]time.Time), seen: make(map[uint32]bool)}
		receipts[i] = r
		dc.OnOpen(func() { opened <- struct{}{} })
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { r.receive(msg.Data) })
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gather:
	case <-ctx.Done():
		return ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(*signal, "/")+"/calls", bytes.NewBufferString(pc.LocalDescription().SDP))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	answer, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("signal status %d: %s", response.StatusCode, answer)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil || location.IsAbs() || !strings.HasPrefix(location.Path, "/calls/") || location.Host != "" {
		return errors.New("signal returned invalid call location")
	}
	sessionURL := base.ResolveReference(location).String()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r, _ := http.NewRequestWithContext(cleanup, http.MethodDelete, sessionURL, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if resp, err := client.Do(r); err == nil {
			_ = resp.Body.Close()
		}
	}()
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		return err
	}
	for range 2 {
		select {
		case <-opened:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for seq := 0; seq < *n; seq++ {
		for i, dc := range channels {
			data := make([]byte, 4)
			binary.BigEndian.PutUint32(data, uint32(seq))
			r := receipts[i]
			r.mu.Lock()
			r.sent[uint32(seq)] = time.Now()
			r.mu.Unlock()
			if err := dc.Send(data); err != nil {
				return err
			}
		}
		if *interval > 0 {
			timer := time.NewTimer(*interval)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
	timer := time.NewTimer(*settle)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Session  string          `json:"session"`
		Channels []channelReport `json:"channels"`
	}{strings.TrimPrefix(location.Path, "/calls/"), []channelReport{receipts[0].report(), receipts[1].report()}})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
