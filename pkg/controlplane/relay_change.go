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

// RelayEvent is one event-stream message (ADR 0016 section 5): relay-change
// carries generation, deadline, required_mids and confirmation; ended carries
// reason and, for relay_change_failed, the evidence detail.
type RelayEvent struct {
	Generation   uint64    `json:"generation,omitempty"`
	Deadline     time.Time `json:"deadline,omitzero"`
	RequiredMIDs []string  `json:"required_mids,omitempty"`
	Confirmation string    `json:"confirmation,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	Slot         string    `json:"slot,omitempty"`
	MissingMIDs  []string  `json:"missing_mids,omitempty"`
}

// ConfigureRelaySet is a bootstrap operation. Existing one-relay construction
// and placement remain unchanged. An explicitly configured set of one slot
// accepts the confirmation contracts and supports same-slot recovery (ADR 0016
// sections 5 and 10); changing to another address needs a second slot.
func (p *Plane) ConfigureRelaySet(ctx context.Context, bindings []RelayBinding) error {
	p.relayPlacement.Lock()
	defer p.relayPlacement.Unlock()
	if _, ok := p.store.(sessionstore.RelayAssignments); !ok || len(bindings) < 1 {
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
	mode, err := p.relayMode(ctx, c)
	if err != nil {
		return err
	}
	capable := mode == sessionstore.RelayConfirmDelivery || mode == sessionstore.RelayConfirmDecode
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
	if confirmations, ok := p.store.(sessionstore.RelayConfirmations); ok {
		// An open recovery episode owns the call's relay outcome.
		if _, rec, e := confirmations.RelayConfirmation(ctx, id); e != nil {
			return e
		} else if rec.Episode.Open() {
			return ErrMoveInProgress
		}
	}
	pending := from
	pending.Pending = target
	if err = p.setRelay(ctx, lease, from, pending); err != nil {
		return errors.Join(err, p.cleanupRelayPending(ctx, lease, pending))
	}
	planned := relayAttemptOptions{kind: "planned", outer: time.Now().Add(relayPlannedLimit)}
	firstOuter := planned.outer // never extended by a fresh rollback window
	err = p.waitRelayChange(ctx, c, pending, relayPlannedLimit, planned)
	cleanup := from.Slot
	if err != nil {
		if recoveryBase.Err() != nil {
			return errors.Join(err, p.cleanupRelayPending(ctx, lease, pending))
		}
		recovery, cancel := context.WithTimeout(recoveryBase, sessionstore.StoreOutageBudget(0)+relayRecoveryLimit)
		defer cancel()
		var current sessionstore.RelayAssignment
		switched := true // the call left its original working path
		switch {
		case errors.Is(err, ErrMoveInProgress), errors.Is(err, errRelayEpisodeEnded):
			// An episode owns the outcome: never abort or roll back under it.
			return err
		case errors.Is(err, ErrConfirmationUnsupported):
			// Never complete on commit alone: the call stays on its slot and
			// is reported as not movable.
			_, abortErr := p.abortRelayPending(recovery, lease, pending)
			return errors.Join(err, abortErr)
		case errors.Is(err, errRelayUnconfirmed):
			current = sessionstore.RelayAssignment{Slot: target, Generation: pending.Generation + 1}
		default:
			rolled, abortErr := p.abortRelayPending(recovery, lease, pending)
			if errors.Is(abortErr, mediaworker.ErrRelaySwitched) {
				// A switched worker retries its fenced commit. Never send it back.
				// The call left its path: arm the episode first, so settlement
				// can never push its end past the first planned deadline + 15 s.
				e, armErr := p.relayRecoveryEpisode(recovery, c, firstOuter)
				if armErr != nil {
					return errors.Join(err, armErr)
				}
				if err = p.waitRelayChange(recovery, c, pending, min(sessionstore.StoreOutageBudget(0), time.Until(e.deadline)), planned); err == nil {
					relayWatchEvent(c, "settlement-confirmed")
					if err = p.completeRelayEpisode(recovery, c, e); err != nil {
						return err // the episode ended at its deadline
					}
					ctx = recovery
					break
				}
				if !errors.Is(err, errRelayUnconfirmed) {
					return err
				}
				current = sessionstore.RelayAssignment{Slot: target, Generation: pending.Generation + 1}
			} else if abortErr != nil {
				return errors.Join(err, abortErr, p.cleanupRelayPending(ctx, lease, pending))
			} else {
				// The attempted generation is consumed before any switch. A
				// caller that dropped its old pair gets one restart on the old
				// slot in a fresh planned window (ADR 0016 section 7).
				current, switched = rolled, false
				planned = relayAttemptOptions{kind: "planned", outer: time.Now().Add(relayPlannedLimit)}
			}
		}
		if current.Slot != "" {
			// Roll back with a higher generation on the still-live old slot,
			// confirmed under the same mode (ADR 0016 section 7).
			if e := p.rollbackRelay(recovery, c, lease, current, from.Slot, planned, firstOuter, switched); e != nil {
				return errors.Join(err, e)
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

// rollbackRelay restarts on the original live slot with higher generations
// until one is confirmed. switched reports that the call already left its
// original working path through a committed but unconfirmed attempt. Only
// when it did not may a rollback that fails before switching simply keep the
// caller's old pair. Otherwise, and when the call is still switched and
// unconfirmed at the planned deadline, it enters a stall episode detected no
// later than the first planned deadline, so the 10 s + 15 s bound holds.
func (p *Plane) rollbackRelay(ctx context.Context, c *call, lease sessionstore.Lease, current sessionstore.RelayAssignment, original string, planned relayAttemptOptions, firstOuter time.Time, switched bool) error {
	var err error
	var episode *relayEpisode
	confirmed := false
	// arm fixes the hard end as soon as the call has left its original path:
	// detection is the first planned deadline, so no wait below (settlement,
	// rollback attempts) can move the end past it + 15 s.
	arm := func() error {
		if episode != nil {
			return nil
		}
		e, armErr := p.relayRecoveryEpisode(ctx, c, firstOuter)
		if armErr != nil {
			return armErr
		}
		episode = e
		return nil
	}
	if switched {
		if err = arm(); err != nil {
			return err
		}
	}
	settle := func() time.Duration {
		if episode == nil {
			return sessionstore.StoreOutageBudget(0)
		}
		return min(sessionstore.StoreOutageBudget(0), time.Until(episode.deadline))
	}
	for time.Now().Before(planned.outer) {
		p.mu.Lock()
		lost := p.relaySet[original].lost
		p.mu.Unlock()
		if lost {
			break
		}
		next := current
		next.Pending = original
		if err = p.setRelay(ctx, lease, current, next); err != nil {
			err = errors.Join(err, p.cleanupRelayPending(ctx, lease, next))
			break
		}
		err = p.waitRelayChange(ctx, c, next, time.Until(planned.outer), planned)
		if err == nil {
			confirmed = true
			break
		}
		if errors.Is(err, errRelayUnconfirmed) {
			current, switched = sessionstore.RelayAssignment{Slot: original, Generation: next.Generation + 1}, true
			if err = arm(); err != nil {
				return err
			}
			continue
		}
		if errors.Is(err, ErrConfirmationUnsupported) {
			err = errors.Join(err, p.cleanupRelayPending(ctx, lease, next))
			break
		}
		_, abortErr := p.abortRelayPending(ctx, lease, next)
		if errors.Is(abortErr, mediaworker.ErrRelaySwitched) {
			// The worker switched: arm the episode before waiting for its
			// fenced commit, and never wait past the episode deadline.
			switched = true
			if err = arm(); err != nil {
				return err
			}
			if err = p.waitRelayChange(ctx, c, next, settle(), planned); err == nil {
				confirmed = true
				break
			}
			if errors.Is(err, errRelayUnconfirmed) {
				current = sessionstore.RelayAssignment{Slot: original, Generation: next.Generation + 1}
				continue
			}
			break
		}
		err = errors.Join(err, abortErr)
		break // the rollback failed before switching
	}
	if confirmed {
		if episode != nil {
			relayWatchEvent(c, "rollback-confirmed")
			if e := p.completeRelayEpisode(ctx, c, episode); e != nil {
				return e // the episode ended at its deadline
			}
		}
		return nil
	}
	if !switched {
		if err == nil {
			err = context.DeadlineExceeded
		}
		return err // the original working path was kept
	}
	seed := current.Slot
	if _, a, e := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.id); e == nil {
		seed = a.Slot
	}
	detected := time.Now()
	if firstOuter.Before(detected) {
		detected = firstOuter
	}
	if e := p.startRelayRecoveryFrom(ctx, c, false, detected, []string{seed}); e != nil {
		return errors.Join(err, e)
	}
	return errors.Join(errRelayUnconfirmed, err)
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
		answer, err := worker.RestartSession(r.Context(), c.id, mediaworker.RestartRequest{Offer: string(body), Generation: g, Slot: a.Pending, Required: event.RequiredMIDs})
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
		writeRestartHeaders(w, *event)
		w.Header().Set("Content-Type", "application/sdp")
		_, _ = io.WriteString(w, answer)
	})
	mux.HandleFunc("POST /calls/{id}/media-progress", p.handleMediaProgress)
	mux.HandleFunc("GET /calls/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		c, err := p.lookup(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		if _, err = p.relayMode(r.Context(), c); err != nil {
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
				reason, detail := c.relayEndReason, c.relayEndDetail
				c.stateMu.Unlock()
				if reason == "" {
					reason = "ended"
				}
				ended := RelayEvent{Reason: reason}
				if reason == "relay_change_failed" {
					ended = detail
					ended.Reason, ended.Deadline = reason, time.Time{}
				}
				data, _ := json.Marshal(ended)
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
