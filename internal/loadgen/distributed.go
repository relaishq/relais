package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
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
	bursts  *eventBursts
	stop    func()
}

const (
	backgroundProbeEvery = 10 * time.Second
	burstProbeEvery      = 250 * time.Millisecond
	// An event takes at most burstProbeLimit probes before its receipt, then
	// burstTrailingProbes more, so drift from the nearest anchor stays small.
	burstProbeLimit     = 60
	burstTrailingProbes = 3
)

// eventBursts holds the probe budget of each armed event. One probe counts
// toward every active event, so concurrent events share a single cadence.
type eventBursts struct {
	mu sync.Mutex
	// Probes still due per accepted arm (each call caps its arms). Finished
	// entries stay at zero so a repeated arm cannot start a second burst.
	due  map[callharness.OperationIdentity]int
	wake chan struct{}
}

func newEventBursts() *eventBursts {
	return &eventBursts{due: map[callharness.OperationIdentity]int{}, wake: make(chan struct{}, 1)}
}

func (b *eventBursts) arm(id callharness.OperationIdentity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.due[id]; ok {
		return
	}
	b.due[id] = burstProbeLimit + burstTrailingProbes
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// received ends the event's main phase; only the trailing probes remain.
func (b *eventBursts) received(id callharness.OperationIdentity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if due, ok := b.due[id]; ok {
		b.due[id] = min(due, burstTrailingProbes)
	}
}

func (b *eventBursts) active() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, due := range b.due {
		if due > 0 {
			return true
		}
	}
	return false
}

// probed charges one probe attempt to every active event. It reports whether
// any event needs probes after this one.
func (b *eventBursts) probed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	more := false
	for id, due := range b.due {
		if due > 0 {
			b.due[id] = due - 1
			more = more || due > 1
		}
	}
	return more
}

// runClockSampler is the only probe caller, so tracker probes stay in send
// order. A slow probe delays the next tick; tickers drop missed ticks.
func runClockSampler(ctx context.Context, sample func() error, bursts *eventBursts) {
	background := time.NewTicker(backgroundProbeEvery)
	defer background.Stop()
	var burst *time.Ticker
	var burstTick <-chan time.Time
	defer func() {
		if burst != nil {
			burst.Stop()
		}
	}()
	// Charge before sending: a probe already in flight when a receipt
	// arrives is not one of its trailing probes.
	probe := func() {
		more := bursts.probed()
		_ = sample()
		if !more && burst != nil {
			burst.Stop()
			burst, burstTick = nil, nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-background.C:
			_ = sample()
		case <-bursts.wake:
			if burst == nil && bursts.active() {
				burst = time.NewTicker(burstProbeEvery)
				burstTick = burst.C
				probe()
			}
		case <-burstTick:
			probe()
		}
	}
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
	bursts := newEventBursts()
	go func() {
		defer close(done)
		runClockSampler(lifetime, sample, bursts)
	}()
	return &distributedClock{options: &callharness.DistributedOptions{RunID: config.RunID, Clock: tracker, ControlOffset: callharness.OffsetInterval{Lower: -config.FrontendControlBound, Upper: config.FrontendControlBound}, WorkerOffset: callharness.OffsetInterval{Lower: -config.FrontendWorkerBound, Upper: config.FrontendWorkerBound}}, bursts: bursts, stop: func() { cancel(); <-done }}, nil
}

func registerDistributedFeed(mux *http.ServeMux, calls []*callharness.Call, bursts *eventBursts) {
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
			return
		}
		bursts.arm(id)
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
			err := c.RecordDistributedEvent(e)
			if err == nil {
				bursts.received(e.Identity)
			}
			all = append(all, err)
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
