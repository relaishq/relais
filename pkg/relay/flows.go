package relay

import (
	"container/list"
	"encoding/binary"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/stun/v4"
)

// maxOutstandingChecks is how many unanswered binding requests a caller's
// pending candidate, and separately its confirmed route, remember. A
// binding success counts only if it answers one of them.
const maxOutstandingChecks = 8

// flowTable maps each caller address to the worker its packets go to. It is
// a cache of the session-owner store, and no routing changes for a caller
// until the worker authenticates a binding request or the trusted control
// plane calls moveSession. A bounded hold brackets that move: caller packets
// wait for the new owner to resume, while worker replies continue flowing.
// The routing rules are:
//
//   - A caller has at most one confirmed route, which carries all of its
//     packets, and at most one pending candidate, which carries only its
//     binding requests for the candidate's session.
//   - A binding request the store routes to a worker the caller has no
//     confirmed route to becomes (or joins) the candidate. The confirmed
//     route, if any, is left alone.
//   - The candidate's worker answers with a STUN binding success only after
//     checking the request's ICE credentials. A binding success from that
//     worker whose transaction ID is one of the candidate's requests
//     promotes the candidate, unless the route is active for another
//     session (below). Nothing else does on the packet path: only trusted
//     moveSession bypasses promotion. The relay reads only STUN headers on
//     the worker leg.
//   - A confirmed route is active while its worker answers the route
//     session's own binding requests from the caller: a binding success
//     for one of the route's outstanding requests renews it, as of the
//     request's arrival. Within stickinessWindow of the last one, no
//     candidate for another session is admitted or promoted, so valid
//     credentials for another session cannot take an active caller's
//     address. Media, unanswered requests, and unmatched or replayed
//     successes renew nothing. The route's own session may always move.
//   - Candidates expire after the pending timeout. Routes expire after the
//     idle timeout without caller packets, but not while active. At most
//     maxPending candidates exist, the oldest evicted first, and at most
//     maxFlows routes and candidates together. Confirmed routes are never
//     evicted: when only they fill the table, a new candidate is rejected.
//
// Losing the table (a relay restart) also loses which routes were active:
// until a caller's next answered check, another session can claim its
// address. Persisted routes (#42) close that gap; see
// docs/adr/0001-route-stickiness.md.
type flowTable struct {
	stickinessWindow time.Duration
	idleTimeout      time.Duration
	pendingTimeout   time.Duration
	maxFlows         int
	maxPending       int

	mu       sync.Mutex
	callers  map[netip.AddrPort]*callerFlows
	sessions map[string]map[netip.AddrPort]*callerFlows // confirmed and pending, bounded by callers
	routes   *list.List                                 // of *callerFlows with a route, most recently active first
	pending  *list.List                                 // of *callerFlows with a candidate, newest first
	evicted  uint64
	rejected uint64
	promoted uint64
}

// callerFlows is one caller's confirmed route and pending candidate.
type callerFlows struct {
	caller netip.AddrPort

	route             *flow
	routeElem         *list.Element
	lastSeen          time.Time     // last caller packet on the route
	lastAuthenticated time.Time     // arrival of the route session's last answered request
	routeChecks       bindingChecks // the route session's unanswered requests

	candidate       *flow
	candidateElem   *list.Element
	admitted        time.Time
	candidateChecks bindingChecks
}

// flow is where a caller's packets go: a worker, for a session.
type flow struct {
	worker  netip.AddrPort
	session string
}

type flowLimits struct {
	stickinessWindow time.Duration
	idleTimeout      time.Duration
	pendingTimeout   time.Duration
	maxFlows         int
	maxPending       int
}

func newFlowTable(limits flowLimits) *flowTable {
	if limits.stickinessWindow <= 0 {
		limits.stickinessWindow = DefaultRouteStickinessWindow
	}
	return &flowTable{
		stickinessWindow: limits.stickinessWindow,
		idleTimeout:      limits.idleTimeout,
		pendingTimeout:   limits.pendingTimeout,
		maxFlows:         limits.maxFlows,
		maxPending:       limits.maxPending,
		callers:          make(map[netip.AddrPort]*callerFlows),
		sessions:         make(map[string]map[netip.AddrPort]*callerFlows),
		routes:           list.New(),
		pending:          list.New(),
	}
}

// routeSTUN returns the worker for a caller's binding request without the
// store: the candidate's worker when the request is for the candidate's
// session, or else the route's worker when it is for the route's session.
// Either records the request, so its worker's answer can be matched.
func (t *flowTable) routeSTUN(caller netip.AddrPort, session string, txID [stun.TransactionIDSize]byte, now time.Time) (netip.AddrPort, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.live(caller, now)
	switch {
	case c == nil:
		return netip.AddrPort{}, false
	case c.candidate != nil && c.candidate.session == session:
		c.candidateChecks.record(txID, now)

		return c.candidate.worker, true
	case c.route != nil && c.route.session == session:
		c.routeChecks.record(txID, now)
		t.touch(c, now)

		return c.route.worker, true
	default:
		return netip.AddrPort{}, false
	}
}

// route returns the worker for any other caller packet: only a confirmed
// route carries those.
func (t *flowTable) route(caller netip.AddrPort, now time.Time) (netip.AddrPort, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.live(caller, now)
	if c == nil || c.route == nil {
		return netip.AddrPort{}, false
	}
	t.touch(c, now)

	return c.route.worker, true
}

// admit places a binding request the store routed to worker. A request
// that matches the caller's confirmed route needs nothing; otherwise it
// becomes, or joins, the caller's candidate, and the confirmed route stays
// as it is. It reports false when the route is active for another session
// or the table has no room for a new candidate; the request is then
// dropped.
func (t *flowTable) admit(caller, worker netip.AddrPort, session string, txID [stun.TransactionIDSize]byte, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.expire(now)
	c := t.callers[caller]
	if c != nil && c.route != nil && c.route.worker == worker && c.route.session == session {
		c.routeChecks.record(txID, now)
		return true
	}
	c, ok := t.propose(caller, flow{worker: worker, session: session}, now)
	if ok {
		c.candidateChecks.record(txID, now)
	}

	return ok
}

// reroute follows a change of a session's owner: when the caller's route
// or candidate for the session is at another worker, the owner becomes the
// caller's candidate. The confirmed route stays until the owner answers.
func (t *flowTable) reroute(caller, owner netip.AddrPort, session string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.live(caller, now)
	if c == nil {
		return false
	}
	if c.candidate != nil && c.candidate.session == session && c.candidate.worker != owner {
		t.dropCandidate(c)
	}
	if c.route == nil || c.route.session != session || c.route.worker == owner {
		return false
	}
	_, ok := t.propose(caller, flow{worker: owner, session: session}, now)

	return ok
}

// moveSession re-points existing authenticated routes without admitting or
// evicting any route. Pending candidates and unanswered route requests for
// the moved session are discarded: their old owner's late answer must not
// undo the trusted move. A moved route keeps its last answered request
// time, so it stays exactly as active as it was.
func (t *flowTable) moveSession(session string, from, to netip.AddrPort) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.sessions[session] {
		if c.candidate != nil && c.candidate.session == session {
			t.dropCandidate(c)
		}
		if c.route != nil && c.route.session == session && (!from.IsValid() || c.route.worker == from) {
			c.route.worker = to
			c.routeChecks = bindingChecks{}
		}
	}
}

// answer reports whether worker may send pkt to caller, and promotes the
// caller's candidate when pkt is that worker's binding success for one of
// the candidate's requests; promoted is then the candidate's session. If
// the route has become active for another session meanwhile, the candidate
// is dropped with the packet instead. A worker may otherwise send only to
// callers it holds the confirmed route of, and its binding success for one
// of the route's requests renews the route.
func (t *flowTable) answer(caller, worker netip.AddrPort, pkt []byte, now time.Time) (allowed bool, promoted string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.live(caller, now)
	if c == nil {
		return false, ""
	}
	txID, success := parseBindingSuccess(pkt)
	if c.candidate != nil && c.candidate.worker == worker && success {
		if sentAt, ok := c.candidateChecks.answer(txID); ok {
			// The route's session may have answered a check since this
			// candidate was admitted.
			if t.sticky(c, c.candidate.session, now) {
				t.dropCandidate(c)
				return false, ""
			}
			if c.route != nil && c.route.session == c.candidate.session {
				sentAt = maxTime(c.lastAuthenticated, sentAt)
			}
			t.promote(c, now)
			c.lastAuthenticated = sentAt
			return true, c.route.session
		}
	}
	if c.route != nil && c.route.worker == worker {
		if success {
			if sentAt, ok := c.routeChecks.answer(txID); ok {
				c.lastAuthenticated = maxTime(c.lastAuthenticated, sentAt)
			}
		}
		return true, ""
	}

	return false, ""
}

// forget drops a caller's route and candidate for a session that no longer
// has an owner.
func (t *flowTable) forget(caller netip.AddrPort, session string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	c, ok := t.callers[caller]
	if !ok {
		return
	}
	if c.candidate != nil && c.candidate.session == session {
		t.dropCandidate(c)
	}
	if c.route != nil && c.route.session == session {
		t.dropRoute(c)
	}
}

// sweep drops expired routes and candidates.
func (t *flowTable) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.expire(now)
}

type flowCounts struct {
	routes, pending             int
	evicted, rejected, promoted uint64
}

func (t *flowTable) counts() flowCounts {
	t.mu.Lock()
	defer t.mu.Unlock()

	return flowCounts{
		routes: t.routes.Len(), pending: t.pending.Len(),
		evicted: t.evicted, rejected: t.rejected, promoted: t.promoted,
	}
}

// forwardRoute returns the current authenticated worker and session for a
// caller without a store lookup. It is rechecked at the send boundary.
func (t *flowTable) forwardRoute(caller netip.AddrPort) (flow, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.live(caller, time.Now())
	if c == nil || c.route == nil {
		return flow{}, false
	}
	return *c.route, true
}

// sessionWorker supplies the current route on automatic hold release.
func (t *flowTable) sessionWorker(session string, fallback netip.AddrPort) netip.AddrPort {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.sessions[session] {
		if c.route != nil && c.route.session == session {
			return c.route.worker
		}
	}
	return fallback
}

// The helpers below run under mu.

func (t *flowTable) index(c *callerFlows, session string) {
	if t.sessions[session] == nil {
		t.sessions[session] = make(map[netip.AddrPort]*callerFlows)
	}
	t.sessions[session][c.caller] = c
}

func (t *flowTable) unindex(c *callerFlows, session string) {
	if c.route != nil && c.route.session == session || c.candidate != nil && c.candidate.session == session {
		return
	}
	delete(t.sessions[session], c.caller)
	if len(t.sessions[session]) == 0 {
		delete(t.sessions, session)
	}
}

// propose makes f the caller's candidate, replacing any candidate it has,
// unless the route is active for another session or the table has no room
// for one.
func (t *flowTable) propose(caller netip.AddrPort, f flow, now time.Time) (*callerFlows, bool) {
	c := t.callers[caller]
	if t.sticky(c, f.session, now) {
		t.rejected++
		return nil, false
	}
	if c != nil && c.candidate != nil {
		if *c.candidate == f {
			return c, true
		}
		t.dropCandidate(c) // frees a slot for the new candidate
		c = t.callers[caller]
	}

	for t.pending.Len() >= t.maxPending || t.routes.Len()+t.pending.Len() >= t.maxFlows {
		oldest := t.pending.Back()
		if oldest == nil {
			t.rejected++

			return nil, false
		}
		victim, _ := oldest.Value.(*callerFlows)
		t.dropCandidate(victim)
		t.evicted++
		c = t.callers[caller]
	}

	if c == nil {
		c = &callerFlows{caller: caller}
		t.callers[caller] = c
	}
	c.candidate = &f
	t.index(c, f.session)
	c.admitted = now
	c.candidateChecks = bindingChecks{}
	c.candidateElem = t.pending.PushFront(c)

	return c, true
}

// promote makes a caller's candidate its confirmed route.
func (t *flowTable) promote(c *callerFlows, now time.Time) {
	f := *c.candidate
	previous := ""
	if c.route != nil {
		previous = c.route.session
	}
	t.pending.Remove(c.candidateElem)
	c.candidate, c.candidateElem = nil, nil
	if c.route == nil {
		c.routeElem = t.routes.PushFront(c)
	} else {
		t.routes.MoveToFront(c.routeElem)
	}
	c.route = &f
	c.routeChecks = c.candidateChecks
	c.candidateChecks = bindingChecks{}
	if previous != "" && previous != f.session {
		t.unindex(c, previous)
	}
	c.lastSeen = now
	t.promoted++
}

// live returns a caller's flows after dropping whatever has expired.
func (t *flowTable) live(caller netip.AddrPort, now time.Time) *callerFlows {
	c, ok := t.callers[caller]
	if !ok {
		return nil
	}
	if c.candidate != nil && now.Sub(c.admitted) > t.pendingTimeout {
		t.dropCandidate(c)
	}
	if c.route != nil && now.Sub(c.lastSeen) > t.idleTimeout && !t.protected(c, now) {
		t.dropRoute(c)
	}
	if c.route == nil && c.candidate == nil {
		return nil // released
	}

	return c
}

// touch records caller activity on the confirmed route.
func (t *flowTable) touch(c *callerFlows, now time.Time) {
	if c.route != nil {
		c.lastSeen = now
		t.routes.MoveToFront(c.routeElem)
	}
}

// expire drops expired candidates and routes from the old end of each list.
func (t *flowTable) expire(now time.Time) {
	for e := t.pending.Back(); e != nil; e = t.pending.Back() {
		c, _ := e.Value.(*callerFlows)
		if now.Sub(c.admitted) <= t.pendingTimeout {
			break
		}
		t.dropCandidate(c)
	}
	for e := t.routes.Back(); e != nil; {
		c, _ := e.Value.(*callerFlows)
		if now.Sub(c.lastSeen) <= t.idleTimeout {
			break
		}
		previous := e.Prev()
		if !t.protected(c, now) { // an active route stays until its window lapses
			t.dropRoute(c)
		}
		e = previous
	}
}

func (t *flowTable) dropCandidate(c *callerFlows) {
	session := c.candidate.session
	t.pending.Remove(c.candidateElem)
	c.candidate, c.candidateElem = nil, nil
	t.unindex(c, session)
	t.release(c)
}

func (t *flowTable) dropRoute(c *callerFlows) {
	session := c.route.session
	t.routes.Remove(c.routeElem)
	c.route, c.routeElem = nil, nil
	c.routeChecks = bindingChecks{}
	c.lastAuthenticated = time.Time{}
	t.unindex(c, session)
	t.release(c)
}

// release forgets a caller with neither a route nor a candidate.
func (t *flowTable) release(c *callerFlows) {
	if c.route == nil && c.candidate == nil {
		delete(t.callers, c.caller)
	}
}

// sticky reports whether the caller's route is active for a session other
// than session, which then may not take the caller's address.
func (t *flowTable) sticky(c *callerFlows, session string, now time.Time) bool {
	return c != nil && c.route != nil && c.route.session != session && t.protected(c, now)
}

// protected reports whether the route is active: its worker answered one of
// the route session's requests that arrived within the stickiness window.
func (t *flowTable) protected(c *callerFlows, now time.Time) bool {
	return !c.lastAuthenticated.IsZero() && now.Sub(c.lastAuthenticated) < t.stickinessWindow
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// bindingChecks remembers a few unanswered binding requests and when each
// arrived, overwriting the oldest when full. A binding success consumes its
// request, so a replayed or unmatched success counts for nothing. The
// request's arrival, not the answer's, is when the session was last active.
type bindingChecks struct {
	checks [maxOutstandingChecks]struct {
		id     [stun.TransactionIDSize]byte
		sentAt time.Time
	}
	next int
}

// record remembers a request; a retransmission keeps its first arrival.
func (b *bindingChecks) record(id [stun.TransactionIDSize]byte, now time.Time) {
	for _, check := range b.checks {
		if check.id == id && !check.sentAt.IsZero() {
			return
		}
	}
	b.checks[b.next].id = id
	b.checks[b.next].sentAt = now
	b.next = (b.next + 1) % len(b.checks)
}

// answer consumes the request a binding success answers and returns when
// it arrived.
func (b *bindingChecks) answer(id [stun.TransactionIDSize]byte) (time.Time, bool) {
	for i, check := range b.checks {
		if check.id == id && !check.sentAt.IsZero() {
			b.checks[i].sentAt = time.Time{}
			return check.sentAt, true
		}
	}
	return time.Time{}, false
}

// parseBindingSuccess returns the transaction ID of a STUN binding success
// response, read from its header only.
func parseBindingSuccess(pkt []byte) ([stun.TransactionIDSize]byte, bool) {
	var txID [stun.TransactionIDSize]byte
	if !isSTUN(pkt) || binary.BigEndian.Uint16(pkt[0:2]) != stun.BindingSuccess.Value() {
		return txID, false
	}
	copy(txID[:], pkt[8:20])

	return txID, true
}

// forgetSession removes every confirmed or pending route for a lost call.
func (t *flowTable) forgetSession(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.sessions[id] {
		if c.candidate != nil && c.candidate.session == id {
			t.dropCandidate(c)
		}
		if c.route != nil && c.route.session == id {
			t.dropRoute(c)
		}
	}
}
