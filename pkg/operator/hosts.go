package operator

import "slices"

// HostSlots returns the ordered host inventory committed by the scenario.
// Parts that omit RelayTwo retain the original four hosts.
func HostSlots(relayTwo bool) []string {
	slots := []string{"relay", "w1", "w2", "shared"}
	if relayTwo {
		slots = append(slots, "relay-two")
	}
	return slots
}

// ValidHostSet admits exactly the four-host set or that set plus relay-two.
func ValidHostSet(slots []string) bool {
	if len(slots) != 4 && len(slots) != 5 {
		return false
	}
	expected := HostSlots(len(slots) == 5)
	seen := map[string]bool{}
	for _, slot := range slots {
		if seen[slot] || !slices.Contains(expected, slot) {
			return false
		}
		seen[slot] = true
	}
	return true
}
