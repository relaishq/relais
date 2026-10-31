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
			edits = []change{{File: m.File, Function: m.Function, Old: m.Old, New: m.New, Receiver: m.Receiver}}
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

func TestMutationGroupsKeepEveryIdentity(t *testing.T) {
	first := mutation{ID: "original", GuardFingerprint: "frozen", File: "guard.go", Function: "guard", Old: "true", New: "false", Test: "TestGuard", Marker: "MUTATION:", Package: "./guard"}
	alias := first
	alias.ID = "successor"
	alias.GuardFingerprint = ""
	other := first
	other.ID = "different-assertion"
	other.Test = "TestOther"
	groups := groupMutations([]mutation{first, alias, other})
	if len(groups) != 2 || len(groups[0]) != 2 || groups[0][0].ID != "original" || groups[0][1].ID != "successor" || groups[1][0].ID != "different-assertion" {
		t.Fatal("mutation grouping lost an identity or assertion")
	}
}
func TestMutationBaselineIncludesEveryNamedParent(t *testing.T) {
	pattern := baselinePattern([]mutation{{Test: "TestA/one"}, {Test: "TestA/two"}, {Test: "TestB"}})
	if pattern != "^(TestA|TestB)$" {
		t.Fatal("baseline selection changed", pattern)
	}
}
