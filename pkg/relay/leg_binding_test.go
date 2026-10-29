package relay

import (
	"net/netip"
	"testing"
	"time"
)

func TestLegBindingNeedsControlConfirmation(t *testing.T) {
	r := &Relay{registry: make(map[netip.AddrPort]struct{})}
	token := "00112233445566778899aabbccddeeff"
	a := netip.MustParseAddrPort("192.0.2.1:17001")
	b := netip.MustParseAddrPort("192.0.2.1:17002")
	if r.observeLeg(token, a) {
		t.Fatal("unexpected token admitted")
	}
	if err := r.ExpectWorker(token); err != nil {
		t.Fatal(err)
	}
	if err := r.AddBoundWorker(a, token); err == nil {
		t.Fatal("token admitted before observed hello")
	}
	if !r.observeLeg(token, a) {
		t.Fatal("expected hello rejected")
	}
	if r.registered(a) {
		t.Fatal("hello admitted media without control")
	}
	if err := r.AddBoundWorker(b, token); err == nil {
		t.Fatal("wrong address admitted")
	}
	if err := r.AddBoundWorker(a, token); err != nil {
		t.Fatal(err)
	}
	if !r.registered(a) {
		t.Fatal("confirmed address not admitted")
	}
	if !r.observeLeg(token, b) {
		t.Fatal("rebind not reported")
	}
	if err := r.AddBoundWorker(b, token); err == nil {
		t.Fatal("token moved the binding")
	}
	if r.registered(b) || !r.registered(a) {
		t.Fatal("NAT rebind changed allowlist")
	}
	r.bindings[token] = legBinding{expires: time.Now().Add(-time.Second)}
	if r.observeLeg(token, a) {
		t.Fatal("expired token accepted")
	}
}
func TestReconnectReplacesLegTokenWithoutLeakingCapacity(t *testing.T) {
	r := &Relay{registry: make(map[netip.AddrPort]struct{})}
	addr := netip.MustParseAddrPort("192.0.2.1:17001")
	for _, token := range []string{"00112233445566778899aabbccddeeff", "112233445566778899aabbccddeeff00"} {
		if err := r.ExpectWorker(token); err != nil {
			t.Fatal(err)
		}
		r.observeLeg(token, addr)
		if err := r.AddBoundWorker(addr, token); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.bindings) != 1 {
		t.Fatal("reconnect retained obsolete binding tokens")
	}
}
