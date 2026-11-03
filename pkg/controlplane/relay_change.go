package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/relaytrace"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
)

var ErrRelayRolledBack = errors.New("controlplane: relay_change_rolled_back")

type RelayBinding struct {
	Slot           mediaworker.RelaySlot
	Relay          Relay
	draining       bool
	lost           bool
	lostAt         time.Time
	lossUnaffected map[string]bool // guarded by Plane.mu
}
type relayChangeWorker interface {
	ConfigureRelays(context.Context, []mediaworker.RelaySlot) error
	CreateSessionAtRelay(context.Context, string, string) (string, string, sessionstore.Lease, error)
	AbortRelayRestart(context.Context, string, uint64) error
	RestartSession(context.Context, string, mediaworker.RestartRequest) (string, error)
}
type RelayEvent struct {
	Generation uint64    `json:"generation"`
	Deadline   time.Time `json:"deadline"`
	Reason     string    `json:"reason,omitempty"`
}

// ConfigureRelaySet is a bootstrap operation. Existing one-relay construction
// and placement remain unchanged; relay changes require a set of at least two.
func (p *Plane) ConfigureRelaySet(ctx context.Context, bindings []RelayBinding) error {
	p.relayPlacement.Lock()
	defer p.relayPlacement.Unlock()
	if _, ok := p.store.(sessionstore.RelayAssignments); !ok || len(bindings) < 2 {
		return mediaworker.ErrRelayUnsupported
	}
	p.mu.Lock()
	if len(p.calls) != 0 {
		p.mu.Unlock()
		return ErrMoveInProgress
	}
	var workers []relayChangeWorker
	for _, w := range p.workers {
		worker, ok := w.worker.(relayChangeWorker)
		if !ok {
			p.mu.Unlock()
			return mediaworker.ErrRelayUnsupported
		}
		workers = append(workers, worker)
	}
	p.mu.Unlock()
	slots := make([]mediaworker.RelaySlot, 0, len(bindings))
	next := make(map[string]RelayBinding, len(bindings))
	for _, b := range bindings {
		if b.Relay == nil || b.Slot.ID == "" || next[b.Slot.ID].Relay != nil {
			return mediaworker.ErrRelayUnsupported
		}
		next[b.Slot.ID] = b
		slots = append(slots, b.Slot)
	}
	for _, w := range workers {
		if err := w.ConfigureRelays(ctx, slots); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.relaySet = next
	p.mu.Unlock()
	return nil
}

// Registration and loss marking serialize through relayPlacement. Publish a
// fresh worker only after it accepts the current view, including prior losses.
func (p *Plane) configureWorkerRelays(ctx context.Context, worker Worker) error {
	p.mu.Lock()
	enabled := len(p.relaySet) > 0
	var slots []mediaworker.RelaySlot
	for _, binding := range p.relaySet {
		if !binding.lost {
			slots = append(slots, binding.Slot)
		}
	}
	p.mu.Unlock()
	if !enabled {
		return nil
	}
	w, ok := worker.(relayChangeWorker)
	if !ok {
		return mediaworker.ErrRelayUnsupported
	}
	return w.ConfigureRelays(ctx, slots)
}

func (p *Plane) ChangeRelay(ctx context.Context, id, target string) error {
	if err := p.requireMutationReady(); err != nil {
		return err
	}
	c, err := p.lookup(id)
	if err != nil {
		return err
	}
	// Request cancellation starts recovery; takeover cancellation stops it.
	// Publish the detached recovery cancellation with ownership of the call.
	recoveryBase, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ctx, cancelRequest := context.WithCancel(ctx)
	stop := context.AfterFunc(recoveryBase, cancelRequest)
	defer func() { stop(); cancelRequest() }()
	if err = c.lockOperatorCancel(ctx, false, cancel); err != nil {
		cancel()
		return err
	}
	defer func() {
		c.stateMu.Lock()
		c.relayEvent = nil
		c.stateMu.Unlock()
		c.controlMu.Lock()
		c.relayCancel = nil
		c.mu.Unlock()
		c.controlMu.Unlock()
		cancel()
	}()
	c.stateMu.Lock()
	capable := c.relayCapable
	c.stateMu.Unlock()
	p.mu.Lock()
	b, known := p.relaySet[target]
	p.mu.Unlock()
	if !capable || !known || b.draining || b.lost {
		return mediaworker.ErrRelayUnsupported
	}
	a := p.store.(sessionstore.RelayAssignments)
	lease, from, err := a.RelayOwner(ctx, id)
	if err != nil {
		return err
	}
	if from.Pending != "" {
		// A previous ambiguous abort may now be acknowledged. Never clear a path
		// that the worker has already switched; its commit must settle first.
		from, err = p.abortRelayPending(ctx, lease, from)
		if err != nil {
			return err
		}
	}
	if from.Slot == b.Slot.ID {
		return sessionstore.ErrRelayConflict
	}
	pending := from
	pending.Pending = target
	if err = p.setRelay(ctx, lease, from, pending); err != nil {
		return errors.Join(err, p.cleanupRelayPending(ctx, lease, pending))
	}
	err = p.waitRelayChange(ctx, c, pending, 10*time.Second)
	cleanup := from.Slot
	if err != nil {
		if recoveryBase.Err() != nil {
			return errors.Join(err, p.cleanupRelayPending(ctx, lease, pending))
		}
		recovery, cancel := context.WithTimeout(recoveryBase, sessionstore.StoreOutageBudget(0))
		defer cancel()
		rolled, abortErr := p.abortRelayPending(recovery, lease, pending)

		if errors.Is(abortErr, mediaworker.ErrRelaySwitched) {
			// A switched worker retries its fenced commit. Never send it back.
			err = p.waitRelayChange(recovery, c, pending, sessionstore.StoreOutageBudget(0))
			if err != nil {
				return err
			}
			ctx = recovery
		} else {
			if abortErr != nil {
				return errors.Join(err, abortErr, p.cleanupRelayPending(ctx, lease, pending))
			}
			pending = rolled
			pending.Pending = from.Slot // a new restart on the still-live old slot
			if e := p.setRelay(recovery, lease, rolled, pending); e != nil {
				return errors.Join(err, e, p.cleanupRelayPending(ctx, lease, pending))
			}
			if e := p.waitRelayChange(recovery, c, pending, 10*time.Second); e != nil {
				return errors.Join(err, e, p.cleanupRelayPending(ctx, lease, pending))
			}
			cleanup = target
			ctx = recovery
			err = errors.Join(ErrRelayRolledBack, err)
		}
	}
	p.mu.Lock()
	old := p.relaySet[cleanup].Relay
	p.mu.Unlock()
	if r, ok := old.(interface {
		ForgetSessionTenure(context.Context, sessionstore.Lease) error
	}); ok {
		return errors.Join(err, r.ForgetSessionTenure(ctx, lease))
	}
	return err
}

// Cancellation cleanup leaves time for takeover inside its original budget.
// Only an acknowledged abort (or a definite absent/fenced session) permits
// clearing an assignment under the old epoch. Uncertain RPC failures do not.
func (p *Plane) cleanupRelayPending(ctx context.Context, lease sessionstore.Lease, pending sessionstore.RelayAssignment) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), takeoverBudget/3)
	defer cancel()
	_, err := p.abortRelayPending(cleanup, lease, pending)
	return err
}
func (p *Plane) abortRelayPending(ctx context.Context, lease sessionstore.Lease, pending sessionstore.RelayAssignment) (sessionstore.RelayAssignment, error) {
	a := p.store.(sessionstore.RelayAssignments)
	_, current, err := a.RelayOwner(ctx, lease.SessionID)
	if err != nil {
		return current, err
	}
	if current != pending {
		if current.Pending == "" && current.Slot == pending.Pending && current.Generation >= pending.Generation+1 {
			return current, mediaworker.ErrRelaySwitched
		}
		if current.Pending == "" {
			return current, nil
		}
		return current, sessionstore.ErrRelayConflict
	}
	p.mu.Lock()
	owner := p.owner(lease.Worker)
	p.mu.Unlock()
	if owner == nil {
		return pending, ErrNoTarget
	}
	err = owner.worker.(relayChangeWorker).AbortRelayRestart(ctx, lease.SessionID, pending.Generation+1)
	if err != nil && !errors.Is(err, mediaworker.ErrUnknownSession) && !errors.Is(err, mediaworker.ErrClosed) && !errors.Is(err, sessionstore.ErrLeaseLost) {
		return pending, err
	}
	rolled := sessionstore.RelayAssignment{Slot: pending.Slot, Generation: pending.Generation + 1}
	if e := p.setRelay(ctx, lease, pending, rolled); e != nil {
		return pending, errors.Join(err, e)
	}
	return rolled, err
}

// Transfer has fenced the old owner before an unacknowledged/dead-owner
// generation is retired. A resumed checkpoint never contains pending creds.
func (p *Plane) settleDeadRelayChange(ctx context.Context, lease sessionstore.Lease) error {
	p.mu.Lock()
	enabled := len(p.relaySet) > 0
	p.mu.Unlock()
	if !enabled {
		return nil
	}
	_, a, err := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, lease.SessionID)
	if err != nil {
		return err
	}
	if a.Pending == "" {
		return nil
	}
	return p.setRelay(ctx, lease, a, sessionstore.RelayAssignment{Slot: a.Slot, Generation: a.Generation + 1})
}

func (p *Plane) setRelay(ctx context.Context, lease sessionstore.Lease, from, to sessionstore.RelayAssignment) error {
	a := p.store.(sessionstore.RelayAssignments)
	for {
		err := a.CASRelay(ctx, lease, from, to)
		if err == nil {
			return nil
		}
		_, got, e := a.RelayOwner(ctx, lease.SessionID)
		if e == nil && got == to {
			return nil
		}
		if errors.Is(err, sessionstore.ErrLeaseLost) || errors.Is(err, sessionstore.ErrRelayTransition) || e == nil && got != from {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (p *Plane) waitRelayChange(ctx context.Context, c *call, pending sessionstore.RelayAssignment, bound time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	deadline, _ := ctx.Deadline()
	c.stateMu.Lock()
	c.relayEvent = &RelayEvent{Generation: pending.Generation + 1, Deadline: deadline}
	c.relayRejected = false
	select {
	case c.relayWake <- struct{}{}:
	default:
	}
	c.stateMu.Unlock()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		c.stateMu.Lock()
		rejected := c.relayRejected
		c.stateMu.Unlock()
		if rejected {
			return mediaworker.ErrRestartMismatch
		}
		_, got, e := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.id)
		if e == nil && got == (sessionstore.RelayAssignment{Slot: pending.Pending, Generation: pending.Generation + 1}) {
			c.stateMu.Lock()
			c.relayEvent = nil
			c.relaySlot = got.Slot
			c.stateMu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (p *Plane) relayHandlers(mux *http.ServeMux) {
	mux.HandleFunc("POST /relays/{id}/drain", func(w http.ResponseWriter, r *http.Request) {
		if err := p.DrainRelay(r.Context(), r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /calls/{id}/relay", func(w http.ResponseWriter, r *http.Request) {
		if err := p.ChangeRelay(r.Context(), r.PathValue("id"), r.URL.Query().Get("to")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /calls/{id}/ice-restart", func(w http.ResponseWriter, r *http.Request) {
		c, err := p.lookup(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		var g uint64
		if r.URL.Query().Get("generation") == "" {
			event, e := p.callerRelayRestart(r.Context(), c)
			if e != nil {
				if errors.Is(e, mediaworker.ErrRelayUnsupported) {
					http.Error(w, "invalid generation", 400)
					return
				}
				writeError(w, e)
				return
			}
			g = event.Generation
		} else {
			g, err = strconv.ParseUint(r.URL.Query().Get("generation"), 10, 64)
		}
		if err != nil {
			http.Error(w, "invalid generation", 400)
			return
		}
		c.stateMu.Lock()
		event := c.relayEvent
		c.stateMu.Unlock()
		if event == nil || event.Generation != g || time.Now().After(event.Deadline) {
			privateapi.Error(w, sessionstore.ErrRelayConflict, RemoteErrors)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "invalid offer", 400)
			return
		}
		lease, a, err := p.store.(sessionstore.RelayAssignments).RelayOwner(r.Context(), c.id)
		if err != nil {
			writeError(w, err)
			return
		}
		p.mu.Lock()
		owner := p.owner(lease.Worker)
		p.mu.Unlock()
		if owner == nil {
			writeError(w, ErrNoTarget)
			return
		}
		worker, ok := owner.worker.(relayChangeWorker)
		if !ok {
			writeError(w, mediaworker.ErrRelayUnsupported)
			return
		}
		answer, err := worker.RestartSession(r.Context(), c.id, mediaworker.RestartRequest{Offer: string(body), Generation: g, Slot: a.Pending})
		if err != nil {
			if errors.Is(err, mediaworker.ErrRestartMismatch) {
				c.stateMu.Lock()
				if c.relayEvent == event { // a late refusal cannot reject a newer generation
					c.relayRejected = true
				}
				c.stateMu.Unlock()
			}
			privateapi.Error(w, err, RemoteErrors)
			return
		}
		w.Header().Set("Content-Type", "application/sdp")
		_, _ = io.WriteString(w, answer)
	})
	mux.HandleFunc("GET /calls/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		c, err := p.lookup(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		c.stateMu.Lock()
		if !c.relayCapable || c.eventOpen {
			c.stateMu.Unlock()
			http.Error(w, "event stream unavailable", http.StatusConflict)
			return
		}
		c.eventOpen = true
		wake := make(chan struct{}, 1)
		c.relayWake = wake
		c.stateMu.Unlock()
		defer func() { c.stateMu.Lock(); c.eventOpen = false; c.stateMu.Unlock() }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		flush := http.NewResponseController(w)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		var sent uint64
		first := true
		keepalive := time.Now()
		for {
			c.stateMu.Lock()
			e := c.relayEvent
			c.stateMu.Unlock()
			justSent := e != nil && e.Generation != sent
			if justSent {
				data, _ := json.Marshal(e)
				if _, err = fmt.Fprintf(w, "event: relay-change\ndata: %s\n\n", data); err != nil {
					return
				}
				sent = e.Generation
			}
			p.mu.Lock()
			terminal := c.terminal
			p.mu.Unlock()
			c.stateMu.Lock()
			endedReason := c.relayEndReason
			c.stateMu.Unlock()
			if c.hungUp.Load() || terminal || endedReason != "" {
				c.stateMu.Lock()
				reason := c.relayEndReason
				c.stateMu.Unlock()
				if reason == "" {
					reason = "ended"
				}
				data, _ := json.Marshal(RelayEvent{Reason: reason})
				_, _ = fmt.Fprintf(w, "event: ended\ndata: %s\n\n", data)
				_ = flush.Flush()
				return
			}
			if time.Since(keepalive) >= 5*time.Second || first {
				if _, err = io.WriteString(w, ": keepalive\n\n"); err != nil {
					return
				}
				keepalive = time.Now()
				first = false
			}
			if err = flush.Flush(); err != nil {
				return
			}
			if justSent {
				relaytrace.Emit("event-sent", c.id, nil)
			}
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			case <-wake:
			}
		}
	})
}

func (w *RemoteWorker) ConfigureRelays(ctx context.Context, slots []mediaworker.RelaySlot) error {
	return privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/relays", mediaworker.RelaySetRequest{Slots: slots}, nil, RemoteErrors)
}
func (w *RemoteWorker) RestartSession(ctx context.Context, id string, req mediaworker.RestartRequest) (string, error) {
	var reply mediaworker.RestartReply
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/sessions/"+id+"/restart", req, &reply, RemoteErrors)
	return reply.Answer, err
}

func (w *RemoteWorker) AbortRelayRestart(ctx context.Context, id string, generation uint64) error {
	return privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodDelete, "/sessions/"+id+"/restart?generation="+strconv.FormatUint(generation, 10), nil, nil, RemoteErrors)
}

func (w *RemoteWorker) CreateSessionAtRelay(ctx context.Context, offer, slot string) (string, string, sessionstore.Lease, error) {
	var reply mediaworker.CreateReply
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodPost, "/sessions", mediaworker.CreateRequest{Offer: offer, RelaySlot: slot}, &reply, mediaworker.RemoteErrors)
	return reply.ID, reply.Answer, reply.Lease, err
}

// pickRelay keeps one-slot placement unchanged. Reads happen outside registry
// and packet locks. Known assignments are local placement hints; authorization
// and all relay transitions still use the authoritative store.
func (p *Plane) pickRelay(ctx context.Context, exclude string) (string, error) {
	p.mu.Lock()
	if len(p.relaySet) == 0 {
		p.mu.Unlock()
		return "", nil
	}
	var slots []string
	var calls []*call
	for id, b := range p.relaySet {
		if !b.draining && !b.lost && id != exclude {
			slots = append(slots, id)
		}
	}
	for _, c := range p.calls {
		calls = append(calls, c)
	}
	p.mu.Unlock()
	if len(slots) == 0 {
		return "", ErrNoTarget
	}
	sort.Strings(slots)
	loads := map[string]int{}
	for _, c := range calls {
		c.stateMu.Lock()
		slot := c.relaySlot
		c.stateMu.Unlock()
		if slot != "" {
			loads[slot]++
			continue
		}
		_, a, e := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.id)
		if errors.Is(e, sessionstore.ErrNotFound) {
			continue
		}
		if e != nil {
			return "", e
		}
		c.stateMu.Lock()
		c.relaySlot = a.Slot
		c.stateMu.Unlock()
		loads[a.Slot]++
	}
	best := slots[0]
	for _, id := range slots[1:] {
		if loads[id] < loads[best] {
			best = id
		}
	}
	return best, nil
}

func (p *Plane) DrainRelay(ctx context.Context, slot string) error {
	if err := p.requireMutationReady(); err != nil {
		return err
	}
	p.relayPlacement.Lock()
	p.mu.Lock()
	b, ok := p.relaySet[slot]
	if !ok {
		p.mu.Unlock()
		p.relayPlacement.Unlock()
		return mediaworker.ErrRelayUnsupported
	}
	b.draining = true
	p.relaySet[slot] = b
	ids := make([]string, 0, len(p.calls))
	for id := range p.calls {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	p.relayPlacement.Unlock()
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, 8)
	out := make(chan error, len(ids))
	for _, id := range ids {
		_, a, e := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, id)
		if errors.Is(e, sessionstore.ErrNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		if a.Slot != slot {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			target, e := p.pickRelay(ctx, slot)
			if e == nil {
				e = p.ChangeRelay(ctx, id, target)
			}
			out <- e
		}()
	}
	wg.Wait()
	close(out)
	var err error
	for e := range out {
		err = errors.Join(err, e)
	}
	return err
}
