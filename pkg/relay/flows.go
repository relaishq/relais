package relay

import (
	"net/netip"
	"sync"
	"time"
)

// flowTable maps each caller address to the worker its packets go to. It is
// a cache of the session-owner store: a STUN binding request binds a flow,
// every packet from the caller keeps it alive, and a flow idle for the
// timeout is dropped. Losing the table (a relay restart) only costs the
// caller's packets until its next binding request.
type flowTable struct {
	timeout time.Duration

	mu    sync.Mutex
	flows map[netip.AddrPort]*flow
}

type flow struct {
	worker   netip.AddrPort
	session  string // the session the binding request named
	lastSeen time.Time
}

func newFlowTable(timeout time.Duration) *flowTable {
	return &flowTable{timeout: timeout, flows: make(map[netip.AddrPort]*flow)}
}

// bind points a caller's flow at a session's worker. It reports whether the
// flow is new or changed, so the change can be logged once.
func (t *flowTable) bind(caller, worker netip.AddrPort, session string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if f, ok := t.flows[caller]; ok {
		changed := f.worker != worker || f.session != session
		f.worker, f.session, f.lastSeen = worker, session, now

		return changed
	}
	t.flows[caller] = &flow{worker: worker, session: session, lastSeen: now}

	return true
}

// lookup returns the worker for a caller's packet and keeps the flow alive.
func (t *flowTable) lookup(caller netip.AddrPort, now time.Time) (netip.AddrPort, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	f, ok := t.flows[caller]
	if !ok {
		return netip.AddrPort{}, false
	}
	if now.Sub(f.lastSeen) > t.timeout {
		delete(t.flows, caller)

		return netip.AddrPort{}, false
	}
	f.lastSeen = now

	return f.worker, true
}

// sweep drops idle flows.
func (t *flowTable) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for caller, f := range t.flows {
		if now.Sub(f.lastSeen) > t.timeout {
			delete(t.flows, caller)
		}
	}
}

func (t *flowTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return len(t.flows)
}
