package callharness

import "errors"

// ErrHarnessClosed is returned by Dial once the harness is closing.
var ErrHarnessClosed = errors.New("callharness: harness closed")

// The harness's calls live under one lock with a closed flag: Dial registers
// a call only while the harness is open, and Close marks it closed and then
// closes every call still registered, before it stops the workers. So no
// call starts after Close began, and no live caller is cut off by its worker
// shutting down under it. Each call's own goroutines follow the same rule
// under the call's lock (Call.startReader, Call.close).

// addCall registers a call, unless the harness is closing.
func (h *Harness) addCall(c *Call) error {
	h.callsMu.Lock()
	defer h.callsMu.Unlock()
	if h.closed {
		return ErrHarnessClosed
	}
	if h.calls == nil {
		h.calls = make(map[*Call]struct{})
	}
	h.calls[c] = struct{}{}

	return nil
}

// removeCall forgets a closed call.
func (h *Harness) removeCall(c *Call) {
	h.callsMu.Lock()
	defer h.callsMu.Unlock()
	delete(h.calls, c)
}

// closeCalls marks the harness closed and closes the calls still open.
func (h *Harness) closeCalls() error {
	h.callsMu.Lock()
	h.closed = true
	calls := make([]*Call, 0, len(h.calls))
	for c := range h.calls {
		calls = append(calls, c)
	}
	h.callsMu.Unlock()

	var errs []error
	for _, c := range calls {
		errs = append(errs, c.close())
	}

	return errors.Join(errs...)
}
