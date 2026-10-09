// Package callharness is the call harness: the single test seam for Relais
// media. It starts the system in-process, a Pion WebRTC client plays the
// caller, and tests check only what the caller can observe: the SDP answer,
// connection-state changes, the packets it receives and when, whether the
// echoed video decodes, SRTP decryption failures, keyframe requests, and any
// renegotiation or ICE restart.
//
// Today the system is one media worker serving WHIP-style signaling, or
// several workers sharing one UDP socket so that a call can move between
// them (Call.Handover, a planned handover). A test looks like this:
//
//	h, _ := callharness.Start(callharness.Options{})
//	defer h.Close()
//	call, _ := h.Dial(ctx, callharness.CallOptions{Video: true})
//	_ = call.SendMedia(ctx, 30*time.Second)
//	report, _ := call.Hangup(ctx)
//	t.Log(report.Summary())
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
	"time"

	"github.com/pion/logging"
	"github.com/relais/pkg/mediaworker"
)

// Options configures the system the harness starts.
type Options struct {
	// WorkerLoggerFactory is passed to the media workers. Defaults to Pion's
	// default logger factory (PION_LOG_* environment variables).
	WorkerLoggerFactory logging.LoggerFactory

	// Workers is how many media workers run. One (the default) owns its UDP
	// socket. Two or more share one UDP socket, calls start on the first,
	// and Call.Handover moves a call between them.
	Workers int
}

// Harness runs the system in-process: media workers on a loopback UDP
// socket and their signaling endpoint on a loopback HTTP server.
type Harness struct {
	workers      *workers
	server       *http.Server
	serveDone    chan struct{}
	signalingURL string
	httpClient   *http.Client
}

// Start starts the system.
func Start(opts Options) (*Harness, error) {
	workers, err := startWorkers(opts)
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = workers.close()

		return nil, fmt.Errorf("callharness: listen for signaling: %w", err)
	}

	h := &Harness{
		workers: workers,
		server: &http.Server{
			Handler:           workers.signaling,
			ReadHeaderTimeout: 5 * time.Second,
		},
		serveDone:    make(chan struct{}),
		signalingURL: "http://" + listener.Addr().String() + mediaworker.CallsPath,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
	go func() {
		defer close(h.serveDone)
		_ = h.server.Serve(listener)
	}()

	return h, nil
}

// SignalingURL is where a caller POSTs its SDP offer.
func (h *Harness) SignalingURL() string {
	return h.signalingURL
}

// ExchangeOffer posts an SDP offer the way a browser would, for example one
// captured from Chrome, and returns what the answer says. It then hangs the
// call up again; no media flows.
func (h *Harness) ExchangeOffer(ctx context.Context, offer string) (AnswerFacts, error) {
	answer, resourceURL, err := h.postOffer(ctx, offer)
	if err != nil {
		return AnswerFacts{}, err
	}
	facts, parseErr := parseAnswer(answer)

	return facts, errors.Join(parseErr, h.deleteCall(ctx, resourceURL))
}

// Close stops the signaling server and the media workers.
func (h *Harness) Close() error {
	serverErr := h.server.Close()
	<-h.serveDone

	return errors.Join(serverErr, h.workers.close())
}

// postOffer makes the one signaling exchange that starts a call: it POSTs
// the offer and returns the answer and the call's resource URL (from
// Location).
func (h *Harness) postOffer(ctx context.Context, offer string) (answer, resourceURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.signalingURL, strings.NewReader(offer))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("callharness: POST offer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("callharness: read answer: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("callharness: POST offer: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/sdp") {
		return "", "", fmt.Errorf("callharness: answer has Content-Type %q", ct)
	}

	if location := resp.Header.Get("Location"); location != "" {
		base, err := url.Parse(h.signalingURL)
		if err != nil {
			return "", "", err
		}
		ref, err := url.Parse(location)
		if err != nil {
			return "", "", fmt.Errorf("callharness: bad Location %q: %w", location, err)
		}
		resourceURL = base.ResolveReference(ref).String()
	}

	return string(body), resourceURL, nil
}

// deleteCall hangs up a call with the WHIP-style DELETE of its resource.
func (h *Harness) deleteCall(ctx context.Context, resourceURL string) error {
	if resourceURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, resourceURL, nil)
	if err != nil {
		return err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("callharness: DELETE call: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("callharness: DELETE call: %s", resp.Status)
	}

	return nil
}
