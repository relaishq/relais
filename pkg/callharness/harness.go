// Package callharness is the call harness: the single test seam for Relais
// media. It starts the system in-process, a Pion WebRTC client plays the
// caller, and tests check only what the caller can observe: the SDP answer,
// connection-state changes, the packets it receives and when, whether the
// echoed video decodes, SRTP decryption failures, keyframe requests, and any
// renegotiation or ICE restart.
//
// Today the system is one media worker serving WHIP-style signaling, or
// several workers sharing one UDP socket so that a call can move between
// them (Call.Handover, a planned handover), or, with Options.Relay, one or
// more workers behind a relay (see topology.go). A test looks like this:
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
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/relais/internal/workerprobe"
	"github.com/relais/pkg/controlplane"
	"github.com/relais/pkg/framecache"
	"github.com/relais/pkg/mediaworker"
	"github.com/relais/pkg/sessionstore"
)

// ExternalTopology connects the real caller to an already running system.
// The harness owns only its callers; it never stops external processes.
type ExternalTopology struct {
	SignalingURL string
	RelayAddr    netip.AddrPort
	// CallerSocket creates the caller's UDP4 media socket. Nil preserves the
	// loopback socket and candidates. A supplied socket must bind a specific
	// IPv4 address; the harness owns and closes it. Signaling stays separate.
	CallerSocket func() (net.PacketConn, error)
}

// Options configures the system the harness starts.
type Options struct {
	// External selects a process topology instead of starting local workers.
	External *ExternalTopology

	// WorkerLoggerFactory is passed to the media workers (and the relay).
	// Defaults to Pion's default logger factory (PION_LOG_* environment
	// variables).
	WorkerLoggerFactory logging.LoggerFactory

	// Workers is how many media workers run. Without Relay, one (the
	// default) owns its UDP socket; two or more share one UDP socket, calls
	// start on the first, and Call.Handover moves a call between them.
	// With Relay, each worker owns a private socket behind the relay, and a
	// call picks its worker with CallOptions.Worker. Call.Handover uses the
	// control plane to transfer ownership and re-point the relay.
	Workers int

	// Relay runs every call through a relay with a session-owner store
	// (Memory by default): answers advertise the relay's public address, and the workers
	// bind only private sockets behind it.
	Relay bool
	// SessionStore selects the relay topology's store. Nil uses Memory.
	// The caller owns its lifetime; it must outlive Harness.Close.
	SessionStore sessionstore.Store
	// FrameCache selects a shared cache, including Redis. Nil uses Memory.
	// The caller owns its lifetime and must keep it open through Harness.Close.
	FrameCache framecache.Store
	// SnapshotInterval sets the worker snapshot cadence for crash tests.
	// TakeoverConfig overrides control-plane bounds for fault scenarios.
	TakeoverConfig   controlplane.Config
	SnapshotInterval time.Duration
	// DisableFrameCache and DisableResumePLI independently select the video
	// recovery paths. Both paths are enabled by default.
	DisableFrameCache bool
	DisableResumePLI  bool
	// ReplayMaxBurstDuration overrides the worker's one-frame replay cap.
	ReplayMaxBurstDuration time.Duration
}

// Harness runs a caller against either local workers and signaling, or an
// externally owned process topology. External mode owns only its callers.
type Harness struct {
	external     *ExternalTopology
	workers      *workers
	server       *http.Server
	serveDone    chan struct{}
	signalingURL string
	httpClient   *http.Client

	// callsMu guards the calls not closed yet and closed (see lifecycle.go).
	callsMu      sync.Mutex
	calls        map[*Call]struct{}
	closed       bool
	disableProbe func()
	failures     map[string][]time.Time
}

// Start starts a local topology, or attaches callers to Options.External.
func Start(opts Options) (*Harness, error) {
	if opts.External != nil {
		if opts.Relay || opts.Workers != 0 || opts.SessionStore != nil || opts.FrameCache != nil {
			return nil, errors.New("callharness: external topology cannot start local workers")
		}
		u, err := url.Parse(opts.External.SignalingURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || !opts.External.RelayAddr.IsValid() {
			return nil, errors.New("callharness: external topology needs a signaling URL and relay address")
		}
		external := *opts.External
		return &Harness{external: &external, workers: &workers{}, failures: make(map[string][]time.Time), signalingURL: external.SignalingURL, httpClient: &http.Client{Timeout: 10 * time.Second}}, nil
	}

	disableProbe := workerprobe.Enable()
	workers, err := startWorkers(opts)
	if err != nil {
		disableProbe()
		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = workers.close()
		disableProbe()

		return nil, fmt.Errorf("callharness: listen for signaling: %w", err)
	}

	h := &Harness{
		workers:      workers,
		disableProbe: disableProbe,
		failures:     make(map[string][]time.Time),
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
	answer, resourceURL, err := h.postOffer(ctx, 0, offer)
	if err != nil {
		return AnswerFacts{}, err
	}
	facts, parseErr := parseAnswer(answer)

	return facts, errors.Join(parseErr, h.deleteCall(ctx, resourceURL))
}

// Close closes any calls still open, then stops the signaling server, the
// media workers and the relay. In external mode it closes callers only.
// Dial fails with ErrHarnessClosed from the moment Close starts.
func (h *Harness) Close() error {
	if h.disableProbe != nil {
		defer h.disableProbe()
	}
	callsErr := h.closeCalls()
	if h.external != nil {
		return callsErr
	}
	serverErr := h.server.Close()
	<-h.serveDone

	return errors.Join(callsErr, serverErr, h.workers.close())
}

// postOffer makes the one signaling exchange that starts a call on a media
// worker: it POSTs the offer and returns the answer and the call's resource
// URL (from Location).
func (h *Harness) postOffer(ctx context.Context, worker int, offer string) (answer, resourceURL string, err error) {
	target, err := h.offerURL(worker)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(offer))
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
