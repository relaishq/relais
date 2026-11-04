package controlplane

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
)

type relayEpisode struct {
	intent                        sessionstore.ControlIntent
	deadline                      time.Time
	done                          chan struct{}
	once                          sync.Once
	running, loaded, cleanupReady bool // guarded by relayEpisodeMu
	persisted                     bool // guarded by relayEpisodeMu
	completed                     atomic.Bool
	// decision orders a driver's completion against the watchdog's terminal
	// decision: exactly one of them wins, before anything is published.
	decision sync.Mutex
	ending   bool // guarded by decision
}

// decideEnd makes the watchdog's terminal decision. It fails if a driver
// already completed the episode; afterwards no completion can be recorded.
func (e *relayEpisode) decideEnd() bool {
	e.decision.Lock()
	defer e.decision.Unlock()
	if e.completed.Load() {
		return false
	}
	e.ending = true
	return true
}

// markCompleted records a driver's completion unless the watchdog already
// decided to end the call.
func (e *relayEpisode) markCompleted() bool {
	e.decision.Lock()
	defer e.decision.Unlock()
	if e.ending {
		return false
	}
	e.completed.Store(true)
	e.once.Do(func() { close(e.done) })
	return true
}

// The existing journal stores the first detection time, independently of the
// call operation lock. A busy operation or a repeated caller request cannot
// buy another 15 seconds. No new signaling payload is involved.
func (p *Plane) relayRecoveryEpisode(ctx context.Context, c *call, detected time.Time, snapshot ...map[string]sessionstore.ControlIntent) (*relayEpisode, error) {
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	journal := p.store.(sessionstore.ControlStore)
	if e := c.relayEpisode; e != nil && e.completed.Load() {
		if !e.cleanupReady {
			return nil, ErrMoveInProgress
		}
		if err := journal.DeleteControlIntent(ctx, e.intent.ID, e.intent.Token); err != nil {
			return nil, err
		}
		c.relayEpisode = nil
	}
	if c.relayEpisode == nil {
		c.stateMu.Lock()
		lease, capable := c.lease, c.relayCapable
		c.stateMu.Unlock()
		kind := "relay-recovery"
		if !capable {
			kind = "relay-recovery-unsupported"
		}
		intent := sessionstore.ControlIntent{ID: "relay-recovery:" + c.id, Token: randomToken(), Kind: kind, Started: detected, Lease: lease}
		e := &relayEpisode{intent: intent, deadline: intent.Started.Add(relayRecoveryLimit), done: make(chan struct{})}
		c.relayEpisode = e
		p.watchRelayEpisode(c, e)
	}
	e := c.relayEpisode
	if !e.loaded {
		var records []sessionstore.ControlIntent
		if len(snapshot) > 0 {
			if saved, ok := snapshot[0][e.intent.ID]; ok {
				records = append(records, saved)
			}
		} else {
			var err error
			records, err = journal.ControlIntents(ctx)
			if err != nil {
				return nil, err
			}
		}
		for _, saved := range records {
			if saved.ID == e.intent.ID {
				if saved.Kind == "relay-recovery-complete" {
					if err := journal.DeleteControlIntent(ctx, saved.ID, saved.Token); err != nil {
						return nil, err
					}
				} else if saved.Token != e.intent.Token {
					e.completed.Store(true)
					e.once.Do(func() { close(e.done) })
					e = &relayEpisode{intent: saved, deadline: saved.Started.Add(relayRecoveryLimit), done: make(chan struct{})}
					c.relayEpisode = e
					p.watchRelayEpisode(c, e)
				}
				break
			}
		}
		e.loaded = true
	}
	err := journal.PutControlIntent(ctx, e.intent)
	if err == nil {
		e.persisted = true
	}
	return e, err
}

func (p *Plane) watchRelayEpisode(c *call, e *relayEpisode) {
	go func() {
		timer := time.NewTimer(time.Until(e.deadline))
		defer timer.Stop()
		select {
		case <-e.done:
			return
		case <-timer.C:
		}
		// A delayed renewal of the same tenure does not rebuild calls. Retain
		// the expired deadline and enforce it as soon as authority returns.
		// Authority is rechecked on every retry and before every terminal side
		// effect; a call that already ended stops the watcher. The expiry
		// write is ordered against durable completion: this episode's
		// confirmation saved first wins. Unavailable store state is never read
		// as expiry: retry until the outcome is definite.
		var detail RelayEvent
		for backoff := 50 * time.Millisecond; ; backoff = min(2*backoff, time.Second) {
			if !p.awaitRelayAuthority(c, e) || e.completed.Load() {
				return
			}
			p.mu.Lock()
			gone := c.terminal || c.hungUp.Load()
			p.mu.Unlock()
			if gone {
				_ = p.completeRelayEpisode(context.Background(), c, e) // before any decision
				return
			}
			expiry, cancelExpiry := context.WithTimeout(context.Background(), time.Second)
			outcome, d := p.expireRelayAttempt(expiry, c, e.deadline)
			cancelExpiry()
			switch outcome {
			case relayExpiryConfirmed, relayExpiryGone:
				_ = p.completeRelayEpisode(context.Background(), c, e) // before any decision
				return
			case relayExpiryEnded:
				if p.controlAuthority() {
					detail = d
				}
			}
			if detail.Detail != "" {
				break
			}
			select {
			case <-e.done:
				return
			case <-time.After(backoff):
			}
		}
		// The terminal decision is irreversible against completion: a driver
		// that completed first wins and nothing is published; after it, no
		// completion is recorded. Only then is ended published, at the
		// deadline, even while a cancelled operation still holds the call.
		relayWatchEvent(c, "deciding")
		if !e.decideEnd() {
			relayWatchEvent(c, "completed-before-decision")
			return
		}
		relayWatchEvent(c, "decided")
		c.stateMu.Lock()
		if c.relayEndDetail.Detail == "" {
			c.relayEndDetail = detail
		}
		c.relayEndReason = "relay_change_failed"
		select {
		case c.relayWake <- struct{}{}:
		default:
		}
		c.stateMu.Unlock()
		c.controlMu.Lock()
		if c.relayCancel != nil {
			c.relayCancel()
		}
		if c.controlCancel != nil {
			c.controlCancel()
		}
		c.controlMu.Unlock()
		for {
			relayWatchEvent(c, "awaiting-call-lock")
			c.mu.Lock()
			if p.controlAuthority() {
				c.stateMu.Lock()
				lease := c.lease
				c.stateMu.Unlock()
				p.endRelayFailure(c, lease, "relay_change_failed")
				p.finishRelayEpisode(context.Background(), c, e)
				c.mu.Unlock()
				return
			}
			// Authority ended while the operation unwound. Release the call
			// and keep watching: the same watcher ends the call once authority
			// returns, so the episode is never left without one.
			c.mu.Unlock()
			relayWatchEvent(c, "suspended")
			if !p.awaitRelayAuthority(c, e) {
				return
			}
		}
	}()
}

// relayWatchHooks is a test barrier for watcher transitions; unset in
// production.
var relayWatchHooks atomic.Pointer[func(*call, string)]

func relayWatchEvent(c *call, name string) {
	if hook := relayWatchHooks.Load(); hook != nil {
		(*hook)(c, name)
	}
}

// awaitRelayAuthority waits until this control holds authority. It reports
// false when the episode finished or control is stopping.
func (p *Plane) awaitRelayAuthority(c *call, e *relayEpisode) bool {
	for !p.controlAuthority() {
		if p.restart != nil && p.restart.stopping.Load() {
			return false
		}
		// A call that ended (for example a hangup admitted before authority
		// lapsed) needs no watcher: close the episode instead of waiting.
		p.mu.Lock()
		gone := c.terminal || c.hungUp.Load()
		p.mu.Unlock()
		if gone {
			p.finishRelayEpisode(context.Background(), c, e)
			relayWatchEvent(c, "gone-while-suspended")
			return false
		}
		select {
		case <-e.done:
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return true
}

// completeRelayEpisode records a driver's success. After the watchdog's
// terminal decision it refuses with errRelayEpisodeEnded, and the caller must
// report failure: the call is ending at its deadline.
func (p *Plane) completeRelayEpisode(ctx context.Context, c *call, e *relayEpisode) error {
	if !e.markCompleted() {
		relayWatchEvent(c, "completion-refused")
		return errRelayEpisodeEnded
	}
	p.cleanupRelayEpisode(ctx, c, e)
	return nil
}

// finishRelayEpisode closes an episode that the watchdog ended, or whose call
// is gone, regardless of the terminal decision.
func (p *Plane) finishRelayEpisode(ctx context.Context, c *call, e *relayEpisode) {
	e.completed.Store(true)
	e.once.Do(func() { close(e.done) })
	p.cleanupRelayEpisode(ctx, c, e)
}

func (p *Plane) cleanupRelayEpisode(ctx context.Context, c *call, e *relayEpisode) {
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	e.cleanupReady = true
	journal := p.store.(sessionstore.ControlStore)
	e.intent.Kind = "relay-recovery-complete"
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if journal.PutControlIntent(cleanup, e.intent) == nil && journal.DeleteControlIntent(cleanup, e.intent.ID, e.intent.Token) == nil && c.relayEpisode == e {
		c.relayEpisode = nil
	}
}

func (p *Plane) startRelayRecovery(ctx context.Context, c *call, sameSlot bool) error {
	return p.startRelayRecoveryFrom(ctx, c, sameSlot, time.Now(), nil)
}

// startRelayRecoveryFrom starts or joins the call's loss episode. seed names
// slots already tried in it, such as an adopted unconfirmed attempt's slot.
// The tried set is never cleared: an exhausted episode waits for an untried
// live slot or its fixed deadline (ADR 0016 section 7).
func (p *Plane) startRelayRecoveryFrom(ctx context.Context, c *call, sameSlot bool, detected time.Time, seed []string) error {
	p.mu.Lock()
	terminal := c.terminal || c.hungUp.Load()
	p.mu.Unlock()
	c.stateMu.Lock()
	reason := c.relayEndReason
	c.stateMu.Unlock()
	if terminal || reason != "" {
		return sessionstore.ErrEnded
	}
	e, err := p.relayRecoveryEpisode(ctx, c, detected)
	if err != nil {
		return err
	}
	if !time.Now().Before(e.deadline) {
		return sessionstore.ErrEnded
	}
	c.relayEpisodeMu.Lock()
	if e.running {
		c.relayEpisodeMu.Unlock()
		return nil
	}
	e.running = true
	c.relayEpisodeMu.Unlock()
	go func() {
		defer func() { c.relayEpisodeMu.Lock(); e.running = false; c.relayEpisodeMu.Unlock() }()
		recovery, cancel := context.WithDeadline(context.WithoutCancel(ctx), e.deadline)
		defer cancel()
		tried := map[string]bool{}
		for _, slot := range seed {
			tried[slot] = true
		}
		for recovery.Err() == nil {
			err = p.recoverRelay(recovery, c, sameSlot, tried, e.deadline, e.intent.Started)
			if err == nil || errors.Is(err, sessionstore.ErrEnded) || errors.Is(err, sessionstore.ErrNotFound) {
				_ = p.completeRelayEpisode(recovery, c, e) // a refusal: the watchdog ends the call
				return
			}
			select {
			case <-recovery.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	return nil
}

func (p *Plane) callerRelayRestart(ctx context.Context, c *call) (RelayEvent, error) {
	p.mu.Lock()
	enabled := len(p.relaySet) > 0
	p.mu.Unlock()
	if !enabled {
		return RelayEvent{}, mediaworker.ErrRelayUnsupported
	}
	if err := p.requireMutationReady(); err != nil {
		return RelayEvent{}, err
	}
	mode, err := p.relayMode(ctx, c)
	if err != nil {
		return RelayEvent{}, err
	}
	if mode != sessionstore.RelayConfirmDelivery && mode != sessionstore.RelayConfirmDecode {
		return RelayEvent{}, mediaworker.ErrRelayUnsupported
	}
	c.stateMu.Lock()
	event := c.relayEvent
	c.stateMu.Unlock()
	var stalled uint64
	if event != nil {
		// A stall after the switch commit fails that unconfirmed attempt at
		// once; its operation then issues a higher generation (section 7).
		_, got, err := p.store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.id)
		if err != nil {
			return RelayEvent{}, err
		}
		if got.Pending == "" && got.Generation >= event.Generation {
			stalled = event.Generation
			c.stateMu.Lock()
			c.relayStall = stalled
			c.stateMu.Unlock()
		}
	}
	if event == nil || time.Now().After(event.Deadline) {
		if err := p.startRelayRecovery(ctx, c, true); err != nil {
			return RelayEvent{}, err
		}
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		c.stateMu.Lock()
		event, reason := c.relayEvent, c.relayEndReason
		c.stateMu.Unlock()
		if reason != "" {
			return RelayEvent{}, sessionstore.ErrEnded
		}
		if event != nil && time.Now().Before(event.Deadline) && event.Generation > stalled {
			return *event, nil
		}
		select {
		case <-ctx.Done():
			return RelayEvent{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// Completion is the mode's confirmation plus the matching fenced commit
// (ADR 0016 section 7); a commit alone never ends the episode.
func (p *Plane) recoverRelay(ctx context.Context, c *call, sameSlot bool, tried map[string]bool, deadline, detected time.Time) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := c.lockOperatorCancel(ctx, false, cancel, true); err != nil {
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
	}()
	if c.terminal || c.hungUp.Load() {
		return sessionstore.ErrEnded
	}
	a := p.store.(sessionstore.RelayAssignments)
	lease, from, err := a.RelayOwner(ctx, c.id)
	if err != nil {
		return err
	}
	c.rememberLease(lease)
	if from.Pending != "" {
		from, err = p.abortRelayPending(ctx, lease, from)
		if err != nil {
			return err
		}
	}
	mode, err := p.relayMode(ctx, c)
	if err != nil {
		return err
	}
	if mode != sessionstore.RelayConfirmDelivery && mode != sessionstore.RelayConfirmDecode {
		p.endRelayFailure(c, lease, "relay_lost")
		return nil
	}
	opt := relayAttemptOptions{kind: "recovery", outer: deadline, detected: detected}
	p.mu.Lock()
	currentLost := p.relaySet[from.Slot].lost
	p.mu.Unlock()
	if !currentLost {
		// A committed attempt still awaiting confirmation (for example after a
		// control restart) is adopted with its saved bound, not superseded.
		if adopted, err := p.adoptRelayAttempt(ctx, c, from, opt); adopted || err != nil {
			tried[from.Slot] = true
			return err
		}
	}
	target := ""
	p.mu.Lock()
	if sameSlot && !p.relaySet[from.Slot].lost && !tried[from.Slot] {
		target = from.Slot
	}
	if target == "" {
		for id, b := range p.relaySet {
			if !b.lost && !b.draining && id != from.Slot && !tried[id] {
				target = id
				break
			}
		}
	}
	p.mu.Unlock()
	if target == "" {
		return ErrNoTarget
	}
	c.controlMu.Lock()
	c.relayTarget = target
	c.controlMu.Unlock()
	pending := from
	pending.Pending = target
	if err = p.setRelay(ctx, lease, from, pending); err != nil {
		return err
	}
	tried[target] = true
	bound := 4 * time.Second
	if sameSlot && target == from.Slot {
		bound = time.Second
	}
	opt.tried = slices.Sorted(maps.Keys(tried))
	err = p.waitRelayChange(ctx, c, pending, bound, opt)
	if err == nil || errors.Is(err, errRelayUnconfirmed) {
		return err // an unconfirmed attempt retries on an untried slot
	}
	if errors.Is(err, ErrConfirmationUnsupported) {
		_, abortErr := p.abortRelayPending(ctx, lease, pending)
		c.stateMu.Lock()
		c.relayEndDetail = RelayEvent{Detail: "confirmation_unsupported", Confirmation: mode}
		c.stateMu.Unlock()
		p.endRelayFailure(c, lease, "relay_change_failed")
		return abortErr
	}
	_, abortErr := p.abortRelayPending(ctx, lease, pending)
	if errors.Is(abortErr, mediaworker.ErrRelaySwitched) {
		return p.waitRelayChange(ctx, c, pending, time.Until(deadline), opt)
	}
	return errors.Join(err, abortErr)
}
