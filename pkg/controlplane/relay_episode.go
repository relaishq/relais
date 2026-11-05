package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/sessionstore"
)

const (
	relayIntentKind            = "relay-recovery"
	relayIntentKindUnsupported = "relay-recovery-unsupported"
	// relayDecisionLimit bounds one decision attempt: read, authority
	// conversion, compare-and-set and read-back share it (ADR 0016 §7).
	relayDecisionLimit = 2 * time.Second
)

// relayEpisode is the in-memory side of one loss episode. The durable decision
// lives in the call's confirmation record (sessionstore.RelayEpisode): this
// struct only drives it. intent is the journal proposal (token, seq, Started).
type relayEpisode struct {
	intentMu               sync.Mutex
	intent                 sessionstore.ControlIntent // guarded by intentMu; a re-proposal replaces it
	deadline               time.Time
	done                   chan struct{}
	once                   sync.Once
	armed                  chan struct{}
	armOnce                sync.Once
	running, loaded        bool // guarded by relayEpisodeMu
	persisted, cleanupDone bool // guarded by relayEpisodeMu
	completed              atomic.Bool
}

func newRelayEpisode(intent sessionstore.ControlIntent) *relayEpisode {
	return &relayEpisode{intent: intent, deadline: intent.Started.Add(relayRecoveryLimit), done: make(chan struct{}), armed: make(chan struct{})}
}

func (e *relayEpisode) arm()   { e.armOnce.Do(func() { close(e.armed) }) }
func (e *relayEpisode) close() { e.completed.Store(true); e.once.Do(func() { close(e.done) }) }
func (e *relayEpisode) unsupported() bool {
	return e.proposal().Kind == relayIntentKindUnsupported
}

// proposal returns the episode's current journal proposal.
func (e *relayEpisode) proposal() sessionstore.ControlIntent {
	e.intentMu.Lock()
	defer e.intentMu.Unlock()
	return e.intent
}

func (e *relayEpisode) setProposal(next sessionstore.ControlIntent) {
	e.intentMu.Lock()
	e.intent = next
	e.intentMu.Unlock()
}

func randomEpisodeToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// relayOutcome is what one decision attempt established for an episode.
type relayOutcome int

const (
	relayUncertain relayOutcome = iota // retry: the store could not decide
	relayArmed                         // our token is open
	relayCompleted                     // our token completed
	relayEnded                         // our token ended
	relayStale                         // our proposal can never begin
	relayStandDown                     // another token owns the decision
	relayGone                          // the session record is gone
)

// relayRecoveryEpisode creates or joins the call's episode and journals its
// proposal. The journal stores the first detection time, so a busy operation
// or a repeated caller request cannot buy another 15 seconds. The episode is
// armed asynchronously by its watcher once begin is definite.
func (p *Plane) relayRecoveryEpisode(ctx context.Context, c *call, detected time.Time, snapshot ...map[string]sessionstore.ControlIntent) (*relayEpisode, error) {
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	journal := p.store.(sessionstore.ControlStore)
	if e := c.relayEpisode; e != nil && e.completed.Load() {
		if !e.cleanupDone {
			return nil, ErrMoveInProgress
		}
		c.relayEpisode = nil
	}
	if c.relayEpisode == nil {
		c.stateMu.Lock()
		lease, capable := c.lease, c.relayCapable
		c.stateMu.Unlock()
		intent := sessionstore.ControlIntent{ID: "relay-recovery:" + c.id, Token: randomEpisodeToken(), Kind: relayIntentKindUnsupported, Started: detected, Lease: lease}
		if capable {
			intent.Kind = relayIntentKind
			// An unreadable seq is resolved by the watcher before begin; the
			// detection time is journaled now either way.
			if seq, err := p.relaySeq(ctx, c); err == nil {
				intent.Seq = seq + 1
			}
		}
		e := newRelayEpisode(intent)
		c.relayEpisode = e
		p.watchRelayEpisode(c, e)
	}
	e := c.relayEpisode
	if !e.loaded {
		var records []sessionstore.ControlIntent
		if len(snapshot) > 0 {
			if saved, ok := snapshot[0][e.proposal().ID]; ok {
				records = append(records, saved)
			}
		} else {
			var err error
			if records, err = journal.ControlIntents(ctx); err != nil {
				return nil, err
			}
		}
		for _, saved := range records {
			if saved.ID != e.proposal().ID || saved.Token == e.proposal().Token {
				continue
			}
			if saved.Kind == relayIntentKind && saved.Seq == 0 || saved.Kind != relayIntentKind && saved.Kind != relayIntentKindUnsupported {
				// Unknown format: never adopted; the stall and end paths remain.
				if err := journal.DeleteControlIntent(ctx, saved.ID, saved.Token); err != nil {
					return nil, err
				}
				continue
			}
			// The journaled proposal keeps its first detection time, so a
			// reload cannot buy another 15 s. Its watcher begins or discards it.
			e.close()
			e = newRelayEpisode(saved)
			c.relayEpisode = e
			p.watchRelayEpisode(c, e)
		}
		e.loaded = true
	}
	err := journal.PutControlIntent(ctx, e.proposal())
	if err == nil {
		e.persisted = true
	}
	return e, err
}

// relaySeq reads the call's decision counter.
func (p *Plane) relaySeq(ctx context.Context, c *call) (uint64, error) {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return 0, nil
	}
	_, rec, err := store.RelayConfirmation(ctx, c.id)
	if err != nil || rec.Episode == nil {
		return 0, err
	}
	return rec.Episode.Seq, nil
}

func (p *Plane) callGone(c *call) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return c.terminal || c.hungUp.Load()
}

// watchRelayEpisode arms the episode, then enforces its deadline. Every
// decision is one compare-and-set on the confirmation record; the watcher
// follows the stored outcome and publishes ended only after a definite end.
func (p *Plane) watchRelayEpisode(c *call, e *relayEpisode) {
	go func() {
		if e.unsupported() {
			p.watchUnsupportedEpisode(c, e)
			return
		}
		outcome := p.armRelayEpisode(c, e)
		if outcome == relayArmed {
			timer := time.NewTimer(time.Until(e.deadline))
			select {
			case <-e.done:
				timer.Stop()
				return
			case <-timer.C:
			}
			outcome = p.decideRelayEnd(c, e)
		}
		var detail RelayEvent
		switch outcome {
		case relayEnded:
			detail = p.relayEndedDetail(c)
		case relayCompleted, relayGone, relayStale:
			p.cleanupRelayEpisode(context.Background(), c, e)
			return
		case relayStandDown:
			p.standDownRelayEpisode(c, e)
			return
		default: // shutdown or a finished episode
			return
		}
		p.publishRelayEnd(c, detail)
		p.finishRelayEnd(c, e)
	}()
}

// armRelayEpisode retries begin until it is definite, past the deadline too:
// an episode that arms late is decided at once. A refused proposal is
// re-proposed within this term while the call's current slot is still lost.
func (p *Plane) armRelayEpisode(c *call, e *relayEpisode) relayOutcome {
	for backoff := 50 * time.Millisecond; ; backoff = min(2*backoff, time.Second) {
		if !p.awaitRelayAuthority(c, e) {
			return relayUncertain
		}
		if p.callGone(c) {
			return relayGone
		}
		ctx, cancel := context.WithTimeout(context.Background(), relayDecisionLimit)
		outcome := relayUncertain
		if p.resolveRelaySeq(ctx, c, e) {
			outcome = p.beginRelayEpisode(ctx, c, e)
		}
		cancel()
		switch outcome {
		case relayArmed:
			e.arm()
			relayWatchEvent(c, "armed")
			return relayArmed
		case relayStale:
			if p.reproposeRelayEpisode(c, e) {
				continue
			}
			return relayStale
		case relayUncertain:
		default:
			return outcome
		}
		select {
		case <-e.done:
			return relayUncertain
		case <-time.After(backoff):
		}
	}
}

// resolveRelaySeq fills a proposal whose seq could not be read when it was
// journaled, and re-journals it under the same token.
func (p *Plane) resolveRelaySeq(ctx context.Context, c *call, e *relayEpisode) bool {
	c.relayEpisodeMu.Lock()
	seq := e.proposal().Seq
	c.relayEpisodeMu.Unlock()
	if seq != 0 {
		return true
	}
	current, err := p.relaySeq(ctx, c)
	if err != nil {
		return false
	}
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	next := e.proposal()
	next.Seq = current + 1
	if p.store.(sessionstore.ControlStore).PutControlIntent(ctx, next) != nil {
		return false
	}
	e.setProposal(next)
	return true
}

// reproposeRelayEpisode replaces a refused proposal with a fresh token at the
// current seq + 1, but only while the call still needs recovery (its current
// slot is lost). Otherwise the proposal is discarded.
func (p *Plane) reproposeRelayEpisode(c *call, e *relayEpisode) bool {
	ctx, cancel := context.WithTimeout(context.Background(), relayDecisionLimit)
	defer cancel()
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return false
	}
	a, rec, err := store.RelayConfirmation(ctx, c.id)
	if err != nil || rec.Episode != nil && rec.Episode.Outcome != sessionstore.RelayEpisodeCompleted && rec.Episode.Outcome != "" {
		return false
	}
	p.mu.Lock()
	lost := p.relaySet[a.Slot].lost
	p.mu.Unlock()
	if !lost {
		return false
	}
	journal := p.store.(sessionstore.ControlStore)
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	next := e.proposal()
	next.Token = randomEpisodeToken()
	next.Seq = 1
	if rec.Episode != nil {
		next.Seq = rec.Episode.Seq + 1
	}
	if journal.DeleteControlIntent(ctx, e.proposal().ID, e.proposal().Token) != nil || journal.PutControlIntent(ctx, next) != nil {
		return false
	}
	e.setProposal(next)
	relayWatchEvent(c, "reproposed")
	return true
}

// beginRelayEpisode opens the proposal's episode with one compare-and-set.
// The same write binds the call's current attempt when it is eligible: still
// pending or switched, owned by no episode, and aimed at a live slot (an
// in-flight switch to a live target is eligible even if its source is lost).
func (p *Plane) beginRelayEpisode(ctx context.Context, c *call, e *relayEpisode) relayOutcome {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return relayStale
	}
	a, rec, err := store.RelayConfirmation(ctx, c.id)
	if errors.Is(err, sessionstore.ErrNotFound) {
		return relayGone
	}
	if err != nil {
		return relayUncertain
	}
	ep, token := rec.Episode, e.proposal().Token
	if ep != nil && ep.Token == token {
		switch ep.Outcome {
		case sessionstore.RelayEpisodeOpen:
			return relayArmed
		case sessionstore.RelayEpisodeCompleted:
			return relayCompleted
		case sessionstore.RelayEpisodeEnded:
			return relayEnded
		}
	}
	if ep != nil && (ep.Open() || ep.Outcome == sessionstore.RelayEpisodeEnded) {
		return relayStandDown // another episode owns the decision; ended is final
	}
	var seq uint64
	if ep != nil {
		seq = ep.Seq
	}
	if e.proposal().Seq != seq+1 || rec.Mode != sessionstore.RelayConfirmDelivery && rec.Mode != sessionstore.RelayConfirmDecode {
		return relayStale
	}
	storeDeadline, err := p.storeClockOf(ctx, c.id, e.deadline)
	if err != nil {
		return relayUncertain
	}
	next := rec
	next.Episode = &sessionstore.RelayEpisode{Seq: e.proposal().Seq, Token: token, StoreDeadline: storeDeadline, Outcome: sessionstore.RelayEpisodeOpen}
	if t := rec.Attempt; t != nil && p.eligibleForEpisode(*t) {
		bound := clampToEpisode(*t, e.deadline, storeDeadline)
		bound.Episode = token
		next.Attempt = &bound
	}
	if err = p.decisionCAS(ctx, c.id, a, rec, next, time.Time{}, time.Time{}); err != nil {
		return relayUncertain // the next read settles conflicts and refusals
	}
	return relayArmed
}

// eligibleForEpisode: an unfinished attempt owned by no episode whose target
// slot is live. Historical confirmations are never adopted.
func (p *Plane) eligibleForEpisode(t sessionstore.RelayAttempt) bool {
	if t.Episode != "" || t.Phase != sessionstore.RelayPhasePending && t.Phase != sessionstore.RelayPhaseSwitched {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b, known := p.relaySet[t.Slot]
	return known && !b.lost
}

// clampToEpisode binds an attempt's bounds to an episode: every bound keeps or
// moves earlier, never later.
func clampToEpisode(t sessionstore.RelayAttempt, deadline, storeDeadline time.Time) sessionstore.RelayAttempt {
	if t.EpisodeDeadline.IsZero() || deadline.Before(t.EpisodeDeadline) {
		t.EpisodeDeadline = deadline
	}
	if !t.AttemptBound.IsZero() && deadline.Before(t.AttemptBound) {
		t.AttemptBound = deadline
	}
	if t.StoreDeadline.IsZero() || storeDeadline.Before(t.StoreDeadline) {
		t.StoreDeadline = storeDeadline
	}
	if !t.StoreBound.IsZero() && storeDeadline.Before(t.StoreBound) {
		t.StoreBound = storeDeadline
	}
	return t
}

// decideRelayEnd writes the episode's end at its deadline, retrying uncertain
// results with backoff under authority. Proof for an attempt owned by the
// episode is promoted instead, in the same compare-and-set.
func (p *Plane) decideRelayEnd(c *call, e *relayEpisode) relayOutcome {
	for backoff := 50 * time.Millisecond; ; backoff = min(2*backoff, time.Second) {
		if !p.awaitRelayAuthority(c, e) {
			return relayUncertain
		}
		if p.callGone(c) {
			return relayGone
		}
		ctx, cancel := context.WithTimeout(context.Background(), relayDecisionLimit)
		outcome := p.endRelayEpisode(ctx, c, e.proposal().Token, "relay_change_failed", true)
		cancel()
		if outcome != relayUncertain {
			return outcome
		}
		select {
		case <-e.done:
			return relayUncertain
		case <-time.After(backoff):
		}
	}
}

// endRelayEpisode is the one end operation, for the timeout and for immediate
// failures. It requires the token to be open; a timeout also requires the
// store clock to have reached the episode deadline.
func (p *Plane) endRelayEpisode(ctx context.Context, c *call, token, reason string, timeout bool) relayOutcome {
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		return relayStandDown
	}
	a, rec, err := store.RelayConfirmation(ctx, c.id)
	if errors.Is(err, sessionstore.ErrNotFound) {
		return relayGone
	}
	if err != nil {
		return relayUncertain
	}
	ep := rec.Episode
	if ep == nil || ep.Token != token {
		return relayStandDown
	}
	switch ep.Outcome {
	case sessionstore.RelayEpisodeCompleted:
		return relayCompleted
	case sessionstore.RelayEpisodeEnded:
		return relayEnded
	}
	next := rec
	episode := *ep
	next.Episode = &episode
	t := rec.Attempt
	if t != nil && t.Episode == token && rec.Confirmed(a) {
		promoted := *t
		promoted.Phase = sessionstore.RelayPhaseConfirmed
		next.Attempt = &promoted
		episode.Outcome, episode.Seq = sessionstore.RelayEpisodeCompleted, episode.Seq+1
		if p.decisionCAS(ctx, c.id, a, rec, next, time.Time{}, time.Time{}) != nil {
			return relayUncertain
		}
		return relayCompleted
	}
	detail := RelayEvent{Detail: "delivery_unconfirmed", Confirmation: rec.Mode}
	if t != nil && t.Episode == token {
		detail = relayFailureDetail(rec)
		if t.Phase == sessionstore.RelayPhasePending || t.Phase == sessionstore.RelayPhaseSwitched {
			expired := *t
			expired.Phase = sessionstore.RelayPhaseExpired
			next.Attempt = &expired
		}
	}
	if reason == "confirmation_unsupported" {
		detail.Detail = reason
		reason = "relay_change_failed"
	}
	episode.Outcome, episode.Reason, episode.Detail = sessionstore.RelayEpisodeEnded, reason, detail.Detail
	var notBefore time.Time
	if timeout {
		notBefore = ep.StoreDeadline
	}
	if p.decisionCAS(ctx, c.id, a, rec, next, notBefore, time.Time{}) != nil {
		return relayUncertain
	}
	c.stateMu.Lock()
	if c.relayEndDetail.Detail == "" {
		c.relayEndDetail = detail
	}
	c.stateMu.Unlock()
	return relayEnded
}

// relayEndedDetail returns the detail saved with the ended outcome.
func (p *Plane) relayEndedDetail(c *call) RelayEvent {
	c.stateMu.Lock()
	detail := c.relayEndDetail
	c.stateMu.Unlock()
	if detail.Detail != "" {
		return detail
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayDecisionLimit)
	defer cancel()
	if store, ok := p.store.(sessionstore.RelayConfirmations); !ok {
		return RelayEvent{Detail: "delivery_unconfirmed"}
	} else if _, rec, err := store.RelayConfirmation(ctx, c.id); err == nil && rec.Episode != nil {
		return RelayEvent{Detail: rec.Episode.Detail, Confirmation: rec.Mode}
	}
	return RelayEvent{Detail: "delivery_unconfirmed"}
}

// publishRelayEnd tells the caller after a definite end. It never waits for
// the call lock, so a busy operation cannot delay it.
func (p *Plane) publishRelayEnd(c *call, detail RelayEvent) {
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
	relayWatchEvent(c, "published")
}

// finishRelayEnd cancels the running operation and ends the session under the
// call lock. While authority is absent it releases the call and keeps
// watching; a call that ends meanwhile closes the episode.
func (p *Plane) finishRelayEnd(c *call, e *relayEpisode) {
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
			c.mu.Unlock()
			p.cleanupRelayEpisode(context.Background(), c, e)
			return
		}
		c.mu.Unlock()
		relayWatchEvent(c, "suspended")
		if !p.awaitRelayAuthority(c, e) {
			return
		}
	}
}

// watchUnsupportedEpisode is the detection-based watchdog for calls without
// relay change (mode none). Such calls write no confirmation record and have
// no success path, so the deadline ends them directly, without begin.
func (p *Plane) watchUnsupportedEpisode(c *call, e *relayEpisode) {
	timer := time.NewTimer(time.Until(e.deadline))
	select {
	case <-e.done:
		timer.Stop()
		return
	case <-timer.C:
	}
	if !p.awaitRelayAuthority(c, e) || e.completed.Load() {
		return
	}
	if p.callGone(c) {
		p.cleanupRelayEpisode(context.Background(), c, e)
		return
	}
	p.publishRelayEnd(c, RelayEvent{})
	p.finishRelayEnd(c, e)
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
// false when the episode finished, control is stopping, or the call ended
// (the episode is then cleaned up).
func (p *Plane) awaitRelayAuthority(c *call, e *relayEpisode) bool {
	for !p.controlAuthority() {
		if p.restart != nil && p.restart.stopping.Load() {
			return false
		}
		if p.callGone(c) {
			p.cleanupRelayEpisode(context.Background(), c, e)
			relayWatchEvent(c, "gone-while-suspended")
			return false
		}
		select {
		case <-e.done:
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return !e.completed.Load()
}

// completeRelayEpisode is a driver's report after its confirmation write. The
// decision is already durable: completion was recorded with the confirmation.
// It refuses with errRelayEpisodeEnded when the episode ended first.
func (p *Plane) completeRelayEpisode(ctx context.Context, c *call, e *relayEpisode) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayDecisionLimit)
	defer cancel()
	store, ok := p.store.(sessionstore.RelayConfirmations)
	if !ok {
		p.cleanupRelayEpisode(ctx, c, e)
		return nil
	}
	_, rec, err := store.RelayConfirmation(ctx, c.id)
	if err == nil && rec.Episode != nil && rec.Episode.Token == e.proposal().Token {
		switch rec.Episode.Outcome {
		case sessionstore.RelayEpisodeEnded:
			return errRelayEpisodeEnded
		case sessionstore.RelayEpisodeOpen:
			return ErrMoveInProgress
		}
	}
	if err != nil && !errors.Is(err, sessionstore.ErrNotFound) {
		return err
	}
	p.cleanupRelayEpisode(ctx, c, e)
	return nil
}

// cleanupRelayEpisode closes the episode and deletes its journal proposal.
func (p *Plane) cleanupRelayEpisode(ctx context.Context, c *call, e *relayEpisode) {
	e.close()
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	journal := p.store.(sessionstore.ControlStore)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	err := journal.DeleteControlIntent(cleanup, e.proposal().ID, e.proposal().Token)
	if err == nil || errors.Is(err, sessionstore.ErrLeaseLost) {
		e.cleanupDone = true
		if c.relayEpisode == e {
			c.relayEpisode = nil
		}
	}
}

// standDownRelayEpisode leaves the decision to the episode that owns it: no
// publication and no cleanup of another token's journal entry.
func (p *Plane) standDownRelayEpisode(c *call, e *relayEpisode) {
	e.close()
	c.relayEpisodeMu.Lock()
	defer c.relayEpisodeMu.Unlock()
	e.cleanupDone = true
	if c.relayEpisode == e {
		c.relayEpisode = nil
	}
	relayWatchEvent(c, "stood-down")
}

func (p *Plane) startRelayRecovery(ctx context.Context, c *call, sameSlot bool) error {
	return p.startRelayRecoveryFrom(ctx, c, sameSlot, time.Now(), nil)
}

// startRelayRecoveryFrom starts or joins the call's loss episode. seed names
// slots already tried in it. The tried set is never cleared: an exhausted
// episode waits for an untried live slot or its fixed deadline (ADR 0016 §7).
// Only an ended call refuses; a completed episode permits a later recovery,
// and an unarmed or expired-but-open one is joined (its watcher decides).
func (p *Plane) startRelayRecoveryFrom(ctx context.Context, c *call, sameSlot bool, detected time.Time, seed []string) error {
	c.stateMu.Lock()
	reason := c.relayEndReason
	c.stateMu.Unlock()
	if p.callGone(c) || reason != "" {
		return sessionstore.ErrEnded
	}
	e, err := p.relayRecoveryEpisode(ctx, c, detected)
	if err != nil {
		return err
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
		if !e.unsupported() {
			select { // attempts are owned by the episode once it is armed
			case <-e.armed:
			case <-e.done:
				return
			}
		}
		recovery, cancel := context.WithDeadline(context.WithoutCancel(ctx), e.deadline)
		defer cancel()
		tried := map[string]bool{}
		for _, slot := range seed {
			tried[slot] = true
		}
		for recovery.Err() == nil {
			err := p.recoverRelay(recovery, c, sameSlot, tried, e)
			if err == nil {
				_ = p.completeRelayEpisode(recovery, c, e)
				return
			}
			if errors.Is(err, sessionstore.ErrEnded) || errors.Is(err, sessionstore.ErrNotFound) {
				if p.callGone(c) { // nothing is left to decide; release the watcher
					p.cleanupRelayEpisode(recovery, c, e)
				}
				return
			}
			if errors.Is(err, errRelayEpisodeEnded) {
				return // the watcher follows the stored outcome
			}
			select {
			case <-recovery.Done():
				return
			case <-e.done:
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

// recoverRelay runs one attempt of the episode. Completion is the mode's
// confirmation plus the matching fenced commit on an attempt owned by the
// episode (ADR 0016 section 7); a commit alone never ends the episode.
func (p *Plane) recoverRelay(ctx context.Context, c *call, sameSlot bool, tried map[string]bool, e *relayEpisode) error {
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
	opt := relayAttemptOptions{kind: "recovery", outer: e.deadline, detected: e.proposal().Started}
	p.mu.Lock()
	currentLost := p.relaySet[from.Slot].lost
	p.mu.Unlock()
	if !currentLost {
		// A committed attempt still awaiting confirmation (for example after a
		// control restart, or an in-flight switch to a live slot) is adopted
		// with its saved bound, not superseded.
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
	if err == nil || errors.Is(err, errRelayUnconfirmed) || errors.Is(err, errRelayEpisodeEnded) {
		return err // an unconfirmed attempt retries on an untried slot
	}
	if errors.Is(err, ErrConfirmationUnsupported) {
		_, abortErr := p.abortRelayPending(ctx, lease, pending)
		// An immediate end goes through the same decision; existing proof
		// would be promoted instead.
		end, cancelEnd := context.WithTimeout(context.WithoutCancel(ctx), relayDecisionLimit)
		outcome := p.endRelayEpisode(end, c, e.proposal().Token, "confirmation_unsupported", false)
		cancelEnd()
		if outcome == relayEnded {
			p.publishRelayEnd(c, p.relayEndedDetail(c))
			p.endRelayFailure(c, lease, "relay_change_failed")
			p.cleanupRelayEpisode(ctx, c, e)
			return errRelayEpisodeEnded
		}
		return errors.Join(abortErr, ErrConfirmationUnsupported)
	}
	_, abortErr := p.abortRelayPending(ctx, lease, pending)
	if errors.Is(abortErr, mediaworker.ErrRelaySwitched) {
		return p.waitRelayChange(ctx, c, pending, time.Until(e.deadline), opt)
	}
	return errors.Join(err, abortErr)
}
