package relay

import (
	"container/list"
	"net/netip"
	"sync"
	"time"
)

// flowTable maps each caller address to the worker its packets go to. It is
// a cache of the session-owner store, and it is bounded:
//
//   - A binding request routed by the store admits a pending flow. A pending
//     flow carries only the caller's binding requests, and it expires after
//     the pending timeout.
//   - The flow is confirmed when its worker sends the caller a packet, which
//     the worker does only after checking the request's ICE credentials (its
//     binding success response). A confirmed flow carries all of the caller's
//     packets and expires when the caller has been idle for the idle timeout.
//   - At most maxPending flows are pending, and at most maxFlows exist in
//     all. A new flow beyond either limit evicts the oldest pending flow, or,
//     with none pending, the confirmed flow idle the longest.
//
// Losing the table (a relay restart) only costs the caller's packets until
// its next binding request is answered.
type flowTable struct {
	idleTimeout    time.Duration
	pendingTimeout time.Duration
	maxFlows       int
	maxPending     int

	mu        sync.Mutex
	flows     map[netip.AddrPort]*flow
	pending   *list.List // of *flow, newest first
	confirmed *list.List // of *flow, most recently active first
	evicted   uint64
}

type flow struct {
	caller    netip.AddrPort
	worker    netip.AddrPort
	session   string // the session the binding request named
	confirmed bool
	admitted  time.Time // when the flow became pending
	lastSeen  time.Time // last caller packet on a confirmed flow
	elem      *list.Element
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
		flows:          make(map[netip.AddrPort]*flow),
		pending:        list.New(),
		confirmed:      list.New(),
	}
}

// admit points a caller's flow at a session's owner after the store named
// it. A new flow, or one that moves to another worker or session, is
// pending until that worker answers; a flow already at that worker is kept
// as it is.
func (t *flowTable) admit(caller, worker netip.AddrPort, session string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.expire(now)
	if f, ok := t.flows[caller]; ok {
		if f.worker == worker && f.session == session {
			return
		}
		t.remove(f)
	}
	for t.pending.Len() >= t.maxPending || len(t.flows) >= t.maxFlows {
		if !t.evictOne() {
			break
		}
	}

	f := &flow{caller: caller, worker: worker, session: session, admitted: now}
	f.elem = t.pending.PushFront(f)
	t.flows[caller] = f
}

// routeSTUN returns the worker for a binding request when the caller's flow,
// pending or confirmed, is for the session the request names.
func (t *flowTable) routeSTUN(caller netip.AddrPort, session string, now time.Time) (netip.AddrPort, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	f := t.live(caller, now)
	if f == nil || f.session != session {
		return netip.AddrPort{}, false
	}
	t.touch(f, now)

	return f.worker, true
}

// route returns the worker for any other caller packet: only a confirmed
// flow carries those.
func (t *flowTable) route(caller netip.AddrPort, now time.Time) (netip.AddrPort, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	f := t.live(caller, now)
	if f == nil || !f.confirmed {
		return netip.AddrPort{}, false
	}
	t.touch(f, now)

	return f.worker, true
}

// answer reports whether worker may send a packet to caller: the caller's
// flow must belong to that worker. The first such packet confirms a pending
// flow, and answer then also returns the flow's session.
func (t *flowTable) answer(caller, worker netip.AddrPort, now time.Time) (allowed bool, confirmed string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	f := t.live(caller, now)
	if f == nil || f.worker != worker {
		return false, ""
	}
	if f.confirmed {
		return true, ""
	}
	t.pending.Remove(f.elem)
	f.confirmed = true
	f.lastSeen = now
	f.elem = t.confirmed.PushFront(f)

	return true, f.session
}

// reroute moves a caller's flow for a session to the session's current
// owner, as pending, when the store names a different one.
func (t *flowTable) reroute(caller, owner netip.AddrPort, session string, now time.Time) bool {
	t.mu.Lock()
	f := t.live(caller, now)
	moved := f != nil && f.session == session && f.worker != owner
	t.mu.Unlock()
	if moved {
		t.admit(caller, owner, session, now)
	}

	return moved
}

// forget drops a caller's flow for a session that no longer has an owner.
func (t *flowTable) forget(caller netip.AddrPort, session string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if f, ok := t.flows[caller]; ok && f.session == session {
		t.remove(f)
	}
}

// sweep drops expired flows.
func (t *flowTable) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.expire(now)
}

// counts returns the number of flows, how many of them are pending, and how
// many flows the limits have evicted.
func (t *flowTable) counts() (flows, pending int, evicted uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.flows), t.pending.Len(), t.evicted
}

// The helpers below run under mu.

// live returns a caller's flow unless it has expired, dropping it if so.
func (t *flowTable) live(caller netip.AddrPort, now time.Time) *flow {
	f, ok := t.flows[caller]
	if !ok {
		return nil
	}
	if t.expired(f, now) {
		t.remove(f)

		return nil
	}

	return f
}

func (t *flowTable) expired(f *flow, now time.Time) bool {
	if f.confirmed {
		return now.Sub(f.lastSeen) > t.idleTimeout
	}

	return now.Sub(f.admitted) > t.pendingTimeout
}

// touch records caller activity on a confirmed flow. Pending flows are not
// kept alive by the caller: only the worker's answer does that.
func (t *flowTable) touch(f *flow, now time.Time) {
	if f.confirmed {
		f.lastSeen = now
		t.confirmed.MoveToFront(f.elem)
	}
}

// expire drops expired flows from the old end of each list.
func (t *flowTable) expire(now time.Time) {
	for _, l := range []*list.List{t.pending, t.confirmed} {
		for e := l.Back(); e != nil; e = l.Back() {
			f, _ := e.Value.(*flow)
			if !t.expired(f, now) {
				break
			}
			t.remove(f)
		}
	}
}

// evictOne drops the oldest pending flow or, with none pending, the
// confirmed flow idle the longest.
func (t *flowTable) evictOne() bool {
	e := t.pending.Back()
	if e == nil {
		e = t.confirmed.Back()
	}
	if e == nil {
		return false
	}
	f, _ := e.Value.(*flow)
	t.remove(f)
	t.evicted++

	return true
}

func (t *flowTable) remove(f *flow) {
	if f.confirmed {
		t.confirmed.Remove(f.elem)
	} else {
		t.pending.Remove(f.elem)
	}
	delete(t.flows, f.caller)
}
