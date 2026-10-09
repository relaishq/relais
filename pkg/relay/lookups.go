package relay

import (
	"context"
	"net/netip"
	"sync"

	"github.com/pion/stun/v4"
)

const (
	// maxWaitersPerSession bounds the binding requests (and flow refreshes)
	// that wait for one session's owner lookup. Callers retransmit, so
	// dropping more costs nothing.
	maxWaitersPerSession = 8

	// maxQueuedBindingRequest is the largest binding request kept while its
	// session's owner is looked up. ICE checks are a few hundred bytes; a
	// larger one is dropped rather than held.
	maxQueuedBindingRequest = 1500
)

// ownerLookups looks session owners up in the session-owner store off the
// caller loop, so a slow store never delays packets on established flows.
//
// Lookups run on a fixed number of goroutines, and at most one per session
// at a time: from the moment a session's lookup is queued until its result
// has been applied, binding requests for the session wait for it, and any
// that arrive after the result was read get a fresh lookup once it has been
// applied. Results for one session are therefore applied in order, and an
// older result never overwrites a newer one. A trusted move invalidates a
// pending lookup by generation; its waiters retry against the new owner.
//
// What waits is bounded: at most maxQueued sessions, maxWaitersPerSession
// requests per session, maxBytes of copied requests in all, and no request
// over maxQueuedBindingRequest. Requests beyond those bounds are dropped,
// and the caller's ICE agent retransmits them.
type ownerLookups struct {
	relay       *Relay
	queue       chan string
	maxQueued   int
	maxBytes    int
	beforeApply func(sessionID string) // test hook; nil outside tests

	mu      sync.Mutex
	pending map[string]*sessionLookup // by session, from queueing until applied
	bytes   int                       // copied requests held by waiters
}

// sessionLookup is one session's queued or running lookup.
type sessionLookup struct {
	waiters    []waiter
	generation uint64
}

// waiter is one caller waiting for a session's owner.
type waiter struct {
	caller netip.AddrPort
	// datagram is a binding request to forward once the owner is known, with
	// MaxHeaderLen bytes of room for the relay-leg header in front, and txID
	// its transaction ID. datagram is nil for a refresh of a flow whose
	// request was already forwarded.
	datagram []byte
	txID     [stun.TransactionIDSize]byte
}

func newOwnerLookups(r *Relay, cfg Config) *ownerLookups {
	l := &ownerLookups{
		relay:       r,
		queue:       make(chan string, cfg.MaxQueuedLookups),
		maxQueued:   cfg.MaxQueuedLookups,
		maxBytes:    cfg.MaxQueuedLookupBytes,
		beforeApply: cfg.beforeApply,
		pending:     make(map[string]*sessionLookup),
	}
	r.running.Add(cfg.OwnerLookups)
	for range cfg.OwnerLookups {
		go l.run(r.ctx)
	}

	return l
}

// resolve queues a binding request until its session's owner is known. It
// reports false when the request is dropped.
func (l *ownerLookups) resolve(sessionID string, caller netip.AddrPort, pkt []byte, txID [stun.TransactionIDSize]byte) bool {
	if len(pkt) > maxQueuedBindingRequest {
		return false
	}
	datagram := make([]byte, MaxHeaderLen+len(pkt))
	copy(datagram[MaxHeaderLen:], pkt)

	return l.add(sessionID, waiter{caller: caller, datagram: datagram, txID: txID})
}

// refresh checks the owner of a session whose binding request was already
// forwarded on the caller's flow, so the flow can follow a change of owner.
// It is best effort.
func (l *ownerLookups) refresh(sessionID string, caller netip.AddrPort) {
	l.add(sessionID, waiter{caller: caller})
}

func (l *ownerLookups) add(sessionID string, w waiter) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if w.datagram != nil && l.bytes+len(w.datagram) > l.maxBytes {
		return false
	}

	lookup, ok := l.pending[sessionID]
	if !ok {
		if len(l.pending) >= l.maxQueued {
			return false
		}
		lookup = &sessionLookup{}
		l.pending[sessionID] = lookup
		l.queue <- sessionID // never blocks: the queue holds at most len(pending) sessions
	}

	if w.datagram == nil {
		for _, other := range lookup.waiters {
			if other.datagram == nil && other.caller == w.caller {
				return true // this flow is already being refreshed
			}
		}
	}
	if len(lookup.waiters) >= maxWaitersPerSession {
		return false
	}
	lookup.waiters = append(lookup.waiters, w)
	l.bytes += len(w.datagram)

	return true
}

func (l *ownerLookups) run(ctx context.Context) {
	defer l.relay.running.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case sessionID := <-l.queue:
			l.lookUp(ctx, sessionID)
		}
	}
}

// lookUp reads a session's owner and applies it to the requests that were
// waiting when it was read. The session stays pending until then, so no
// other lookup for it runs or applies meanwhile; requests that arrived
// later are queued for a fresh lookup.
func (l *ownerLookups) lookUp(ctx context.Context, sessionID string) {
	l.mu.Lock()
	lookup := l.pending[sessionID]
	generation := lookup.generation
	l.mu.Unlock()
	owner, err := l.relay.owner(sessionID)
	l.mu.Lock()
	nWaiters := len(lookup.waiters)
	l.mu.Unlock()

	if l.beforeApply != nil {
		l.beforeApply(sessionID)
	}
	if ctx.Err() != nil {
		return
	}

	l.relay.routeMu.Lock()
	l.mu.Lock()
	if lookup.generation != generation {
		// Keep the bounded waiters, but discard the stale result. A fresh lookup
		// follows the transferred lease; no history map grows with moved calls.
		l.queue <- sessionID
		l.mu.Unlock()
		l.relay.routeMu.Unlock()
		return
	}
	waiters := lookup.waiters[:nWaiters]
	lookup.waiters = lookup.waiters[nWaiters:]
	for _, w := range waiters {
		l.bytes -= len(w.datagram)
	}
	l.mu.Unlock()
	forwards := l.relay.resolved(sessionID, owner, err, waiters)
	l.relay.routeMu.Unlock()
	for _, w := range forwards {
		l.relay.forward(w.datagram, w.caller, owner)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(lookup.waiters) > 0 {
		l.queue <- sessionID
	} else {
		delete(l.pending, sessionID)
	}
}
