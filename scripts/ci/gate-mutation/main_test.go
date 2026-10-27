package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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

func TestMutationEditsMatchSource(t *testing.T) {
	var cases []mutation
	if err := json.Unmarshal(specifications, &cases); err != nil {
		t.Fatal(err)
	}
	for _, m := range cases {
		edits := m.Edits
		if len(edits) == 0 {
			edits = []change{{m.File, m.Function, m.Old, m.New}}
		}
		for _, e := range edits {
			b, err := os.ReadFile(filepath.Join("../../..", e.File))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = edit(b, e); err != nil {
				t.Errorf("%s: %v", m.ID, err)
			}
		}
	}
}
