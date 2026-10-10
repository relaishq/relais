// Package workerprobe supplies an opt-in test seam for a deliberately faulty
// relay worker. It is internal, disabled by default, and never changes the
// production session fence or ownership checks.
package workerprobe

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
)

// Sender sends independently encrypted marked media on the old private leg,
// even after the real session has been exported. It returns the ciphertext
// so a test can check the caller's raw UDP observations as well as decoding.
type Sender func([]byte) ([]byte, error)

// Capture builds a zombie sender from one live session without changing it.
type Capture func(string) (Sender, error)

type registration struct {
	initialSequence *[2]uint16
	capture         Capture
	ignoredBarriers int
	lifecycle       Lifecycle
	beforeSnapshot  func(context.Context, string)
	afterEcho       func(context.Context, string, []byte)
	afterResume     func(string, bool)
}

// enabled keeps every production probe call off the process-wide mutex.
var enabled atomic.Bool

var registry struct {
	sync.Mutex
	users   int
	workers map[netip.AddrPort]*registration
}

// Enable acquires a test scope before workers start. Scopes are reference
// counted: overlapping tests keep each other's registrations. Cleanup is
// idempotent; the last cleanup disables and clears the internal registry.
func Enable() func() {
	registry.Lock()
	if registry.users == 0 {
		registry.workers = make(map[netip.AddrPort]*registration)
	}
	registry.users++
	enabled.Store(true)
	registry.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.Lock()
			defer registry.Unlock()

			registry.users--
			if registry.users == 0 {
				enabled.Store(false)
				registry.workers = nil
			}
		})
	}
}

// Register attaches a relay worker only while at least one test scope exists.
func Register(addr netip.AddrPort, capture Capture) {
	if !enabled.Load() {
		return
	}

	registry.Lock()
	defer registry.Unlock()

	if registry.users > 0 {
		registry.workers[addr] = &registration{capture: capture}
	}
}

// Remove detaches a closed relay worker without affecting other workers.
func Remove(addr netip.AddrPort) {
	if !enabled.Load() {
		return
	}

	registry.Lock()
	defer registry.Unlock()

	delete(registry.workers, addr)
}

// Zombie captures a sender from an enabled relay worker and a live session.
func Zombie(addr netip.AddrPort, id string) (Sender, error) {
	registry.Lock()
	worker := registry.workers[addr]
	registry.Unlock()
	if worker == nil {
		return nil, errors.New("workerprobe: test probe not enabled for worker")
	}

	return worker.capture(id)
}

// IgnoreBarriers acquires an enabled relay worker's silent-barrier test scope.
// It affects acknowledgements only; the session and its lease keep running.
func IgnoreBarriers(addr netip.AddrPort) (func(), error) {
	registry.Lock()
	worker := registry.workers[addr]
	if worker == nil {
		registry.Unlock()
		return nil, errors.New("workerprobe: test probe not enabled for worker")
	}
	worker.ignoredBarriers++
	registry.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.Lock()
			defer registry.Unlock()
			worker.ignoredBarriers--
		})
	}, nil
}

// BarrierIgnored is inert unless Enable and IgnoreBarriers scopes are active.
func BarrierIgnored(addr netip.AddrPort) bool {
	if !enabled.Load() {
		return false
	}

	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	registry.Lock()
	defer registry.Unlock()
	worker := registry.workers[addr]
	return worker != nil && worker.ignoredBarriers > 0
}

// Lifecycle supplies crash/pause actions only to opt-in internal test scopes.
type Lifecycle struct {
	Kill          func() error
	Pause         func(bool)
	SnapshotReady func(string) bool
}

// RegisterLifecycle is inert outside Enable scopes.
func RegisterLifecycle(addr netip.AddrPort, actions Lifecycle) {
	if !enabled.Load() {
		return
	}

	registry.Lock()
	defer registry.Unlock()
	if w := registry.workers[addr]; w != nil {
		w.lifecycle = actions
	}
}

// Kill closes a worker's sockets and discards its memory without a flush.
func Kill(addr netip.AddrPort) error {
	registry.Lock()
	w := registry.workers[addr]
	registry.Unlock()
	if w == nil || w.lifecycle.Kill == nil {
		return errors.New("workerprobe: no lifecycle probe")
	}
	return w.lifecycle.Kill()
}

// Pause stops packet processing, snapshots, renewal and heartbeats while
// retaining memory and the private socket. Zombie can still send on it.
func Pause(addr netip.AddrPort, paused bool) error {
	registry.Lock()
	w := registry.workers[addr]
	registry.Unlock()
	if w == nil || w.lifecycle.Pause == nil {
		return errors.New("workerprobe: no lifecycle probe")
	}
	w.lifecycle.Pause(paused)
	return nil
}

// SetBeforeSnapshot installs a cancellable gate just before a background copy.
// A gate can reliably kill at the end of a snapshot interval, without putting
// test hooks on the exported production worker API.
func SetBeforeSnapshot(addr netip.AddrPort, hook func(context.Context, string)) error {
	registry.Lock()
	defer registry.Unlock()
	w := registry.workers[addr]
	if w == nil {
		return errors.New("workerprobe: no snapshot probe")
	}
	w.beforeSnapshot = hook
	return nil
}

// BeforeSnapshot runs the opt-in background gate outside the session lock.
func BeforeSnapshot(addr netip.AddrPort, ctx context.Context, id string) {
	if !enabled.Load() {
		return
	}

	registry.Lock()
	var hook func(context.Context, string)
	if w := registry.workers[addr]; w != nil {
		hook = w.beforeSnapshot
	}
	registry.Unlock()
	if hook != nil {
		hook(ctx, id)
	}
}

// SetAfterEcho installs a packet gate after a successfully encrypted echo.
// The packet is plaintext, borrowed for the duration of the callback. Gates
// must return when ctx ends; Kill cancels ctx before waiting for packet locks.
func SetAfterEcho(addr netip.AddrPort, hook func(context.Context, string, []byte)) error {
	registry.Lock()
	defer registry.Unlock()
	w := registry.workers[addr]
	if w == nil {
		return errors.New("workerprobe: no packet probe")
	}
	w.afterEcho = hook
	return nil
}

// AfterEcho is inert unless a packet gate was installed in a test scope.
func AfterEcho(addr netip.AddrPort, ctx context.Context, id string, packet []byte) {
	if !enabled.Load() {
		return
	}

	registry.Lock()
	var hook func(context.Context, string, []byte)
	if w := registry.workers[addr]; w != nil {
		hook = w.afterEcho
	}
	registry.Unlock()
	if hook != nil {
		hook(ctx, id, packet)
	}
}

// SnapshotReady observes completion of the initial put without exposing the
// blob or requesting a fresh snapshot. It is available only in Enable scopes.
func SnapshotReady(addr netip.AddrPort, id string) bool {
	registry.Lock()
	var ready func(string) bool
	if w := registry.workers[addr]; w != nil {
		ready = w.lifecycle.SnapshotReady
	}
	registry.Unlock()
	return ready != nil && ready(id)
}

// SetAfterResume observes whether takeover needs a PLI before its source SSRC
// is learned. It is internal and unavailable outside Enable scopes.
func SetAfterResume(addr netip.AddrPort, hook func(string, bool)) error {
	registry.Lock()
	defer registry.Unlock()
	w := registry.workers[addr]
	if w == nil {
		return errors.New("workerprobe: no resume probe")
	}
	w.afterResume = hook
	return nil
}

func AfterResume(addr netip.AddrPort, id string, pendingPLI bool) {
	if !enabled.Load() {
		return
	}
	registry.Lock()
	var hook func(string, bool)
	if w := registry.workers[addr]; w != nil {
		hook = w.afterResume
	}
	registry.Unlock()
	if hook != nil {
		hook(id, pendingPLI)
	}
}

// SetInitialSequences overrides new outbound tracks on one enabled relay
// worker. It is an internal harness option; production keeps random starts.
func SetInitialSequences(addr netip.AddrPort, audio, video uint16) error {
	registry.Lock()
	defer registry.Unlock()
	w := registry.workers[addr]
	if w == nil {
		return errors.New("workerprobe: no sequence probe")
	}
	w.initialSequence = &[2]uint16{audio, video}
	return nil
}

// InitialSequences is inert outside an opt-in harness scope.
func InitialSequences(addr netip.AddrPort) (audio, video uint16, ok bool) {
	if !enabled.Load() {
		return 0, 0, false
	}
	registry.Lock()
	defer registry.Unlock()
	if w := registry.workers[addr]; w != nil && w.initialSequence != nil {
		return w.initialSequence[0], w.initialSequence[1], true
	}
	return 0, 0, false
}
