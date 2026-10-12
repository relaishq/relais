// Package relaylease owns same-host relay acquisition and process fencing.
package relaylease

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relais/internal/processidentity"
	"github.com/relais/pkg/sessionstore"
)

const (
	DefaultTTL   = 600 * time.Millisecond
	DefaultRenew = 200 * time.Millisecond
	DefaultPoll  = 100 * time.Millisecond
	RetryRenew   = 50 * time.Millisecond
)

var configurationErrors atomic.Uint64

// ConfigurationErrorCount includes failures before any sockets are bound.
func ConfigurationErrorCount() uint64 { return configurationErrors.Load() }
func configurationError(err error) error {
	count := configurationErrors.Add(1)
	log.Printf("relay configuration_error configuration_errors_total=%d; refusing claim and bind: %v", count, err)
	return err
}

type Config struct {
	Store            sessionstore.RelayLeases
	Key              string
	Process          sessionstore.RelayProcess
	TTL, Renew, Poll time.Duration
	// Only plain startup may reclaim an expired lease with unknown-host evidence.
	AllowUnknownHostRestart bool
	// Fence is injectable for deterministic safety tests. Nil uses kernel identity.
	Fence func(context.Context, processidentity.Identity) error
	// Gone only observes identity; it must never signal a live or stopped holder.
	Gone func(context.Context, processidentity.Identity) (bool, error)
}
type Timing struct {
	ClaimAt         time.Time     `json:"claim_at"`
	Wait            time.Duration `json:"wait_ns"`
	Fence           time.Duration `json:"fence_ns"`
	Activate        time.Duration `json:"activate_ns"`
	FencingFailures uint64        `json:"fencing_failures"`
	StoreFailures   uint64        `json:"store_failures"`
}
type Guard struct {
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	valid        atomic.Bool
	mu           sync.Mutex
	onLost       func()
	lostHookOnce sync.Once
	store        sessionstore.RelayLeases
	lease        sessionstore.RelayLease
	timing       Timing
	err          error
}

func (g *Guard) Context() context.Context       { return g.ctx }
func (g *Guard) Allowed() bool                  { return g.valid.Load() && g.ctx.Err() == nil }
func (g *Guard) Lease() sessionstore.RelayLease { return g.lease }
func (g *Guard) Timing() Timing                 { return g.timing }
func (g *Guard) Err() error                     { g.mu.Lock(); defer g.mu.Unlock(); return g.err }

// Release requires stopped forwarding and closed sockets. Close must run first.
func (g *Guard) Release(ctx context.Context) error { return g.store.ReleaseRelay(ctx, g.lease) }
func (g *Guard) Close()                            { g.cancel(); <-g.done }

// OnLost installs the immediate packet-stop hook, including a loss that raced
// relay construction. It must not wait for the relay's background goroutines.
func (g *Guard) OnLost(fn func()) {
	g.mu.Lock()
	g.onLost = fn
	lost := g.err != nil
	g.mu.Unlock()
	if lost {
		g.lostHookOnce.Do(fn)
	}
}
func (g *Guard) lose(err error) {
	g.valid.Store(false)
	g.mu.Lock()
	g.err = err
	fn := g.onLost
	g.mu.Unlock()
	if fn != nil {
		g.lostHookOnce.Do(fn)
	}
	g.cancel()
}

// Acquire binds nothing. Even an uncertain claim is not permission to signal
// or bind. The next claim can settle the same holder idempotently in the store.
func Acquire(ctx context.Context, cfg Config) (*Guard, error) {
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Renew == 0 {
		cfg.Renew = DefaultRenew
	}
	if cfg.Poll == 0 {
		cfg.Poll = DefaultPoll
	}
	if cfg.Store == nil || cfg.Key == "" || cfg.Process.Owner == "" || cfg.Process.PID <= 0 || cfg.Process.Start == "" || cfg.TTL < time.Millisecond || cfg.Renew <= 0 || cfg.Renew >= cfg.TTL || cfg.Poll <= 0 {
		return nil, errors.New("relay lease needs a store, key and 0 < renewal < TTL, poll > 0")
	}
	if cfg.Process.Host == "" || cfg.Process.Namespace == "" {
		return nil, configurationError(processidentity.ErrForeignScope)
	}
	if cfg.Process.Host == processidentity.UnknownHost && !cfg.AllowUnknownHostRestart {
		return nil, configurationError(processidentity.ErrForeignScope)
	}
	if cfg.Process.Executable == "" {
		return nil, configurationError(processidentity.ErrExecutable)
	}
	if cfg.Fence == nil {
		cfg.Fence = func(ctx context.Context, id processidentity.Identity) error {
			return processidentity.FenceFrom(ctx, cfg.Process.Identity(), id)
		}
	}
	if cfg.Gone == nil {
		cfg.Gone = func(ctx context.Context, id processidentity.Identity) (bool, error) {
			return processidentity.GoneFrom(ctx, cfg.Process.Identity(), id)
		}
	}
	started := time.Now()
	var l sessionstore.RelayLease
	var err error
	var failures uint64
	tick := time.NewTicker(cfg.Poll)
	defer tick.Stop()

	var observation staleness
	var hadStoreFailure, sawRecord bool
	var unknownClaim bool
	for {
		op, cancel := context.WithTimeout(ctx, cfg.Poll)
		current, readErr := cfg.Store.GetRelay(op, cfg.Key)
		cancel()
		claim := errors.Is(readErr, sessionstore.ErrNotFound)
		missing := claim
		stale := observation.observe(current, missing, readErr, time.Now(), cfg.TTL, cfg.Renew)
		if readErr != nil && !missing {
			hadStoreFailure = true
		}
		// Fresh empty-store bootstrap is immediate. Absence after a failed poll
		// or previously observed holder gives that active a full TTL to restore.
		claim = missing && (!hadStoreFailure && !sawRecord || stale)
		deadClaim := false
		unknownClaim = false
		if readErr == nil {
			sawRecord = true
			unknownClaim = cfg.AllowUnknownHostRestart && hasUnknownHost(current) && (current.Expired || current.Holder == cfg.Process)
			for _, p := range []sessionstore.RelayProcess{current.Holder, current.Forwarder, current.PreviousHolder} {
				if p == (sessionstore.RelayProcess{}) {
					continue
				}
				if unknownClaim && p.Host == processidentity.UnknownHost {
					continue // Never interpret an unknown-host PID.
				}
				if err := processidentity.SameScope(cfg.Process.Identity(), p.Identity()); err != nil {
					return nil, configurationError(err)
				}
			}
			switch {
			case unknownClaim:
				claim = current.Holder == cfg.Process || current.Expired && stale
			case current.Holder.Owner == "", current.Holder == cfg.Process:
				claim = true // bootstrap, graceful release, or settle an uncertain claim
			default:
				op, cancel = context.WithTimeout(ctx, cfg.Poll)
				deadClaim = predecessorsGone(op, current, cfg.Gone)
				cancel()
				claim = deadClaim
				if !deadClaim && current.Expired && stale {
					claim = true
				}
			}
		}
		err = readErr
		if claim {
			op, cancel = context.WithTimeout(ctx, cfg.Poll)
			if unknownClaim {
				l, err = cfg.Store.ClaimExpiredUnknownRelay(op, current, cfg.Process, cfg.TTL)
			} else if deadClaim {
				l, err = cfg.Store.ClaimDeadRelay(op, current, cfg.Process, cfg.TTL)
			} else {
				l, err = cfg.Store.ClaimRelay(op, cfg.Key, cfg.Process, cfg.TTL)
			}
			cancel()
			if err == nil {
				break
			}
			if errors.Is(err, processidentity.ErrForeignScope) {
				return nil, configurationError(err)
			}
			observation.reset() // rejected or uncertain claim needs fresh evidence
		}
		if err != nil && !errors.Is(err, sessionstore.ErrNotFound) && !errors.Is(err, sessionstore.ErrLeaseHeld) && !errors.Is(err, sessionstore.ErrLeaseLost) {
			hadStoreFailure = true
			failures++
			log.Printf("relay lease claim unavailable; no takeover: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}

	owned, cancel := context.WithCancel(ctx)
	g := &Guard{ctx: owned, cancel: cancel, done: make(chan struct{}), store: cfg.Store, lease: l, timing: Timing{ClaimAt: time.Now(), Wait: time.Since(started), StoreFailures: failures}}
	g.valid.Store(true)
	go g.renew(cfg)
	fail := func(err error) (*Guard, error) { g.Close(); return nil, err }
	fenceAt := time.Now()
	seen := make(map[sessionstore.RelayProcess]bool)
	for _, p := range []sessionstore.RelayProcess{l.PreviousHolder, l.Forwarder} {
		if p.PID == 0 || p == cfg.Process || seen[p] {
			continue
		}
		if unknownClaim && p.Host == processidentity.UnknownHost {
			continue // Single-relay restart assumes the old relay was stopped.
		}
		seen[p] = true
		op, stop := context.WithTimeout(owned, cfg.TTL)
		err = cfg.Fence(op, p.Identity())
		stop()
		if err != nil {
			g.timing.FencingFailures++
			log.Printf("relay fencing_failed pid=%d fencing_failures=%d; refusing bind: %v", p.PID, g.timing.FencingFailures, err)
			return fail(fmt.Errorf("fence relay pid %d: %w", p.PID, err))
		}
	}
	g.timing.Fence = time.Since(fenceAt)
	activateAt := time.Now()

	for {
		op, stop := context.WithTimeout(owned, cfg.Renew)
		_, err = cfg.Store.ActivateRelay(op, l)
		stop()
		if err == nil {
			break
		}
		if errors.Is(err, sessionstore.ErrLeaseLost) {
			return fail(err)
		}
		if owned.Err() != nil {
			return fail(owned.Err())
		}
		g.timing.StoreFailures++
		log.Printf("relay activation unavailable; retrying without binding: %v", err)
		select {
		case <-owned.Done():
			return fail(owned.Err())
		case <-tick.C:
		}
	}

	if !g.Allowed() {
		return fail(sessionstore.ErrLeaseLost)
	}
	g.timing.Activate = time.Since(activateAt)
	return g, nil
}

func hasUnknownHost(l sessionstore.RelayLease) bool {
	return l.Holder.Host == processidentity.UnknownHost || l.Forwarder.Host == processidentity.UnknownHost || l.PreviousHolder.Host == processidentity.UnknownHost
}
func (g *Guard) renew(cfg Config) {
	defer close(g.done)
	tick := time.NewTimer(cfg.Renew)
	defer tick.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-tick.C:
		}
		op, stop := context.WithTimeout(g.ctx, cfg.Renew)
		_, err := cfg.Store.RenewRelay(op, g.lease, cfg.TTL)
		stop()
		if errors.Is(err, sessionstore.ErrLeaseLost) {
			g.lose(err)
			return
		}
		delay := cfg.Renew
		if err != nil && g.ctx.Err() == nil {
			delay = min(RetryRenew, cfg.Renew)
			// No expiry-based self-fence: during an outage only this process can
			// forward. Once Redis returns, the CAS observes any successor.
			log.Printf("relay lease renewal unavailable; retaining forwarding: %v", err)
		}
		tick.Reset(delay)
	}
}

// Claim-before-expiry needs every possible forwarder to be verifiably gone.
// The store CAS rechecks this snapshot's identities and epoch before replacing
// it. Any permission failure or uncertainty preserves the normal expiry path.
func predecessorsGone(ctx context.Context, l sessionstore.RelayLease, check func(context.Context, processidentity.Identity) (bool, error)) bool {
	seen := make(map[processidentity.Identity]bool)
	for _, p := range []sessionstore.RelayProcess{l.Holder, l.Forwarder} {
		if p == (sessionstore.RelayProcess{}) {
			continue
		}
		id := p.Identity()
		if id.Host == processidentity.UnknownHost {
			return false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		exited, err := check(ctx, id)
		if err != nil || !exited {
			return false
		}
	}
	return true
}
