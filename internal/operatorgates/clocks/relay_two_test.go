package clocks

import (
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/observe"
)

func TestRelayTwoClockAdmission(t *testing.T) {
	in, policy := selectionFixture()
	extra := in.Hosts[0]
	extra.Slot = "relay-two"
	in.Hosts = append(in.Hosts, extra)
	selected, err := Select(in, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Receipt.Hosts) != 5 {
		t.Fatal("optional clock evidence dropped")
	}
	binding := observe.ClockBinding{Receipt: selected.Receipt, Binding: observe.ReceiptBinding{ReceiptSHA256: selected.SHA256, EvidenceSHA256: selected.Receipt.EvidenceSHA256, Bounds: selected.Receipt.Bounds}}
	if !binding.Valid(selected.SHA256) {
		t.Fatal("five-host receipt rejected")
	}
	reads := []BindingHost{}
	for _, h := range in.Hosts {
		reads = append(reads, BindingHost{Slot: h.Slot, LeapMode: h.LeapMode, Tracking: h.Tracking})
		reads[len(reads)-1].Tracking.RunStartUnixMS = (in.EndUnixSeconds + 3) * 1000
	}
	if err = bindingReadback(in, reads); err != nil {
		t.Fatal(err)
	}
	if err = bindingReadback(in, reads[:4]); err == nil {
		t.Fatal("missing relay-two readback admitted")
	}
	in.Hosts[4].Slot = "other"
	if _, err = Select(in, policy); err == nil || err.Error() != "clock-select-admission" {
		t.Fatal("unknown fifth slot admitted", err)
	}
	rows := map[string]Sample{}
	for _, slot := range []string{"relay", "w1", "w2", "shared"} {
		rows[slot] = Sample{}
	}
	if _, err = Check(rows, Bounds{}, "shared", "shared", true); err == nil {
		t.Fatal("missing second relay clock admitted")
	}
	rows["relay-two"] = Sample{}
	if _, err = Check(rows, Bounds{}, "shared", "shared", true); err != nil {
		t.Fatal(err)
	}
}

func TestRelayTwoClockRoleRequiresHost(t *testing.T) {
	in, policy := selectionFixture()
	in.FrontendSlot = "relay-two"
	if _, err := Select(in, policy); err == nil || err.Error() != "clock-select-admission" {
		t.Fatal("absent frontend admitted", err)
	}
	if err := CheckConvergence(in, policy); err == nil || err.Error() != "clock-select-admission" {
		t.Fatal("absent convergence frontend admitted", err)
	}
}
