package main

import "testing"

func TestOnlyNamedAssertionFailureKills(t *testing.T) {
	valid := []event{{Action: "run", Test: "TestGuard"}, {Action: "output", Test: "TestGuard", Output: "MUTATION: guard"}, {Action: "fail", Test: "TestGuard"}}
	if !assertionFailure(valid, "TestGuard", "MUTATION: guard") {
		t.Fatal("named assertion was not counted")
	}
	for _, fault := range []string{"panic: setup", "fatal error: runtime", "build failed", "test timed out"} {
		events := append(append([]event{}, valid...), event{Action: "output", Output: fault})
		if assertionFailure(events, "TestGuard", "MUTATION: guard") {
			t.Fatalf("invalid kill: %s", fault)
		}
	}
	for _, events := range [][]event{valid[1:], valid[:2], {{Action: "run", Test: "TestGuard"}, {Action: "output", Test: "TestOther", Output: "MUTATION: guard"}, {Action: "fail", Test: "TestGuard"}}, {{Action: "run", Test: "TestGuard"}, {Action: "output", Test: "TestGuard", Output: "INVALID Python guard"}, {Action: "fail", Test: "TestGuard"}}} {
		if assertionFailure(events, "TestGuard", "MUTATION: guard") {
			t.Fatal("missing test or unrelated failure counted")
		}
	}
}
