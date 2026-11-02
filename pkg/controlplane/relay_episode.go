package controlplane

import (
	"context"
	"errors"
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
	completed                     atomic.Bool
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
	return e, journal.PutControlIntent(ctx, e.intent)
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
		for !p.controlAuthority() {
			if p.restart != nil && p.restart.stopping.Load() {
				return
			}
			select {
			case <-e.done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		if e.completed.Load() {
			return
		}
		// Publish the terminal event at the deadline even while a cancelled
		// operation is unwinding; ownership cleanup still uses the call lock.
		c.stateMu.Lock()
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
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stateMu.Lock()
		lease := c.lease
		c.stateMu.Unlock()
		p.endRelayFailure(c, lease, "relay_change_failed")
		p.completeRelayEpisode(context.Background(), c, e)
	}()
}

func (p *Plane) completeRelayEpisode(ctx context.Context, c *call, e *relayEpisode) {
	e.completed.Store(true)
	e.once.Do(func() { close(e.done) })
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
	p.mu.Lock()
	terminal := c.terminal || c.hungUp.Load()
	p.mu.Unlock()
	c.stateMu.Lock()
	reason := c.relayEndReason
	c.stateMu.Unlock()
	if terminal || reason != "" {
		return sessionstore.ErrEnded
	}
	e, err := p.relayRecoveryEpisode(ctx, c, time.Now())
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
		for recovery.Err() == nil {
			err = p.recoverRelay(recovery, c, sameSlot, tried)
			if errors.Is(err, ErrNoTarget) {
				clear(tried)
			}
			if err == nil || errors.Is(err, sessionstore.ErrEnded) || errors.Is(err, sessionstore.ErrNotFound) {
				p.completeRelayEpisode(recovery, c, e)
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
	c.stateMu.Lock()
	capable, event := c.relayCapable, c.relayEvent
	c.stateMu.Unlock()
	if !capable {
		return RelayEvent{}, mediaworker.ErrRelayUnsupported
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
		if event != nil && time.Now().Before(event.Deadline) {
			return *event, nil
		}
		select {
		case <-ctx.Done():
			return RelayEvent{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// Completion deliberately remains the slice-1 fenced assignment commit.
// Caller media-progress acceptance (§7 / row 6) is parked by the owner.
func (p *Plane) recoverRelay(ctx context.Context, c *call, sameSlot bool, tried map[string]bool) error {
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
	c.stateMu.Lock()
	capable := c.relayCapable
	c.stateMu.Unlock()
	if !capable {
		p.endRelayFailure(c, lease, "relay_lost")
		return nil
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
	if err = p.waitRelayChange(ctx, c, pending, bound); err == nil {
		return nil
	}
	_, abortErr := p.abortRelayPending(ctx, lease, pending)
	if errors.Is(abortErr, mediaworker.ErrRelaySwitched) {
		deadline, _ := ctx.Deadline()
		return p.waitRelayChange(ctx, c, pending, time.Until(deadline))
	}
	return errors.Join(err, abortErr)
}
