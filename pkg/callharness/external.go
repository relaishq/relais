package callharness

import "time"

// RecordProcessKill aligns the control plane's takeover event with the real
// SIGKILL action. It does not cause or accelerate recovery.
func (h *Harness) RecordProcessKill(worker string, at time.Time) {
	h.callsMu.Lock()
	defer h.callsMu.Unlock()
	h.failures[worker] = append(h.failures[worker], at)
}
