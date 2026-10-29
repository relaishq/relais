package relay

import (
	"errors"
	"net/netip"
	"time"

	"github.com/relaishq/relais/internal/relayleg"
)

type legBinding struct {
	address netip.AddrPort
	expires time.Time
}

// ExpectWorker admits only a hello, never media or a route. Control must still
// confirm the token/address pair with AddBoundWorker. Pending tokens expire.
func (r *Relay) ExpectWorker(token string) error {
	if len(relayleg.Binding(relayleg.Hello, token, netip.AddrPort{})) == 0 {
		return errors.New("relay: invalid leg token")
	}
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	if r.bindings == nil {
		r.bindings = make(map[string]legBinding)
	}
	now := time.Now()
	for key, b := range r.bindings {
		if !b.expires.IsZero() && now.After(b.expires) {
			delete(r.bindings, key)
		}
	}
	if _, ok := r.bindings[token]; ok {
		return nil
	}
	if len(r.bindings) >= 4096 {
		return errors.New("relay: leg binding capacity")
	}
	r.bindings[token] = legBinding{expires: now.Add(5 * time.Second)}
	return nil
}
func (r *Relay) AddBoundWorker(address netip.AddrPort, token string) error {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	b, ok := r.bindings[token]
	if !ok || b.address != address || !b.expires.IsZero() && time.Now().After(b.expires) {
		return errors.New("relay: leg token not bound to address")
	}
	for key, prior := range r.bindings {
		if key != token && prior.address == address {
			delete(r.bindings, key)
		}
	}
	b.expires = time.Time{}
	r.bindings[token] = b
	r.registry[address] = struct{}{}
	return nil
}
func (r *Relay) legBinding(packet []byte, from netip.AddrPort) bool {
	kind, token, _, ok := relayleg.ParseBinding(packet)
	if !ok {
		return false
	}
	if kind == relayleg.Observed {
		return true
	}
	valid := r.observeLeg(token, from)
	// A valid keepalive at a new source reports it but never changes a binding
	// or an allowlist entry. Only control can drain and replace that identity.
	if valid {
		_, _ = r.workers.WriteToUDPAddrPort(relayleg.Binding(relayleg.Observed, token, from), from)
	}
	return true
}

// observeLeg binds a token once. Rebinding remains control's decision.
func (r *Relay) observeLeg(token string, from netip.AddrPort) bool {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	b, known := r.bindings[token]
	valid := known && (b.expires.IsZero() || time.Now().Before(b.expires))
	if valid && !b.address.IsValid() {
		b.address = from
		r.bindings[token] = b
	}
	return valid
}
