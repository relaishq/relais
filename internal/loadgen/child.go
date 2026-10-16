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

	"github.com/relaishq/relais/internal/clusterprocess"
	"github.com/relaishq/relais/internal/processrss"
	"github.com/relaishq/relais/internal/processrun"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
)

type ChildOptions struct {
	EventRetention time.Duration
	Distributed    *DistributedConfig

	Soak                  bool
	Topology              callharness.ExternalTopology
	Callers               int
	Duration, SampleEvery time.Duration
	Video                 bool
	Output                string
	ParentPID             int
	StartTimeout          time.Duration
	PlaceOnWorker         bool
	// Desync gives each call a seeded media start offset and maintenance
	// phase, and this process a seeded sampling phase (#85).
	Desync Desync
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

// dialTimeout bounds each dial. It is longer than the caller's ICE checking
// (about 10 s), so a short UDP stall at call start can still connect, and a
// server that never answers fails here.
const dialTimeout = 20 * time.Second

// dialFailures returns every failed dial, not only the first, with the kind
// of each failure.
func dialFailures(errs []error) error {
	var failed []error
	for i, err := range errs {
		if err == nil {
			continue
		}
		var failure *callharness.DialError
		if errors.As(err, &failure) {
			err = fmt.Errorf("dial %d (%s): %w", i, failure.Report.Failure, err)
		} else {
			err = fmt.Errorf("dial %d: %w", i, err)
		}
		failed = append(failed, err)
	}
	if len(failed) == 0 {
		return nil
	}

	return fmt.Errorf("%d of %d dials failed:\n%w", len(failed), len(errs), errors.Join(failed...))
}

// RunChild exposes only a loopback operator endpoint. Topology injection keeps
// signaling separate from media and accepts #36's setns socket factory.
func RunChild(ctx context.Context, o ChildOptions) error {
	if o.Soak {
		if err := processrss.Check(); err != nil {
			return fmt.Errorf("soak memory sampling unavailable: %w", err)
		}
	}
	if o.Callers < 1 || o.Duration <= 0 || o.SampleEvery <= 0 {
		return errors.New("invalid child counts or durations")
	}
	history, err := eventRetention(o.EventRetention, o.Distributed != nil)
	if err != nil {
		return err
	}
	if o.Distributed != nil {
		clock, err := startDistributedClock(ctx, o.Topology.SignalingURL, o.Topology.HTTPClient, *o.Distributed)
		if err != nil {
			return err
		}
		defer clock.stop()
		o.Topology.Distributed = clock.options
	}
	if o.ParentPID == 0 {
		o.ParentPID = os.Getppid()
	}
	ctx, stopParent := watchParent(ctx, o.ParentPID, 250*time.Millisecond, os.Getppid)
	defer stopParent()
	if o.StartTimeout == 0 {
		o.StartTimeout = 3 * time.Minute
	}
	meter := &Meter{}
	poller := newStatusPoller(o.Topology.SignalingURL, meter, o.Topology.HTTPClient)
	o.Topology.Status = poller.Get
	o.Topology.FinalStatus = poller.GetFinal
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
	calls := make([]*callharness.Call, o.Callers)
	// Bounded setup concurrency prevents signaling setup from flooding the server.
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	errs := make([]error, o.Callers)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every dial reports its outcome as it ends, connected or not.
			m := Message{Type: "dial", PID: os.Getpid(), CallID: i}
			defer func() { writer.Write(m) }()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				errs[i], m.Error = ctx.Err(), ctx.Err().Error()
				return
			}
			defer func() { <-slots }()
			dial, stop := context.WithTimeout(ctx, dialTimeout)
			defer stop()
			worker := -1
			if o.PlaceOnWorker {
				worker = 0
			}
			recording := callharness.RecordingOptions{History: history, DecodeEvery: 30, MaintenancePhase: o.Desync.Maintenance(i)}
			call, err := h.Dial(dial, callharness.CallOptions{Video: o.Video, Worker: worker, Recording: recording, SendTiming: meter.Observe, SendPacing: meter.ObservePacing})
			if err != nil {
				errs[i], m.Error = err, err.Error()
				var failure *callharness.DialError
				if errors.As(err, &failure) {
					m.Session, m.Dial = failure.Report.Session, &failure.Report
				}
				return
			}
			calls[i] = call
			report := call.DialReport()
			m.Session, m.Dial = call.SessionID(), &report
		}()
	}
	wg.Wait()
	if err := dialFailures(errs); err != nil {
		return err
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
	if o.Distributed != nil {
		registerDistributedFeed(mux, calls)
	}
	if o.Soak {
		registerMemoryDiagnostics(mux, calls)
	}
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
		if o.Distributed != nil {
			http.Error(w, "use identity-bearing /record-events", http.StatusBadRequest)
			return
		}
		var req KillRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.At.IsZero() {
			http.Error(w, "invalid kill", http.StatusBadRequest)
			return
		}
		h.RecordProcessKill(req.Worker, req.At)
	})
	mux.HandleFunc("POST /move", func(w http.ResponseWriter, r *http.Request) {
		if o.Distributed != nil {
			http.Error(w, "use identity-bearing /record-events", http.StatusBadRequest)
			return
		}
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
	mux.HandleFunc("POST /record-moves", func(w http.ResponseWriter, r *http.Request) {
		if o.Distributed != nil {
			http.Error(w, "use identity-bearing /record-events", http.StatusBadRequest)
			return
		}
		var moves []controlplane.MoveResult
		if json.NewDecoder(r.Body).Decode(&moves) != nil {
			http.Error(w, "invalid moves", http.StatusBadRequest)
			return
		}
		for _, move := range moves {
			for _, c := range calls {
				if c.SessionID() == move.ID {
					c.RecordExternalMove(move)
				}
			}
		}
	})
	served := make(chan error, 1)
	go func() { served <- processrun.Serve(lifetime, listener, mux) }()
	defer func() { cancel(); <-served }()
	processrun.Ready(map[string]any{"http": "http://" + listener.Addr().String(), "type": "ready", "callers": len(calls)})
	if err := waitForStart(ctx, start, o.StartTimeout); err != nil {
		return err
	}
	// Lifetime request totals include setup; interval rates start with media.
	meter.sample()
	sampler := &Sampler{CurrentMemory: o.Soak}
	if err := sampler.Reset(); err != nil {
		return err
	}
	finished := make(chan struct{})
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Real callers do not start together. Each call starts media at
			// its own seeded offset in [0, 1s) after the common start, then
			// sends for the whole duration, so its frame ticks and keyframes
			// keep that phase and it ends up to one second after the others.
			offset := o.Desync.MediaStart(i)
			sendErr := sendAfter(lifetime, offset, o.Duration, c.SendMedia)
			// Allow the 10s final status read plus echo drain and deletion.
			hangup, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			rep, hangErr := c.Hangup(hangup)
			m := Message{Type: "call", PID: os.Getpid(), CallID: i, Session: c.SessionID(), Call: rep, MediaOffset: offset, MaintenancePhase: o.Desync.Maintenance(i)}
			if err := errors.Join(sendErr, hangErr); err != nil {
				m.Error = err.Error()
			}
			writer.Write(m)
		}()
	}
	go func() { wg.Wait(); close(finished) }()
	// Generator processes sample on their own seeded phases, so they do not
	// all read CPU and memory on the same instant. Only the first interval is
	// longer (SampleEvery plus the phase); later samples keep SampleEvery.
	first := time.NewTimer(o.SampleEvery + o.Desync.SamplePhase(o.SampleEvery))
	defer first.Stop()
	ticks := first.C
	var tick *time.Ticker
	defer func() {
		if tick != nil {
			tick.Stop()
		}
	}()
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
		case <-ticks:
			if tick == nil {
				tick = time.NewTicker(o.SampleEvery)
				ticks = tick.C
			}
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
