package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/relais/internal/clusterprocess"
	"github.com/relais/internal/processrun"
	"github.com/relais/pkg/callharness"
)

type ChildOptions struct {
	Topology              callharness.ExternalTopology
	Callers               int
	Duration, SampleEvery time.Duration
	Video                 bool
	Output                string
}
type StartRequest struct {
	At time.Time `json:"at"`
}
type KillRequest struct {
	Worker string    `json:"worker"`
	At     time.Time `json:"at"`
}
type MoveRequest struct {
	Sessions []string `json:"sessions"`
	To       int      `json:"to"`
}

// RunChild exposes only a loopback operator endpoint. Topology injection keeps
// signaling separate from media and accepts #36's setns socket factory.
func RunChild(ctx context.Context, o ChildOptions) error {
	if o.Callers < 1 || o.Duration <= 0 || o.SampleEvery <= 0 {
		return errors.New("invalid child counts or durations")
	}
	h, err := callharness.Start(callharness.Options{External: &o.Topology})
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	output, err := os.Create(o.Output)
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	writer := NewWriter(io.MultiWriter(output, os.Stdout))
	meter := &Meter{}
	calls := make([]*callharness.Call, o.Callers)
	// Bounded setup concurrency prevents signaling setup from flooding the server.
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	errs := make(chan error, o.Callers)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
			defer func() { <-slots }()
			dial, stop := context.WithTimeout(ctx, 20*time.Second)
			defer stop()
			call, err := h.Dial(dial, callharness.CallOptions{Video: o.Video, Worker: -1, Recording: callharness.RecordingOptions{History: 15 * time.Second, DecodeEvery: 30}, SendTiming: meter.Observe})
			if err != nil {
				errs <- fmt.Errorf("dial %d: %w", i, err)
				return
			}
			calls[i] = call
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	address, err := clusterprocess.FreeTCP()
	if err != nil {
		return err
	}
	listener, err := processrun.ListenPrivate(address)
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	start := make(chan time.Time, 1)
	var startMu sync.Mutex
	started := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) {
		ids := make([]string, len(calls))
		for i, c := range calls {
			ids[i] = c.SessionID()
		}
		_ = json.NewEncoder(w).Encode(ids)
	})
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		var req StartRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.At.IsZero() {
			http.Error(w, "invalid start", http.StatusBadRequest)
			return
		}
		startMu.Lock()
		defer startMu.Unlock()
		if started {
			http.Error(w, "already started", http.StatusConflict)
			return
		}
		started = true
		start <- req.At
	})
	mux.HandleFunc("POST /kill", func(w http.ResponseWriter, r *http.Request) {
		var req KillRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.At.IsZero() {
			http.Error(w, "invalid kill", http.StatusBadRequest)
			return
		}
		h.RecordProcessKill(req.Worker, req.At)
	})
	mux.HandleFunc("POST /move", func(w http.ResponseWriter, r *http.Request) {
		var req MoveRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "invalid move", http.StatusBadRequest)
			return
		}
		for _, id := range req.Sessions {
			found := false
			for _, c := range calls {
				if c.SessionID() == id {
					found = true
					if err := c.MoveExternal(r.Context(), req.To); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					break
				}
			}
			if !found {
				http.Error(w, "unknown session", http.StatusBadRequest)
				return
			}
		}
	})
	served := make(chan error, 1)
	go func() { served <- processrun.Serve(lifetime, listener, mux) }()
	defer func() { cancel(); <-served }()
	processrun.Ready(map[string]any{"http": "http://" + listener.Addr().String(), "type": "ready", "callers": len(calls)})
	var at time.Time
	select {
	case at = <-start:
	case <-ctx.Done():
		return ctx.Err()
	}
	timer := time.NewTimer(max(time.Until(at), 0))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	sampler := &Sampler{}
	if err := sampler.Reset(); err != nil {
		return err
	}
	finished := make(chan struct{})
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendErr := c.SendMedia(lifetime, o.Duration)
			hangup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			rep, hangErr := c.Hangup(hangup)
			m := Message{Type: "call", PID: os.Getpid(), CallID: i, Session: c.SessionID(), Call: rep}
			if err := errors.Join(sendErr, hangErr); err != nil {
				m.Error = err.Error()
			}
			writer.Write(m)
		}()
	}
	go func() { wg.Wait(); close(finished) }()
	tick := time.NewTicker(o.SampleEvery)
	defer tick.Stop()
	sample := func() error {
		stats, err := sampler.Sample(meter)
		if err != nil {
			return err
		}
		writer.Write(Message{Type: "stats", PID: os.Getpid(), Stats: &stats})
		return writer.Err()
	}
	var sampleErr error
running:
	for {
		select {
		case <-tick.C:
			if err := sample(); err != nil {
				sampleErr = err
				cancel()
				<-finished
				break running
			}
		case <-finished:
			break running
		}
	}
	sampleErr = errors.Join(sampleErr, sample())
	writer.Write(Message{Type: "done", PID: os.Getpid()})
	return errors.Join(sampleErr, writer.Err())
}
