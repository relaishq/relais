package workerprobe

import (
	"errors"
	"net/netip"
)

// SetHoldTaskResults pauses mailbox consumption without pausing the actor,
// task admission, export or checkpoints. It is an opt-in test seam only.
func SetHoldTaskResults(addr netip.AddrPort, hold bool) error {
	registry.Lock()
	defer registry.Unlock()
	w := registry.workers[addr]
	if w == nil {
		return errors.New("workerprobe: no task result probe")
	}
	w.holdTaskResults = hold
	return nil
}
func HoldTaskResults(addr netip.AddrPort) bool {
	if !enabled.Load() {
		return false
	}
	registry.Lock()
	defer registry.Unlock()
	w := registry.workers[addr]
	return w != nil && w.holdTaskResults
}
