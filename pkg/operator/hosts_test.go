package operator

import (
	"slices"
	"testing"
)

func TestRelayTwoScenarioIdentity(t *testing.T) {
	base := Scenario{SchemaVersion: 1}
	first, err := BuildScenarioIdentity(base, "")
	if err != nil {
		t.Fatal(err)
	}
	base.RelayTwo = true
	second, err := BuildScenarioIdentity(base, "")
	if err != nil || first.Digest == second.Digest {
		t.Fatal("host topology not bound to scenario", err)
	}
	if !slices.Equal(HostSlots(false), []string{"relay", "w1", "w2", "shared"}) {
		t.Fatal("default hosts changed")
	}
	for _, slots := range [][]string{HostSlots(false), HostSlots(true)} {
		if !ValidHostSet(slots) {
			t.Fatal("valid host set refused")
		}
	}
	for _, slots := range [][]string{{"relay", "w1", "w2", "relay-two"}, {"relay", "w1", "w2", "shared", "other"}, {"relay", "w1", "w2", "shared", "shared"}} {
		if ValidHostSet(slots) {
			t.Fatal("invalid host set admitted", slots)
		}
	}
}

func TestRelayTwoPublicOperationLabels(t *testing.T) {
	for _, action := range []string{"relay-isolate", "relay-restore"} {
		row := (Uncertainty{Target: "relay-two", Action: action}).Public()
		if row.Target != "relay-two" || row.Action != action {
			t.Fatal("optional host operation label lost", row)
		}
	}
}
