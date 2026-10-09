package relay

import (
	"context"
	"net/netip"
	"sync"
)

// maxWaitersPerSession bounds the binding requests (and flow refreshes) that
// wait for one session's owner lookup. Callers retransmit, so dropping more
// costs nothing.
const maxWaitersPerSession = 8

// ownerLookups looks session owners up in the session-owner store off the
// caller loop, so a slow store never delays packets on established flows.
// Lookups run on a fixed number of goroutines, at most one per session at a
// time: binding requests for a session whose lookup is already queued or
// running wait for its result. At most maxQueued sessions wait at once;
// binding requests beyond either bound are dropped, and the caller's ICE
// agent retransmits them.
type ownerLookups struct {
	relay     *Relay
	queue     chan string
	maxQueued int

	mu      sync.Mutex
	pending map[string][]waiter // by session, while its lookup is queued or running
}

// waiter is one caller waiting for a session's owner.
type waiter struct {
	caller netip.AddrPort
	// datagram is a binding request to forward once the owner is known, with
	// MaxHeaderLen bytes of room for the relay-leg header in front. It is nil
	// for a refresh of a flow whose request was already forwarded.
	datagram []byte
}

func newOwnerLookups(r *Relay, workers, maxQueued int) *ownerLookups {
	l := &ownerLookups{
		relay:     r,
		queue:     make(chan string, maxQueued),
		maxQueued: maxQueued,
		pending:   make(map[string][]waiter),
	}
	r.running.Add(workers)
	for range workers {
		go l.run(r.ctx)
	}

	return l
}

// resolve queues a binding request until its session's owner is known. It
// reports false when the request is dropped.
func (l *ownerLookups) resolve(sessionID string, caller netip.AddrPort, pkt []byte) bool {
	datagram := make([]byte, MaxHeaderLen+len(pkt))
	copy(datagram[MaxHeaderLen:], pkt)

	return l.add(sessionID, waiter{caller: caller, datagram: datagram})
}

// refresh checks the owner of a session whose binding request was already
// forwarded on the caller's flow, so the flow follows a change of owner. It
// is best effort.
func (l *ownerLookups) refresh(sessionID string, caller netip.AddrPort) {
	l.add(sessionID, waiter{caller: caller})
}

func (l *ownerLookups) add(sessionID string, w waiter) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if waiters, ok := l.pending[sessionID]; ok {
		if w.datagram == nil {
			for _, other := range waiters {
				if other.datagram == nil && other.caller == w.caller {
					return true // this flow is already being refreshed
				}
			}
		}
		if len(waiters) >= maxWaitersPerSession {
			return false
		}
		l.pending[sessionID] = append(waiters, w)

		return true
	}

	if len(l.pending) >= l.maxQueued {
		return false
	}
	l.pending[sessionID] = []waiter{w}
	l.queue <- sessionID // never blocks: the queue holds at most len(pending) sessions

	return true
}

func (l *ownerLookups) run(ctx context.Context) {
	defer l.relay.running.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case sessionID := <-l.queue:
			owner, err := l.relay.owner(sessionID)

			l.mu.Lock()
			waiters := l.pending[sessionID]
			delete(l.pending, sessionID)
			l.mu.Unlock()

			if ctx.Err() != nil {
				return
			}
			l.relay.resolved(sessionID, owner, err, waiters)
		}
	}
}
