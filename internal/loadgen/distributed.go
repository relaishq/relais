package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
)

// DistributedConfig is intentionally host/provider independent. The two
// bounds are absolute host-minus-frontend errors, measured by the operator.
type DistributedConfig struct {
	RunID                string
	FrontendControlBound time.Duration
	FrontendWorkerBound  time.Duration
	DriftPPM             float64
}

func eventRetention(configured time.Duration, distributed bool) (time.Duration, error) {
	if configured < 0 {
		return 0, errors.New("negative event retention")
	}
	if configured == 0 {
		if distributed {
			return 180 * time.Second, nil
		}
		return 15 * time.Second, nil
	}
	if configured < 9*time.Second || distributed && configured < 180*time.Second {
		return 0, errors.New("event retention needs >=9s, or >=180s in distributed mode")
	}
	return configured, nil
}

type distributedClock struct {
	options *callharness.DistributedOptions
	stop    func()
}

func startDistributedClock(ctx context.Context, signaling string, client *http.Client, config DistributedConfig) (*distributedClock, error) {
	if config.RunID == "" || config.FrontendControlBound < 0 || config.FrontendWorkerBound < 0 {
		return nil, errors.New("distributed mode needs run ID and nonnegative host clock bounds")
	}
	tracker, err := callharness.NewClockTracker(config.DriftPPM)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSuffix(signaling, "/calls") + "/clock"
	sample := func() error { return sampleDistributedClock(ctx, tracker, client, endpoint) }
	if err := sample(); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-tick.C:
				_ = sample()
			}
		}
	}()
	return &distributedClock{options: &callharness.DistributedOptions{RunID: config.RunID, Clock: tracker, ControlOffset: callharness.OffsetInterval{Lower: -config.FrontendControlBound, Upper: config.FrontendControlBound}, WorkerOffset: callharness.OffsetInterval{Lower: -config.FrontendWorkerBound, Upper: config.FrontendWorkerBound}}, stop: func() { cancel(); <-done }}, nil
}

func registerDistributedFeed(mux *http.ServeMux, calls []*callharness.Call) {
	find := func(id string) *callharness.Call {
		for _, c := range calls {
			if c.SessionID() == id {
				return c
			}
		}
		return nil
	}
	mux.HandleFunc("POST /arm-event", func(w http.ResponseWriter, r *http.Request) {
		var id callharness.OperationIdentity
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&id) != nil {
			http.Error(w, "invalid identity", http.StatusBadRequest)
			return
		}
		c := find(id.SessionID)
		if c == nil {
			http.Error(w, "unknown session", http.StatusBadRequest)
			return
		}
		if err := c.ArmDistributedEvent(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})
	mux.HandleFunc("POST /record-events", func(w http.ResponseWriter, r *http.Request) {
		var events []callharness.DistributedEvent
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&events) != nil {
			http.Error(w, "invalid events", http.StatusBadRequest)
			return
		}
		// Attempt every record: one failed operation must not drop later evidence.
		var all []error
		for _, e := range events {
			c := find(e.Identity.SessionID)
			if c == nil {
				all = append(all, errors.New("unknown session"))
				continue
			}
			all = append(all, c.RecordDistributedEvent(e))
		}
		if err := errors.Join(all...); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /clock-probes", func(w http.ResponseWriter, _ *http.Request) {
		if len(calls) == 0 {
			return
		}
		_ = json.NewEncoder(w).Encode(calls[0].ClockProbes())
	})
}

// A missing transport sample leaves prior probes and their drift widening
// intact. Only a completed probe carrying invalid timestamps poisons trust.
func sampleDistributedClock(ctx context.Context, tracker *callharness.ClockTracker, client *http.Client, endpoint string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := callharness.ProbeClock(probeCtx, client, endpoint)
	if err != nil {
		if errors.Is(err, callharness.ErrClockEvidence) {
			tracker.Invalidate(err)
		}
		return err
	}
	return tracker.Add(p)
}
