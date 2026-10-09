package relay

import (
	"container/list"
	"encoding/binary"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/stun/v4"
)

// maxCandidateTransactions is how many binding requests a pending candidate
// remembers; a binding success for any of them confirms it.
const maxCandidateTransactions = 8

// flowTable maps each caller address to the worker its packets go to. It is
// a cache of the session-owner store, and no routing changes for a caller
// until the owning worker has authenticated one of the caller's binding
// requests:
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
//     promotes the candidate to the caller's confirmed route. Nothing else
//     does: the relay reads only STUN headers on the worker leg.
//   - Candidates expire after the pending timeout, routes after the idle
//     timeout without caller packets. At most maxPending candidates exist,
//     the oldest evicted first, and at most maxFlows routes and candidates
//     together. Confirmed routes are never evicted: when only they fill the
//     table, a new candidate is rejected.
//
// Losing the table (a relay restart) only costs the caller's packets until
// its next binding request is answered.
type flowTable struct {
	idleTimeout    time.Duration
	pendingTimeout time.Duration
	maxFlows       int
	maxPending     int

	mu       sync.Mutex
	callers  map[netip.AddrPort]*callerFlows
	routes   *list.List // of *callerFlows with a route, most recently active first
	pending  *list.List // of *callerFlows with a candidate, newest first
	evicted  uint64
	rejected uint64
	promoted uint64
}

// callerFlows is one caller's confirmed route and pending candidate.
type callerFlows struct {
	caller netip.AddrPort

	route     *flow
	routeElem *list.Element
	lastSeen  time.Time // last caller packet on the route

	candidate     *flow
	candidateElem *list.Element
	admitted      time.Time
	txIDs         [maxCandidateTransactions][stun.TransactionIDSize]byte
	nTxIDs        int // binding requests recorded, including overwritten ones
}

// flow is where a caller's packets go: a worker, for a session.
type flow struct {
	worker  netip.AddrPort
	session string
}

type flowLimits struct {
	idleTimeout    time.Duration
	pendingTimeout time.Duration
	maxFlows       int
	maxPending     int
}

func newFlowTable(limits flowLimits) *flowTable {
	return &flowTable{
		idleTimeout:    limits.idleTimeout,
		pendingTimeout: limits.pendingTimeout,
		maxFlows:       limits.maxFlows,
		maxPending:     limits.maxPending,
		callers:        make(map[netip.AddrPort]*callerFlows),
		routes:         list.New(),
		pending:        list.New(),
	}
}

// routeSTUN returns the worker for a caller's binding request without the
// store: the candidate's worker when the request is for the candidate's
// session (which records the request), or else the route's worker when it
// is for the route's session.
func (t *flowTable) routeSTUN(caller netip.AddrPort, session string, txID [stun.TransactionIDSize]byte, now time.Time) (netip.AddrPort, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.live(caller, now)
	switch {
	case c == nil:
		return netip.AddrPort{}, false
	case c.candidate != nil && c.candidate.session == session:
		c.record(txID)

		return c.candidate.worker, true
	case c.route != nil && c.route.session == session:
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
// as it is. It reports false when the table has no room for a new
// candidate; the request is then dropped.
func (t *flowTable) admit(caller, worker netip.AddrPort, session string, txID [stun.TransactionIDSize]byte, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.expire(now)
	c := t.callers[caller]
	if c != nil && c.route != nil && c.route.worker == worker && c.route.session == session {
		return true
	}
	c, ok := t.propose(caller, flow{worker: worker, session: session}, now)
	if ok {
		c.record(txID)
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

// answer reports whether worker may send pkt to caller, and promotes the
// caller's candidate when pkt is that worker's binding success for one of
// the candidate's requests; promoted is then the candidate's session. A
// worker may otherwise send only to callers it holds the confirmed route of.
func (t *flowTable) answer(caller, worker netip.AddrPort, pkt []byte, now time.Time) (allowed bool, promoted string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	c := t.live(caller, now)
	if c == nil {
		return false, ""
	}
	if c.candidate != nil && c.candidate.worker == worker {
		if txID, ok := parseBindingSuccess(pkt); ok && c.answers(txID) {
			t.promote(c, now)

			return true, c.route.session
		}
	}
	if c.route != nil && c.route.worker == worker {
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

// The helpers below run under mu.

// propose makes f the caller's candidate, replacing any candidate it has,
// unless the table has no room for one.
func (t *flowTable) propose(caller netip.AddrPort, f flow, now time.Time) (*callerFlows, bool) {
	c := t.callers[caller]
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
	c.admitted = now
	c.nTxIDs = 0
	c.candidateElem = t.pending.PushFront(c)

	return c, true
}

// promote makes a caller's candidate its confirmed route.
func (t *flowTable) promote(c *callerFlows, now time.Time) {
	f := *c.candidate
	t.pending.Remove(c.candidateElem)
	c.candidate, c.candidateElem = nil, nil
	if c.route == nil {
		c.routeElem = t.routes.PushFront(c)
	} else {
		t.routes.MoveToFront(c.routeElem)
	}
	c.route = &f
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
	if c.route != nil && now.Sub(c.lastSeen) > t.idleTimeout {
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
	for e := t.routes.Back(); e != nil; e = t.routes.Back() {
		c, _ := e.Value.(*callerFlows)
		if now.Sub(c.lastSeen) <= t.idleTimeout {
			break
		}
		t.dropRoute(c)
	}
}

func (t *flowTable) dropCandidate(c *callerFlows) {
	t.pending.Remove(c.candidateElem)
	c.candidate, c.candidateElem = nil, nil
	t.release(c)
}

func (t *flowTable) dropRoute(c *callerFlows) {
	t.routes.Remove(c.routeElem)
	c.route, c.routeElem = nil, nil
	t.release(c)
}

// release forgets a caller with neither a route nor a candidate.
func (t *flowTable) release(c *callerFlows) {
	if c.route == nil && c.candidate == nil {
		delete(t.callers, c.caller)
	}
}

// record remembers a binding request sent on the candidate.
func (c *callerFlows) record(txID [stun.TransactionIDSize]byte) {
	for i := range min(c.nTxIDs, maxCandidateTransactions) {
		if c.txIDs[i] == txID {
			return // a retransmission
		}
	}
	c.txIDs[c.nTxIDs%maxCandidateTransactions] = txID
	c.nTxIDs++
}

// answers reports whether txID is one of the candidate's requests.
func (c *callerFlows) answers(txID [stun.TransactionIDSize]byte) bool {
	for i := range min(c.nTxIDs, maxCandidateTransactions) {
		if c.txIDs[i] == txID {
			return true
		}
	}

	return false
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
