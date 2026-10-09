// Package callharness is the call harness: the single test seam for Relais
// media. It starts the system in-process, a Pion WebRTC client plays the
// caller, and tests check only what the caller can observe: the SDP answer,
// connection-state changes, the packets it receives and when, SRTP
// decryption failures, and any renegotiation or ICE restart.
//
// Today the system is one media worker serving WHIP-style signaling. A test
// looks like this:
//
//	h, _ := callharness.Start(callharness.Options{})
//	defer h.Close()
//	call, _ := h.Dial(ctx)
//	_ = call.SendAudio(ctx, 30*time.Second)
//	report, _ := call.Hangup(ctx)
//	t.Log(report.Summary())
package callharness

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/pion/logging"
	"github.com/relais/pkg/mediaworker"
)

// Options configures the system the harness starts.
type Options struct {
	// WorkerLoggerFactory is passed to the media worker. Defaults to Pion's
	// default logger factory (PION_LOG_* environment variables).
	WorkerLoggerFactory logging.LoggerFactory
}

// Harness runs the system in-process: a media worker on a loopback UDP
// socket and its signaling endpoint on a loopback HTTP server.
type Harness struct {
	worker       *mediaworker.Worker
	server       *http.Server
	serveDone    chan struct{}
	signalingURL string
	httpClient   *http.Client
}

// Start starts the system.
func Start(opts Options) (*Harness, error) {
	worker, err := mediaworker.New(mediaworker.Config{
		ListenAddr:    "127.0.0.1:0",
		LoggerFactory: opts.WorkerLoggerFactory,
	})
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = worker.Close()

		return nil, fmt.Errorf("callharness: listen for signaling: %w", err)
	}

	h := &Harness{
		worker: worker,
		server: &http.Server{
			Handler:           worker.SignalingHandler(),
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

// Close stops the signaling server and the media worker.
func (h *Harness) Close() error {
	serverErr := h.server.Close()
	<-h.serveDone

	return errors.Join(serverErr, h.worker.Close())
}
