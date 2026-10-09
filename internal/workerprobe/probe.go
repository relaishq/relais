// Package workerprobe supplies an opt-in test seam for a deliberately faulty
// relay worker. It is internal, disabled by default, and never changes the
// production session fence or ownership checks.
package workerprobe

import (
	"errors"
	"net/netip"
	"sync"
)

// Sender sends independently encrypted marked media on the old private leg,
// even after the real session has been exported. It returns the ciphertext
// so a test can check the caller's raw UDP observations as well as decoding.
type Sender func([]byte) ([]byte, error)

// Capture builds a zombie sender from one live session without changing it.
type Capture func(string) (Sender, error)

type registration struct {
	capture         Capture
	ignoredBarriers int
}

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
	registry.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.Lock()
			defer registry.Unlock()

			registry.users--
			if registry.users == 0 {
				registry.workers = nil
			}
		})
	}
}

// Register attaches a relay worker only while at least one test scope exists.
func Register(addr netip.AddrPort, capture Capture) {
	registry.Lock()
	defer registry.Unlock()

	if registry.users > 0 {
		registry.workers[addr] = &registration{capture: capture}
	}
}

// Remove detaches a closed relay worker without affecting other workers.
func Remove(addr netip.AddrPort) {
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
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	registry.Lock()
	defer registry.Unlock()
	worker := registry.workers[addr]
	return worker != nil && worker.ignoredBarriers > 0
}
