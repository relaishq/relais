package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// ADR 0016 sections 5 and 7 defaults.
const (
	relayPlannedLimit     = 10 * time.Second // planned event to confirmation
	relayAttemptBound     = 3 * time.Second  // A mode, from the observed switch commit
	relayDeliveryPoll     = 100 * time.Millisecond
	mediaProgressMaxBytes = 4 << 10
)

var errRelayUnconfirmed = errors.New("controlplane: relay change unconfirmed")

// errRelayEpisodeEnded: the call's episode already decided to end the call;
// no confirmation is saved and no success is reported after that decision.
var errRelayEpisodeEnded = errors.New("controlplane: relay_change_failed: episode ended")

// relayDecisionGate runs a success write while the call's active episode
// cannot make its terminal decision, and refuses it after that decision. The
// gate holds only the episode's decision lock, never the call lock, so
// publication at the deadline stays independent of a busy operation.
func (p *Plane) relayDecisionGate(c *call, write func(episode time.Time) error) error {
	c.relayEpisodeMu.Lock()
	e := c.relayEpisode
	c.relayEpisodeMu.Unlock()
	if e == nil {
		return write(time.Time{})
	}
	e.decision.Lock()
	defer e.decision.Unlock()
	if e.ending {
		return errRelayEpisodeEnded
	}
	return write(e.deadline)
}

// errRelayRetired: the attempt's generation was consumed without a switch.
var errRelayRetired = errors.New("controlplane: relay generation retired without a switch")

// ErrConfirmationUnsupported marks an owning worker without the C contract.
// A planned drain leaves such a call on its slot; recovery ends it.
var ErrConfirmationUnsupported = errors.New("controlplane: confirmation_unsupported")

// relayUnconfirmed is a committed attempt whose required evidence did not
// arrive by its attempt bound. The call is on Slot at Generation.
type relayUnconfirmed struct{ detail RelayEvent }

func (e *relayUnconfirmed) Error() string {
	return "controlplane: relay change unconfirmed: " + e.detail.Detail
}
func (e *relayUnconfirmed) Is(target error) bool { return target == errRelayUnconfirmed }

// relayDeliveryWorker is the worker C contract. An older worker lacks it.
type relayDeliveryWorker interface {
	RelayDelivery(context.Context, string, uint64) (mediaworker.RelayDeliveryReport, error)
}

type relayAttemptOptions struct {
	kind     string    // planned or recovery
	outer    time.Time // planned deadline, or the saved episode deadline
	detected time.Time // episode detection; zero for planned
	episode  time.Time // planned attempts issued after an episode was armed
	tried    []string
}

// relayFailureDetail names the evidence state for an unconfirmed attempt
// (ADR 0016 section 7). It describes saved evidence, not physical loss.
func relayFailureDetail(rec sessionstore.RelayConfirmation) RelayEvent {
	t := rec.Attempt
	if t == nil {
		return RelayEvent{Detail: "delivery_unconfirmed", Confirmation: rec.Mode}
	}
	detail := RelayEvent{Generation: t.Generation, Slot: t.Slot, Confirmation: rec.Mode, Detail: "delivery_unconfirmed"}
	evidence := t.Delivered
	if rec.Mode == sessionstore.RelayConfirmDecode {
		evidence = t.Decoded
		if sessionstore.Covers(t.Delivered, t.Required) {
			detail.Detail = "delivered_decode_unconfirmed"
		}
	}
	for _, mid := range t.Required {
		if !slices.Contains(evidence, mid) {
			detail.MissingMIDs = append(detail.MissingMIDs, mid)
		}
	}
	return detail
}

// relayMode returns the call's accepted confirmation mode. A call created by
// an older control has no stored mode: a relay-capable one is assigned
// delivery before any further change (ADR 0016 section 10, rule 3).
func (p *Plane) relayMode(ctx context.Context, c *call) (string, error) {
	c.stateMu.Lock()
	mode, capable := c.relayMode, c.relayCapable
	c.stateMu.Unlock()
	if mode != "" {
		return mode, nil
	}
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		// No durable record: a change attempt fails closed as
		// confirmation_unsupported; it never completes on commit.
		if capable {
			return sessionstore.RelayConfirmDelivery, nil
		}
		return sessionstore.RelayConfirmNone, nil
	}
	for range 5 {
		a, rec, err := store.RelayConfirmation(ctx, c.id)
		if err != nil {
			return "", err
		}
		mode = rec.Mode
		p.mu.Lock()
		enabled := len(p.relaySet) > 0
		p.mu.Unlock()
		if mode == "" && (!capable || !enabled) {
			// Without a relay set nothing can change relays, and a default
			// profile never writes the confirmation record (contract 3).
			mode = sessionstore.RelayConfirmNone
		}
		if mode == "" {
			mode = sessionstore.RelayConfirmDelivery
			if err = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: mode}, time.Time{}); err != nil {
				if errors.Is(err, sessionstore.ErrRelayConflict) {
					continue
				}
				return "", err
			}
		}
		c.stateMu.Lock()
		c.relayMode = mode
		c.relayCapable = mode == sessionstore.RelayConfirmDelivery || mode == sessionstore.RelayConfirmDecode
		c.stateMu.Unlock()
		return mode, nil
	}
	return "", sessionstore.ErrRelayConflict
}

// acceptCapabilities decides a new call's mode. Without a relay set, or a
// store for the confirmation record, nothing can change relays: the call is
// ordinary, nothing is written and nothing is echoed (the event stream stays
// available, as before). A requested A needs the selected worker's C and
// confirmation contract; otherwise create is refused as relay_unsupported.
func (p *Plane) acceptCapabilities(ctx context.Context, c *call, relayChange, mediaProgress bool) (string, error) {
	p.mu.Lock()
	enabled := len(p.relaySet) > 0
	p.mu.Unlock()
	_, durable := p.store.(sessionstore.RelayConfirmations)
	if !relayChange || !enabled || !durable {
		c.stateMu.Lock()
		c.relayMode, c.relayCapable = sessionstore.RelayConfirmNone, relayChange
		c.stateMu.Unlock()
		return sessionstore.RelayConfirmNone, nil
	}
	mode := sessionstore.RelayConfirmDelivery
	if mediaProgress {
		if err := p.workerConfirms(ctx, c); err != nil {
			return "", err
		}
		mode = sessionstore.RelayConfirmDecode
	}
	return mode, p.acceptRelayMode(ctx, c, mode)
}

// workerConfirms asks the call's owner for its C contract. An older worker
// has no route and reports relay_unsupported.
func (p *Plane) workerConfirms(ctx context.Context, c *call) error {
	lease, err := p.store.Get(ctx, c.id)
	if err != nil {
		return err
	}
	p.mu.Lock()
	owner := p.owner(lease.Worker)
	p.mu.Unlock()
	if owner == nil {
		return ErrNoTarget
	}
	worker, ok := owner.worker.(relayDeliveryWorker)
	if !ok {
		return mediaworker.ErrRelayUnsupported
	}
	_, err = worker.RelayDelivery(ctx, c.id, 1)
	return err
}

// acceptRelayMode persists the mode accepted at create, before the create
// response echoes it. It is fixed for the whole call.
func (p *Plane) acceptRelayMode(ctx context.Context, c *call, mode string) error {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return mediaworker.ErrRelayUnsupported
	}
	a, rec, err := store.RelayConfirmation(ctx, c.id)
	if err != nil {
		return err
	}
	if rec.Mode != "" && rec.Mode != mode {
		return sessionstore.ErrRelayConflict
	}
	if rec.Mode == "" {
		if err = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: mode}, time.Time{}); err != nil {
			return err
		}
	}
	c.stateMu.Lock()
	c.relayMode = mode
	c.relayCapable = true
	c.stateMu.Unlock()
	return nil
}

// casRelayConfirmation settles an uncertain write by reading it back. It never
// reports success for a write it cannot prove.
//
// Every write is fenced by notAfter, a store-clock time (zero: none), and by
// the end of this control's authority. The store enforces both with its own
// clock inside the atomic write, so a delayed write cannot land after either.
func (p *Plane) casRelayConfirmation(ctx context.Context, id string, at sessionstore.RelayAssignment, from, to sessionstore.RelayConfirmation, notAfter time.Time) error {
	store := p.store.(sessionstore.RelayConfirmations)
	if authority, ok := p.authorityDeadline(); ok {
		storeAuthority, err := p.storeTime(ctx, id, authority)
		if err != nil {
			return err
		}
		if notAfter.IsZero() || storeAuthority.Before(notAfter) {
			notAfter = storeAuthority
		}
	}
	err := store.CASRelayConfirmation(ctx, id, at, from, to, notAfter)
	if err == nil || errors.Is(err, sessionstore.ErrRelayConflict) || errors.Is(err, sessionstore.ErrRelayTransition) || errors.Is(err, sessionstore.ErrNotFound) || errors.Is(err, sessionstore.ErrRelayConfirmationInvalid) || errors.Is(err, sessionstore.ErrRelayAttemptExpired) {
		return err
	}
	gotA, got, e := store.RelayConfirmation(ctx, id)
	if e == nil && gotA == at && sameRelayConfirmation(got, to) {
		return nil
	}
	return err
}

// storeTime converts a local deadline to the store clock without comparing
// clocks across hosts: it reads the store clock and subtracts the local time
// still remaining, minus the read's round trip (a conservative bound). A
// deadline that has already passed locally is refused here.
func (p *Plane) storeTime(ctx context.Context, id string, local time.Time) (time.Time, error) {
	if !local.After(time.Now()) {
		return time.Time{}, sessionstore.ErrRelayAttemptExpired
	}
	converted, err := p.storeClockOf(ctx, id, local)
	if err == nil && !local.After(time.Now()) {
		return time.Time{}, sessionstore.ErrRelayAttemptExpired
	}
	return converted, err
}

// storeClockOf converts any local time, past or future, to the store clock
// with the same conservative round-trip bound as storeTime.
func (p *Plane) storeClockOf(ctx context.Context, id string, local time.Time) (time.Time, error) {
	clock, ok := p.store.(interface {
		Clock(context.Context, string) (time.Time, error)
	})
	start := time.Now()
	if !ok {
		return local, nil
	}
	now, err := clock.Clock(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	end := time.Now()
	return now.Add(local.Sub(end) - end.Sub(start)), nil
}

// bindEpisode stamps an episode on an attempt without extending any bound.
// A record without store-clock fields first gets them from its own original
// Outer and Bound; then every value is clamped to the episode deadline.
func (p *Plane) bindEpisode(ctx context.Context, id string, t sessionstore.RelayAttempt, episode, detected time.Time) (sessionstore.RelayAttempt, error) {
	bound := t
	if bound.StoreDeadline.IsZero() && !t.Outer().IsZero() {
		storeOuter, err := p.storeClockOf(ctx, id, t.Outer())
		if err != nil {
			return t, err
		}
		bound.StoreDeadline = storeOuter
	}
	if bound.StoreBound.IsZero() && !t.AttemptBound.IsZero() {
		storeBound, err := p.storeClockOf(ctx, id, t.Bound())
		if err != nil {
			return t, err
		}
		bound.StoreBound = storeBound
	}
	bound.EpisodeDeadline = episode
	if !detected.IsZero() {
		bound.Detected = detected
	}
	if !bound.AttemptBound.IsZero() && episode.Before(bound.AttemptBound) {
		bound.AttemptBound = episode
	}
	storeEpisode, err := p.storeClockOf(ctx, id, episode)
	if err != nil {
		return t, err
	}
	if bound.StoreDeadline.IsZero() || storeEpisode.Before(bound.StoreDeadline) {
		bound.StoreDeadline = storeEpisode
	}
	if !bound.StoreBound.IsZero() && storeEpisode.Before(bound.StoreBound) {
		bound.StoreBound = storeEpisode
	}
	return bound, nil
}

// evidenceBound is the store-clock bound for A and C writes. The saved store
// bound is authoritative; only a record without one (written before the
// attempt saved store-clock fields) falls back to converting its local bound.
func (p *Plane) evidenceBound(ctx context.Context, id string, t sessionstore.RelayAttempt) (time.Time, error) {
	if bound := t.EvidenceBound(); !bound.IsZero() {
		return bound, nil
	}
	if local := t.Bound(); !local.IsZero() {
		return p.storeTime(ctx, id, local)
	}
	return time.Time{}, nil
}

// authorityDeadline is the end of this control's acknowledged authority.
func (p *Plane) authorityDeadline() (time.Time, bool) {
	if p.restart == nil {
		return time.Time{}, false
	}
	guard := p.restart.guard.Load()
	if guard == nil {
		return time.Now(), true // no authority: every fenced write is refused
	}
	return guard.deadline, true
}

func sameRelayConfirmation(a, b sessionstore.RelayConfirmation) bool {
	x, _ := json.Marshal(a.Attempt)
	y, _ := json.Marshal(b.Attempt)
	return a.Mode == b.Mode && string(x) == string(y)
}

// beginRelayAttempt fixes the required MIDs with the owning worker and saves
// the pending attempt before the event. A repeated call for the same
// generation adopts the saved attempt; its bounds never grow.
func (p *Plane) beginRelayAttempt(ctx context.Context, c *call, lease sessionstore.Lease, pending sessionstore.RelayAssignment, mode string, opt relayAttemptOptions) (*sessionstore.RelayAttempt, error) {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return nil, ErrConfirmationUnsupported
	}
	generation := pending.Generation + 1
	for range 5 {
		a, rec, err := store.RelayConfirmation(ctx, c.id)
		if err != nil {
			return nil, err
		}
		committed := a.Pending == "" && a.Slot == pending.Pending && a.Generation == generation
		if a != pending && !committed {
			return nil, sessionstore.ErrRelayConflict
		}
		if rec.Attempt != nil && rec.Attempt.Generation == generation {
			// An armed episode durably owns an attempt that already exists,
			// before any wait, so its watchdog recognises the confirmation.
			bound, err := p.bindRelayAttempt(ctx, c.id, a, rec, opt.episode)
			if errors.Is(err, sessionstore.ErrRelayConflict) {
				continue
			}
			return bound, err
		}
		if rec.Attempt != nil && rec.Attempt.Generation > generation {
			return nil, sessionstore.ErrRelayConflict
		}
		p.mu.Lock()
		owner := p.owner(lease.Worker)
		p.mu.Unlock()
		if owner == nil {
			return nil, ErrNoTarget
		}
		worker, ok := owner.worker.(relayDeliveryWorker)
		if !ok {
			return nil, ErrConfirmationUnsupported
		}
		report, err := worker.RelayDelivery(ctx, c.id, generation)
		if err != nil {
			if errors.Is(err, mediaworker.ErrRelayUnsupported) {
				return nil, errors.Join(ErrConfirmationUnsupported, err)
			}
			return nil, err
		}
		attempt := &sessionstore.RelayAttempt{Generation: generation, Slot: pending.Pending, Phase: sessionstore.RelayPhasePending,
			Required: slices.Clone(report.Required), Kind: opt.kind, Tried: slices.Clone(opt.tried)}
		if opt.kind == "planned" {
			// An armed episode owns a planned attempt too, so its watchdog
			// recognises this attempt's confirmation (finding: episode binding).
			attempt.PlannedDeadline, attempt.EpisodeDeadline = opt.outer, opt.episode
		} else {
			attempt.Detected, attempt.EpisodeDeadline = opt.detected, opt.outer
		}
		if attempt.StoreDeadline, err = p.storeTime(ctx, c.id, attempt.Outer()); err != nil {
			return nil, err
		}
		if rec.Attempt != nil && opt.kind != "planned" && rec.Attempt.EpisodeDeadline.Equal(opt.outer) {
			for _, slot := range rec.Attempt.Tried {
				if !slices.Contains(attempt.Tried, slot) {
					attempt.Tried = append(attempt.Tried, slot)
				}
			}
		}
		// Supersession consumes the previous generation.
		if err = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: mode, Attempt: attempt}, time.Time{}); err != nil {
			if errors.Is(err, sessionstore.ErrRelayConflict) {
				continue
			}
			return nil, err
		}
		return attempt, nil
	}
	return nil, sessionstore.ErrRelayConflict
}

// bindRelayAttempt stamps an episode deadline on an attempt that has none. It
// keeps or shortens every saved bound and never extends one: Outer is the
// earlier deadline, and the store-clock values only move earlier.
func (p *Plane) bindRelayAttempt(ctx context.Context, id string, a sessionstore.RelayAssignment, rec sessionstore.RelayConfirmation, episode time.Time) (*sessionstore.RelayAttempt, error) {
	t := rec.Attempt
	if episode.IsZero() || !t.EpisodeDeadline.IsZero() {
		return t, nil
	}
	bound, err := p.bindEpisode(ctx, id, *t, episode, time.Time{})
	if err != nil {
		return nil, err
	}
	if err = p.casRelayConfirmation(ctx, id, a, rec, sessionstore.RelayConfirmation{Mode: rec.Mode, Attempt: &bound, Commit: rec.Commit}, time.Time{}); err != nil {
		return nil, err
	}
	return &bound, nil
}

// waitRelayChange publishes the attempt's event and waits for its switch
// commit within commitBound, then for the mode's confirmation (ADR 0016
// section 7). A switch commit alone never completes the change. A committed
// attempt without evidence by its attempt bound returns relayUnconfirmed.
func (p *Plane) waitRelayChange(ctx context.Context, c *call, pending sessionstore.RelayAssignment, commitBound time.Duration, opt relayAttemptOptions) error {
	mode, err := p.relayMode(ctx, c)
	if err != nil {
		return err
	}
	if mode != sessionstore.RelayConfirmDelivery && mode != sessionstore.RelayConfirmDecode {
		return mediaworker.ErrRelayUnsupported
	}
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return ErrConfirmationUnsupported
	}
	lease, _, err := store.RelayOwner(ctx, c.id)
	if err != nil {
		return err
	}
	attempt, err := p.beginRelayAttempt(ctx, c, lease, pending, mode, opt)
	if err != nil {
		return err
	}
	generation := pending.Generation + 1
	committed := sessionstore.RelayAssignment{Slot: pending.Pending, Generation: generation}
	commitBy := time.Now().Add(commitBound)
	c.stateMu.Lock()
	c.relayEvent = &RelayEvent{Generation: generation, Deadline: opt.outer.UTC(), RequiredMIDs: slices.Clone(attempt.Required), Confirmation: mode}
	c.relayRejected = false
	select {
	case c.relayWake <- struct{}{}:
	default:
	}
	c.stateMu.Unlock()
	poller := relayDeliveryPoller{lease: lease}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		c.stateMu.Lock()
		rejected, stall := c.relayRejected, c.relayStall == generation
		c.stateMu.Unlock()
		if rejected {
			return mediaworker.ErrRestartMismatch
		}
		a, rec, e := store.RelayConfirmation(ctx, c.id)
		if e == nil && a == committed && rec.Attempt != nil && rec.Attempt.Generation == generation {
			if done, err := p.confirmStep(ctx, c, a, rec, mode, opt, stall, &poller); done {
				return err
			}
		} else if !time.Now().Before(commitBy) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

type relayDeliveryPoller struct {
	lease  sessionstore.Lease
	worker relayDeliveryWorker
	last   time.Time
}

// confirmStep advances one committed attempt. Its writes are compare-and-set
// against what was read; a conflict or an uncertain write is settled by the
// next read. done reports a final outcome: nil (confirmed) or an error.
func (p *Plane) confirmStep(ctx context.Context, c *call, a sessionstore.RelayAssignment, rec sessionstore.RelayConfirmation, mode string, opt relayAttemptOptions, stall bool, poller *relayDeliveryPoller) (bool, error) {
	t := *rec.Attempt
	next := func(t sessionstore.RelayAttempt) sessionstore.RelayConfirmation {
		return sessionstore.RelayConfirmation{Mode: mode, Attempt: &t, Commit: rec.Commit}
	}
	if rec.Confirmed(a) {
		// Success is gated by the episode's terminal decision: after it, no
		// confirmation is saved and no success is returned. Before it, the
		// confirmed attempt is bound to the episode, so its watchdog sees it.
		err := p.relayDecisionGate(c, func(episode time.Time) error {
			bound := t.Phase != sessionstore.RelayPhaseConfirmed || !episode.IsZero() && t.EpisodeDeadline.IsZero()
			if !bound {
				return nil
			}
			t.Phase = sessionstore.RelayPhaseConfirmed
			if !episode.IsZero() && t.EpisodeDeadline.IsZero() {
				t.EpisodeDeadline = episode
			}
			return p.casRelayConfirmation(ctx, c.id, a, rec, next(t), time.Time{})
		})
		if errors.Is(err, errRelayEpisodeEnded) {
			return true, err
		}
		if err != nil {
			return false, nil
		}
		log.Printf("controlplane: relay change confirmed call=%s generation=%d slot=%s mode=%s delivered=%v decoded=%v", c.id, t.Generation, t.Slot, mode, t.Delivered, t.Decoded)
		c.stateMu.Lock()
		c.relayEvent = nil
		c.relaySlot = a.Slot
		c.stateMu.Unlock()
		return true, nil
	}
	if rec.Aborted() {
		// The generation was retired without a switch (an abort, possibly
		// on the same slot). It can never confirm.
		return true, errRelayRetired
	}
	if t.Phase == sessionstore.RelayPhaseExpired {
		return true, &relayUnconfirmed{relayFailureDetail(rec)}
	}
	if rec.Commit.Switched != t.Generation {
		return false, nil // the commit write has not been observed yet
	}
	now := time.Now()
	if t.Phase == sessionstore.RelayPhasePending && rec.Switched(a) {
		// The control-observed commit time starts the attempt bound once;
		// repeated observations and takeover never restart it (owner
		// decision: not the store's commit time).
		t.Phase, t.CommitAt, t.AttemptBound, t.StoreBound = sessionstore.RelayPhaseSwitched, now, t.Outer(), t.StoreDeadline
		if mode == sessionstore.RelayConfirmDecode && now.Add(relayAttemptBound).Before(t.Outer()) {
			t.AttemptBound = now.Add(relayAttemptBound)
			storeBound, err := p.storeTime(ctx, c.id, t.AttemptBound)
			if err != nil {
				return false, nil
			}
			if t.StoreBound.IsZero() || storeBound.Before(t.StoreBound) {
				t.StoreBound = storeBound
			}
		}
		_ = p.casRelayConfirmation(ctx, c.id, a, rec, next(t), time.Time{})
		return false, nil
	}
	if t.Phase == sessionstore.RelayPhaseSwitched && now.Sub(poller.last) >= relayDeliveryPoll {
		poller.last = now
		if poller.worker == nil {
			p.mu.Lock()
			if owner := p.owner(poller.lease.Worker); owner != nil {
				poller.worker, _ = owner.worker.(relayDeliveryWorker)
			}
			p.mu.Unlock()
		}
		if poller.worker != nil {
			p.retainDelivery(ctx, c, poller.worker, poller.lease, a, rec, mode)
		}
	}
	// A mode: C is diagnostic and never extends the bound. A caller stall
	// after the commit fails the unconfirmed attempt at once. A commit that
	// landed after the outer deadline (pending, !Switched) also expires.
	if t.Phase == sessionstore.RelayPhaseSwitched && now.Before(t.Bound()) && !stall {
		return false, nil
	}
	t.Phase = sessionstore.RelayPhaseExpired
	if p.casRelayConfirmation(ctx, c.id, a, rec, next(t), time.Time{}) != nil {
		return false, nil
	}
	detail := relayFailureDetail(next(t))
	log.Printf("controlplane: relay change unconfirmed call=%s generation=%d slot=%s mode=%s detail=%s missing=%v", c.id, t.Generation, t.Slot, mode, detail.Detail, detail.MissingMIDs)
	return true, &relayUnconfirmed{detail}
}

// retainDelivery saves fresh C for the committed attempt. The report must
// match the lease epoch, generation and slot; only required MIDs count. The
// store refuses the write after the attempt bound.
func (p *Plane) retainDelivery(ctx context.Context, c *call, worker relayDeliveryWorker, lease sessionstore.Lease, a sessionstore.RelayAssignment, rec sessionstore.RelayConfirmation, mode string) {
	t := *rec.Attempt
	report, err := worker.RelayDelivery(ctx, c.id, t.Generation)
	if err != nil || !report.Switched || report.Epoch != lease.Epoch || report.Slot != t.Slot || report.Generation != t.Generation {
		return
	}
	grown := false
	for _, mid := range report.Delivered {
		if slices.Contains(t.Required, mid) && !slices.Contains(t.Delivered, mid) {
			t.Delivered = append(slices.Clone(t.Delivered), mid)
			grown = true
		}
	}
	if grown {
		bound, err := p.evidenceBound(ctx, c.id, t)
		if err != nil {
			return
		}
		_ = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: mode, Attempt: &t, Commit: rec.Commit}, bound)
	}
}

type relayExpiry int

const (
	relayExpiryEnded     relayExpiry = iota // the episode ends unconfirmed
	relayExpiryConfirmed                    // this episode's attempt confirmed first
	relayExpiryUncertain                    // the store could not decide; retry
	relayExpiryGone                         // the session is gone: nothing to end
)

// expireRelayAttempt records the episode deadline against the episode's own
// attempt. Only an attempt bound to this episode (same saved episode
// deadline) can report confirmed: an earlier episode's confirmation never
// completes a later one. Unavailable or contended state is uncertain, never
// expiry.
func (p *Plane) expireRelayAttempt(ctx context.Context, c *call, deadline time.Time) (relayExpiry, RelayEvent) {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return relayExpiryEnded, RelayEvent{Detail: "delivery_unconfirmed"}
	}
	for range 5 {
		a, rec, err := store.RelayConfirmation(ctx, c.id)
		if errors.Is(err, sessionstore.ErrNotFound) || errors.Is(err, sessionstore.ErrEnded) {
			return relayExpiryGone, RelayEvent{}
		}
		if err != nil {
			return relayExpiryUncertain, RelayEvent{}
		}
		t := rec.Attempt
		if t == nil || !t.EpisodeDeadline.Equal(deadline) {
			// Not this episode's attempt. An unconfirmed one still in flight is
			// expired durably too, so no later commit or report revives it.
			if t != nil && !rec.Confirmed(a) && !rec.Aborted() && (t.Phase == sessionstore.RelayPhasePending || t.Phase == sessionstore.RelayPhaseSwitched) {
				next := *t
				next.Phase = sessionstore.RelayPhaseExpired
				err = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: rec.Mode, Attempt: &next, Commit: rec.Commit}, time.Time{})
				if errors.Is(err, sessionstore.ErrRelayConflict) {
					continue
				}
				if err != nil {
					return relayExpiryUncertain, RelayEvent{}
				}
			}
			return relayExpiryEnded, RelayEvent{Detail: "delivery_unconfirmed", Confirmation: rec.Mode}
		}
		if rec.Confirmed(a) {
			return relayExpiryConfirmed, RelayEvent{}
		}
		detail := relayFailureDetail(rec)
		if t.Phase == sessionstore.RelayPhaseExpired || rec.Aborted() {
			return relayExpiryEnded, detail
		}
		next := *t
		next.Phase = sessionstore.RelayPhaseExpired
		err = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: rec.Mode, Attempt: &next, Commit: rec.Commit}, time.Time{})
		if err == nil {
			return relayExpiryEnded, detail
		}
		if !errors.Is(err, sessionstore.ErrRelayConflict) {
			return relayExpiryUncertain, RelayEvent{}
		}
	}
	return relayExpiryUncertain, RelayEvent{}
}

// mediaProgress is the authenticated caller's A report (ADR 0016 section 5).
type mediaProgress struct {
	Generation  uint64   `json:"generation"`
	DecodedMIDs []string `json:"decoded_mids"`
}

// allowMediaProgress applies one request per second per call, burst 2.
func (c *call) allowMediaProgress(now time.Time) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.progressAt.IsZero() {
		c.progressTokens, c.progressAt = 2, now
	}
	c.progressTokens = min(2, c.progressTokens+now.Sub(c.progressAt).Seconds())
	c.progressAt = now
	if c.progressTokens < 1 {
		return false
	}
	c.progressTokens--
	return true
}

func (p *Plane) handleMediaProgress(w http.ResponseWriter, r *http.Request) {
	// Only a control holding mutation authority may retain A. Each write is
	// also fenced by the end of that authority inside the store (see
	// casRelayConfirmation); a write refused there is never acknowledged.
	if err := p.requireMutationReady(); err != nil {
		writeError(w, err)
		return
	}
	c, err := p.lookup(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	p.mu.Lock()
	ended := c.terminal || c.hungUp.Load()
	p.mu.Unlock()
	c.stateMu.Lock()
	ended = ended || c.relayEndReason != ""
	c.stateMu.Unlock()
	if ended {
		http.Error(w, "call ended", http.StatusGone)
		return
	}
	if !c.allowMediaProgress(time.Now()) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "media progress rate exceeded", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mediaProgressMaxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "media progress too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid media progress", http.StatusBadRequest)
		return
	}
	var report mediaProgress
	ct, _, ctErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ctErr != nil || ct != "application/json" || json.Unmarshal(body, &report) != nil || report.Generation == 0 || len(report.DecodedMIDs) == 0 || len(report.DecodedMIDs) > 16 {
		http.Error(w, "invalid media progress", http.StatusBadRequest)
		return
	}
	mode, err := p.relayMode(r.Context(), c)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if mode != sessionstore.RelayConfirmDecode {
		http.Error(w, "media progress not accepted for this call", http.StatusBadRequest)
		return
	}
	store := p.store.(sessionstore.RelayConfirmations)
	for range 5 {
		a, rec, err := store.RelayConfirmation(r.Context(), c.id)
		if errors.Is(err, sessionstore.ErrNotFound) {
			http.Error(w, "call ended", http.StatusGone)
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		t := rec.Attempt
		if t == nil || t.Generation != report.Generation || t.Phase == sessionstore.RelayPhaseExpired || rec.Aborted() {
			http.Error(w, "stale or unknown generation", http.StatusConflict)
			return
		}
		for _, mid := range report.DecodedMIDs {
			if !slices.Contains(t.Required, mid) {
				http.Error(w, "unexpected mid", http.StatusConflict)
				return
			}
		}
		if t.Phase == sessionstore.RelayPhaseConfirmed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		bound := t.Bound()
		if !bound.IsZero() && !time.Now().Before(bound) {
			http.Error(w, "attempt expired", http.StatusConflict)
			return
		}
		next := *t
		next.Decoded = slices.Clone(t.Decoded)
		for _, mid := range report.DecodedMIDs {
			if !slices.Contains(next.Decoded, mid) {
				next.Decoded = append(next.Decoded, mid)
			}
		}
		slices.Sort(next.Decoded)
		updated := sessionstore.RelayConfirmation{Mode: mode, Attempt: &next, Commit: rec.Commit}
		// The write that supplies the last prerequisite records the outcome.
		if updated.Confirmed(a) {
			next.Phase = sessionstore.RelayPhaseConfirmed
		}
		if slices.Equal(next.Decoded, t.Decoded) && next.Phase == t.Phase {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		storeBound, err := p.evidenceBound(r.Context(), c.id, *t)
		if err == nil {
			// A is never acknowledged after the episode's terminal decision.
			err = p.relayDecisionGate(c, func(time.Time) error {
				return p.casRelayConfirmation(r.Context(), c.id, a, rec, updated, storeBound)
			})
		}
		if errors.Is(err, errRelayEpisodeEnded) {
			http.Error(w, "call ended", http.StatusGone)
			return
		}
		if errors.Is(err, sessionstore.ErrRelayConflict) {
			continue
		}
		if errors.Is(err, sessionstore.ErrRelayAttemptExpired) {
			if !p.controlAuthority() {
				writeError(w, ErrNotReady)
				return
			}
			http.Error(w, "attempt expired", http.StatusConflict)
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// writeRestartHeaders adds the ADR 0016 section 5 reply metadata.
func writeRestartHeaders(w http.ResponseWriter, event RelayEvent) {
	mids, _ := json.Marshal(event.RequiredMIDs)
	if event.RequiredMIDs == nil {
		mids = []byte("[]")
	}
	w.Header().Set("Relais-Relay-Generation", strconv.FormatUint(event.Generation, 10))
	w.Header().Set("Relais-Relay-Deadline", event.Deadline.UTC().Format(time.RFC3339Nano))
	w.Header().Set("Relais-Required-Mids", string(mids))
	w.Header().Set("Relais-Relay-Confirmation", event.Confirmation)
}

func (w *RemoteWorker) RelayDelivery(ctx context.Context, id string, generation uint64) (mediaworker.RelayDeliveryReport, error) {
	var report mediaworker.RelayDeliveryReport
	err := privateapi.Do(ctx, remoteClient(w.Client), w.URL, http.MethodGet, "/sessions/"+id+"/relay-delivery?generation="+strconv.FormatUint(generation, 10), nil, &report, RemoteErrors)
	var status *privateapi.HTTPError
	if errors.As(err, &status) && (status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusMethodNotAllowed) && !errors.Is(err, mediaworker.ErrUnknownSession) && !errors.Is(err, sessionstore.ErrNotFound) {
		// A worker from before the confirmation contract has no route.
		return report, errors.Join(mediaworker.ErrRelayUnsupported, err)
	}
	return report, err
}

// resumeRelayAttempts restores the drivers of unfinished attempts after a
// control restart (ADR 0016 section 10, rule 4). Saved A, mode, attempt state
// and deadlines come from the store; nothing is asked again. A planned attempt
// becomes a stall episode detected no later than its planned deadline. A
// failure here leaves the call to its caller-stall trigger.
func (p *Plane) resumeRelayAttempts(ctx context.Context) {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	p.mu.Lock()
	enabled := len(p.relaySet) > 0
	calls := make([]*call, 0, len(p.calls))
	for _, c := range p.calls {
		calls = append(calls, c)
	}
	p.mu.Unlock()
	if !ok || !enabled {
		return
	}
	for _, c := range calls {
		_, rec, err := store.RelayConfirmation(ctx, c.id)
		if err != nil || rec.Attempt == nil || rec.Attempt.Phase != sessionstore.RelayPhasePending && rec.Attempt.Phase != sessionstore.RelayPhaseSwitched {
			continue
		}
		t := rec.Attempt
		detected := t.Detected
		if t.Kind == "planned" {
			detected = time.Now()
			if t.PlannedDeadline.Before(detected) {
				detected = t.PlannedDeadline
			}
		}
		if _, err = p.relayMode(ctx, c); err != nil {
			continue
		}
		if err = p.startRelayRecoveryFrom(ctx, c, false, detected, t.Tried); err != nil {
			log.Printf("controlplane: relay attempt not resumed call=%s generation=%d: %v", c.id, t.Generation, err)
		}
	}
}

// adoptRelayAttempt waits for a committed attempt that is still unconfirmed,
// within its saved bound, and binds it to the current episode. It never starts
// a new generation. A pending record whose switch commit is recorded counts
// as committed: control may have acknowledged full A and then stopped before
// it observed the commit.
func (p *Plane) adoptRelayAttempt(ctx context.Context, c *call, from sessionstore.RelayAssignment, opt relayAttemptOptions) (bool, error) {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok || from.Generation == 0 {
		return false, nil
	}
	a, rec, err := store.RelayConfirmation(ctx, c.id)
	if err != nil || a != from {
		return false, err
	}
	t := rec.Attempt
	if t == nil || t.Generation != from.Generation || t.Slot != from.Slot || rec.Commit.Switched != t.Generation || rec.Aborted() ||
		t.Phase != sessionstore.RelayPhaseSwitched && t.Phase != sessionstore.RelayPhasePending {
		return false, nil
	}
	if !t.EpisodeDeadline.Equal(opt.outer) {
		if !t.EpisodeDeadline.IsZero() {
			return false, nil // owned by another episode; never extend it
		}
		// Adoption keeps or shortens every saved deadline, never extends
		// one (bindEpisode).
		bound, err := p.bindEpisode(ctx, c.id, *t, opt.outer, opt.detected)
		if err != nil {
			return false, err
		}
		if err = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: rec.Mode, Attempt: &bound, Commit: rec.Commit}, time.Time{}); err != nil {
			return false, err
		}
	}
	previous := sessionstore.RelayAssignment{Slot: from.Slot, Generation: from.Generation - 1, Pending: from.Slot}
	return true, p.waitRelayChange(ctx, c, previous, 0, opt)
}
