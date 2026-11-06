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

// errRelayEpisodeEnded: the call's episode durably ended; no confirmation is
// saved and no success is reported after that decision.
var errRelayEpisodeEnded = errors.New("controlplane: relay_change_failed: episode ended")

// errRelayEpisodeClosed: the recovery's episode is no longer open (it
// completed, or a later decision replaced it), so its driver is finished and
// starts no further attempt.
var errRelayEpisodeClosed = errors.New("controlplane: relay episode closed")

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
	tried    []string
	episode  string // recovery: the token that must still be open
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

// relayMode returns the call's accepted confirmation mode, saved at create.
// A call without a saved mode has none: relay change was never accepted.
func (p *Plane) relayMode(ctx context.Context, c *call) (string, error) {
	c.stateMu.Lock()
	mode := c.relayMode
	c.stateMu.Unlock()
	if mode != "" {
		return mode, nil
	}
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return sessionstore.RelayConfirmNone, nil
	}
	_, rec, err := store.RelayConfirmation(ctx, c.id)
	if err != nil {
		return "", err
	}
	mode = rec.Mode
	if mode == "" {
		mode = sessionstore.RelayConfirmNone
	}
	c.stateMu.Lock()
	c.relayMode = mode
	c.relayCapable = mode == sessionstore.RelayConfirmDelivery || mode == sessionstore.RelayConfirmDecode
	c.stateMu.Unlock()
	return mode, nil
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
	return p.decisionCAS(ctx, id, at, from, to, time.Time{}, notAfter)
}

// decisionCAS is casRelayConfirmation with a store-clock lower bound. Its
// read-back proves the whole requested record, including the episode.
func (p *Plane) decisionCAS(ctx context.Context, id string, at sessionstore.RelayAssignment, from, to sessionstore.RelayConfirmation, notBefore, notAfter time.Time) error {
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
	to.Commit = from.Commit
	err := store.CASRelayConfirmation(ctx, id, at, from, to, notBefore, notAfter)
	if err == nil || errors.Is(err, sessionstore.ErrRelayConflict) || errors.Is(err, sessionstore.ErrRelayTransition) || errors.Is(err, sessionstore.ErrNotFound) || errors.Is(err, sessionstore.ErrRelayConfirmationInvalid) || errors.Is(err, sessionstore.ErrRelayAttemptExpired) || errors.Is(err, sessionstore.ErrRelayTooEarly) {
		return err
	}
	gotA, got, e := store.RelayConfirmation(ctx, id)
	if e == nil && gotA == at && sameRelayRecord(got, to) {
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
	converted, err := p.convertClock(ctx, id, local, true)
	if err == nil && !local.After(time.Now()) {
		return time.Time{}, sessionstore.ErrRelayAttemptExpired
	}
	return converted, err
}

// convertClock converts a time, past or future, between the local and the
// store clock (toStore picks the direction). It reads the store clock once and
// charges the read's round trip so that the result is never later than the
// true equivalent.
func (p *Plane) convertClock(ctx context.Context, id string, t time.Time, toStore bool) (time.Time, error) {
	clock, ok := p.store.(interface {
		Clock(context.Context, string) (time.Time, error)
	})
	if !ok {
		return t, nil
	}
	start := time.Now()
	now, err := clock.Clock(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	end := time.Now()
	if toStore {
		return now.Add(t.Sub(end) - end.Sub(start)), nil
	}
	return start.Add(t.Sub(now)), nil
}

// evidenceBound is the saved store-clock bound for A and C writes. A record
// without it is an unknown format and accepts no evidence.
func (p *Plane) evidenceBound(t sessionstore.RelayAttempt) (time.Time, error) {
	if bound := t.EvidenceBound(); !bound.IsZero() {
		return bound, nil
	}
	return time.Time{}, sessionstore.ErrRelayConfirmationInvalid
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

// sameRelayRecord compares every field a confirmation write sets or compares:
// mode, attempt, the whole episode and the commit record.
func sameRelayRecord(a, b sessionstore.RelayConfirmation) bool {
	x, _ := json.Marshal(a.Attempt)
	y, _ := json.Marshal(b.Attempt)
	e, _ := json.Marshal(a.Episode)
	f, _ := json.Marshal(b.Episode)
	return a.Mode == b.Mode && string(x) == string(y) && string(e) == string(f) &&
		a.Commit.Switched == b.Commit.Switched && a.Commit.Retired == b.Commit.Retired && a.Commit.SwitchedAt.Equal(b.Commit.SwitchedAt)
}

// confirmRelaySuccess is the single success write. It needs proof, an outcome
// that is not ended and, while an episode is open, an attempt owned by that
// episode; otherwise the call belongs to the episode (ErrMoveInProgress). It
// advances seq and completes the open episode in the same compare-and-set.
func (p *Plane) confirmRelaySuccess(ctx context.Context, id string, a sessionstore.RelayAssignment, rec sessionstore.RelayConfirmation) error {
	ep, t := rec.Episode, *rec.Attempt
	if ep != nil && ep.Outcome == sessionstore.RelayEpisodeEnded {
		return errRelayEpisodeEnded
	}
	if ep.Open() && t.Episode != ep.Token {
		return ErrMoveInProgress
	}
	if t.Phase == sessionstore.RelayPhaseConfirmed && !ep.Open() {
		return nil // already decided; seq advanced with that write
	}
	return p.casRelayConfirmation(ctx, id, a, rec, successRecord(rec), time.Time{})
}

// successRecord is the record after a success: the attempt confirmed, seq
// advanced (fencing every older proposal) and an open episode completed.
func successRecord(rec sessionstore.RelayConfirmation) sessionstore.RelayConfirmation {
	t := *rec.Attempt
	t.Phase = sessionstore.RelayPhaseConfirmed
	episode := sessionstore.RelayEpisode{}
	if rec.Episode != nil {
		episode = *rec.Episode
	}
	episode.Seq++
	if rec.Episode.Open() {
		episode.Outcome = sessionstore.RelayEpisodeCompleted
	}
	rec.Attempt, rec.Episode = &t, &episode
	return rec
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
		if rec.Episode != nil && rec.Episode.Outcome == sessionstore.RelayEpisodeEnded {
			return nil, errRelayEpisodeEnded // ended is final for the live session
		}
		if opt.episode != "" {
			// A recovery attempt belongs to its episode: the compare-and-set
			// below carries this open token, and its completion is final.
			if ep := rec.Episode; !ep.Open() || ep.Token != opt.episode {
				return nil, errRelayEpisodeClosed
			}
		}
		committed := a.Pending == "" && a.Slot == pending.Pending && a.Generation == generation
		if a != pending && !committed {
			return nil, sessionstore.ErrRelayConflict
		}
		if rec.Attempt != nil && rec.Attempt.Generation == generation {
			// An open episode adopts an eligible attempt that already exists
			// (for example an in-flight switch to a live slot), so its
			// confirmation completes the episode.
			bound, err := p.adoptIntoOpenEpisode(ctx, c.id, a, rec)
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
			attempt.PlannedDeadline = opt.outer
		} else {
			attempt.Detected, attempt.EpisodeDeadline = opt.detected, opt.outer
		}
		if attempt.StoreDeadline, err = p.storeTime(ctx, c.id, attempt.Outer()); err != nil {
			return nil, err
		}
		if ep := rec.Episode; ep.Open() {
			// A new attempt is owned by the open episode from its creation.
			deadline := attempt.EpisodeDeadline
			if deadline.IsZero() {
				deadline = attempt.Outer()
			}
			*attempt = clampToEpisode(*attempt, deadline, ep.StoreDeadline)
			attempt.Episode = ep.Token
		}
		if rec.Attempt != nil && opt.kind != "planned" && rec.Attempt.EpisodeDeadline.Equal(opt.outer) {
			for _, slot := range rec.Attempt.Tried {
				if !slices.Contains(attempt.Tried, slot) {
					attempt.Tried = append(attempt.Tried, slot)
				}
			}
		}
		// Supersession consumes the previous generation.
		if err = p.casRelayConfirmation(ctx, c.id, a, rec, sessionstore.RelayConfirmation{Mode: mode, Attempt: attempt, Episode: rec.Episode}, time.Time{}); err != nil {
			if errors.Is(err, sessionstore.ErrRelayConflict) {
				continue
			}
			return nil, err
		}
		return attempt, nil
	}
	return nil, sessionstore.ErrRelayConflict
}

// adoptIntoOpenEpisode binds an existing eligible attempt to the call's open
// episode (the same eligibility as begin) and returns the attempt to wait on.
func (p *Plane) adoptIntoOpenEpisode(ctx context.Context, id string, a sessionstore.RelayAssignment, rec sessionstore.RelayConfirmation) (*sessionstore.RelayAttempt, error) {
	t := rec.Attempt
	ep := rec.Episode
	if !ep.Open() || !p.eligibleForEpisode(*t) {
		return t, nil
	}
	local, err := p.convertClock(ctx, id, ep.StoreDeadline, false)
	if err != nil {
		return nil, err
	}
	bound := clampToEpisode(*t, local, ep.StoreDeadline)
	bound.Episode = ep.Token
	next := rec
	next.Attempt = &bound
	if err = p.casRelayConfirmation(ctx, id, a, rec, next, time.Time{}); err != nil {
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
		if e == nil && rec.Episode != nil && rec.Episode.Outcome == sessionstore.RelayEpisodeEnded {
			return errRelayEpisodeEnded
		}
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
		return sessionstore.RelayConfirmation{Mode: mode, Attempt: &t, Episode: rec.Episode, Commit: rec.Commit}
	}
	if rec.Confirmed(a) {
		// The single success write: ordered against the episode's end in the
		// same compare-and-set; an ended or foreign episode refuses it.
		err := p.confirmRelaySuccess(ctx, c.id, a, rec)
		if errors.Is(err, errRelayEpisodeEnded) || errors.Is(err, ErrMoveInProgress) {
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
	if rec.Episode != nil && rec.Episode.Outcome == sessionstore.RelayEpisodeEnded {
		return
	}
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
		bound, err := p.evidenceBound(t)
		if err != nil {
			return
		}
		next := rec
		next.Attempt = &t
		_ = p.casRelayConfirmation(ctx, c.id, a, rec, next, bound)
	}
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
		if rec.Episode != nil && rec.Episode.Outcome == sessionstore.RelayEpisodeEnded {
			http.Error(w, "call ended", http.StatusGone) // ended is final; no acknowledgement
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
			w.WriteHeader(http.StatusNoContent) // read shows not ended (above)
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
		if slices.Equal(next.Decoded, t.Decoded) {
			w.WriteHeader(http.StatusNoContent) // read shows not ended (above)
			return
		}
		updated := rec
		updated.Attempt = &next
		// The write that supplies the last prerequisite records the outcome,
		// under the single success rule: an attempt not owned by an open
		// episode only retains A.
		if updated.Confirmed(a) && (!rec.Episode.Open() || t.Episode == rec.Episode.Token) {
			updated = successRecord(updated)
		}
		storeBound, err := p.evidenceBound(*t)
		if err != nil {
			http.Error(w, "attempt has no evidence bound", http.StatusConflict)
			return
		}
		err = p.casRelayConfirmation(r.Context(), c.id, a, rec, updated, storeBound)
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

// resumeRelayAttempts runs after a control restart rebuilt the journal's
// episodes (restoreRelayEpisode). Per live call it inspects the durable
// episode even without a pending or switched attempt: an open episode
// without a journal entry is re-journaled and re-armed, an ended one with a
// live session resumes the end, and an unfinished planned attempt arms a new
// episode detected no later than its planned deadline. Every restored episode
// gets its driver; saved A, mode and deadlines come from the store.
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
		c.relayEpisodeMu.Lock()
		e := c.relayEpisode
		c.relayEpisodeMu.Unlock()
		_, rec, err := store.RelayConfirmation(ctx, c.id)
		if err != nil {
			continue
		}
		if _, err = p.relayMode(ctx, c); err != nil {
			continue
		}
		var tried []string
		if rec.Attempt != nil {
			tried = rec.Attempt.Tried
		}
		if e != nil && !e.completed.Load() {
			if !e.unsupported() {
				_ = p.startRelayRecoveryFrom(ctx, c, false, e.proposal().Started, tried)
			}
			continue
		}
		if ep := rec.Episode; ep != nil && (ep.Open() || ep.Outcome == sessionstore.RelayEpisodeEnded) {
			e, err = p.journalDurableEpisode(ctx, c, *ep)
			if err == nil && ep.Open() {
				err = p.startRelayRecoveryFrom(ctx, c, false, e.proposal().Started, tried)
			}
			if err != nil {
				log.Printf("controlplane: relay episode not resumed call=%s: %v", c.id, err)
			}
			continue
		}
		t := rec.Attempt
		if t == nil || t.Phase != sessionstore.RelayPhasePending && t.Phase != sessionstore.RelayPhaseSwitched {
			continue
		}
		detected := t.Detected
		if t.Kind == "planned" {
			detected = time.Now()
			if t.PlannedDeadline.Before(detected) {
				detected = t.PlannedDeadline
			}
		}
		if err = p.startRelayRecoveryFrom(ctx, c, false, detected, t.Tried); err != nil {
			log.Printf("controlplane: relay attempt not resumed call=%s generation=%d: %v", c.id, t.Generation, err)
		}
	}
}

// restoreRelayEpisode reconciles one journal proposal with the durable
// episode at restart (ADR 0016 §7). The session record is authoritative; the
// journal only proposes. Unknown formats and stale proposals are deleted.
func (p *Plane) restoreRelayEpisode(ctx context.Context, c *call, intent sessionstore.ControlIntent) error {
	journal := p.store.(sessionstore.ControlStore)
	if intent.Kind == relayIntentKindUnsupported {
		p.installRelayEpisode(c, intent)
		return nil
	}
	discard := func() error { return journal.DeleteControlIntent(ctx, intent.ID, intent.Token) }
	if intent.Kind != relayIntentKind || intent.Seq == 0 {
		return discard() // unknown format: the stall and end paths remain
	}
	_, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return discard()
	}
	_, rec, err := p.relayConfirmationOrClear(ctx, c)
	if errors.Is(err, sessionstore.ErrNotFound) {
		return discard()
	}
	if err != nil {
		return err
	}
	if rec.Mode == "" || rec.Mode == sessionstore.RelayConfirmNone {
		return discard()
	}
	ep := rec.Episode
	switch {
	case ep != nil && ep.Token == intent.Token && ep.Outcome == sessionstore.RelayEpisodeCompleted:
		return discard() // completed before its journal entry was deleted
	case ep != nil && ep.Token == intent.Token:
		p.installRelayEpisode(c, intent) // open: re-arm; ended: resume the end
		return nil
	case ep != nil && (ep.Open() || ep.Outcome == sessionstore.RelayEpisodeEnded):
		if err = discard(); err != nil {
			return err
		}
		_, err = p.journalDurableEpisode(ctx, c, *ep)
		return err
	}
	var seq uint64
	if ep != nil {
		seq = ep.Seq
	}
	if intent.Seq != seq+1 {
		return discard() // stale: a later decision already happened
	}
	p.installRelayEpisode(c, intent) // its watcher begins it
	return nil
}

// relayConfirmationOrClear treats an undecodable record as no saved mode.
// Its raw-field deletion uses the same authority deadline as confirmation CAS.
func (p *Plane) relayConfirmationOrClear(ctx context.Context, c *call) (sessionstore.RelayAssignment, sessionstore.RelayConfirmation, error) {
	store := p.store.(sessionstore.RelayConfirmations)
	a, rec, err := store.RelayConfirmation(ctx, c.id)
	if errors.Is(err, sessionstore.ErrRelayConfirmationInvalid) {
		var notAfter time.Time
		if authority, ok := p.authorityDeadline(); ok {
			var e error
			notAfter, e = p.storeTime(ctx, c.id, authority)
			if e != nil {
				return a, rec, e
			}
		}
		if e := store.ClearInvalidRelayConfirmation(ctx, c.id, notAfter); e != nil {
			return a, rec, e
		}
		log.Printf("controlplane: discarded relay confirmation session_id=%q reason=%q", c.id, err.Error())
		rec, err = sessionstore.RelayConfirmation{}, nil
	}
	if err == nil && (rec.Mode == "" || rec.Mode == sessionstore.RelayConfirmNone) {
		c.stateMu.Lock()
		c.relayMode, c.relayCapable = sessionstore.RelayConfirmNone, false
		c.stateMu.Unlock()
	}
	return a, rec, err
}

// journalDurableEpisode re-journals an episode that the session record holds
// and installs it. Started is derived from the store-clock deadline.
func (p *Plane) journalDurableEpisode(ctx context.Context, c *call, ep sessionstore.RelayEpisode) (*relayEpisode, error) {
	local, err := p.convertClock(ctx, c.id, ep.StoreDeadline, false)
	if err != nil {
		return nil, err
	}
	c.stateMu.Lock()
	lease := c.lease
	c.stateMu.Unlock()
	intent := sessionstore.ControlIntent{ID: "relay-recovery:" + c.id, Token: ep.Token, Kind: relayIntentKind, Started: local.Add(-relayRecoveryLimit), Lease: lease, Seq: ep.Seq}
	journal := p.store.(sessionstore.ControlStore)
	if err = journal.PutControlIntent(ctx, intent); err != nil {
		return nil, err
	}
	return p.installRelayEpisode(c, intent), nil
}

// installRelayEpisode replaces any in-memory episode (a stale one from an
// earlier term stands down) with one restored from durable state.
func (p *Plane) installRelayEpisode(c *call, intent sessionstore.ControlIntent) *relayEpisode {
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	if old := c.relayEpisode; old != nil {
		old.close()
	}
	e := newRelayEpisode(intent)
	e.loaded, e.persisted, e.watching = true, true, true
	c.relayEpisode = e
	p.watchRelayEpisode(c, e)
	return e
}

// adoptRelayAttempt waits for a committed attempt that is still unconfirmed,
// within its saved bound. The attempt must be owned by the call's open
// episode, or be eligible to join it (adoptIntoOpenEpisode): an in-flight
// switch to a live slot whose commit landed after begin, or an attempt
// recovered after a control restart. It never starts a new generation.
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
	ep := rec.Episode
	if t == nil || !ep.Open() || t.Generation != from.Generation || t.Slot != from.Slot || rec.Commit.Switched != t.Generation || rec.Aborted() ||
		t.Phase != sessionstore.RelayPhaseSwitched && t.Phase != sessionstore.RelayPhasePending {
		return false, nil
	}
	if t.Episode != ep.Token {
		if !p.eligibleForEpisode(*t) {
			return false, nil // owned by another episode, or historical
		}
		if _, err = p.adoptIntoOpenEpisode(ctx, c.id, a, rec); err != nil {
			return false, err
		}
	}
	previous := sessionstore.RelayAssignment{Slot: from.Slot, Generation: from.Generation - 1, Pending: from.Slot}
	return true, p.waitRelayChange(ctx, c, previous, 0, opt)
}
