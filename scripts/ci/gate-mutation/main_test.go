package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// The example cases mutate a package outside the operator: a declaration
// edit, a whole-file edit and a unified patch must each be killed.
func TestExampleCasesKillOutsideOperator(t *testing.T) {
	cases, err := loadCases("testdata/example-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "internal/iceufrag/ufrag.go")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := runCases(root, cases, "", 2); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("mutation run changed the checkout", err)
	}
}
func TestRestoreDetectsChangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.go")
	originals := map[string][]byte{path: []byte("package guard\n")}
	if err := restore(originals, os.WriteFile); err != nil {
		t.Fatal(err)
	}
	truncate := func(name string, data []byte, perm os.FileMode) error {
		return os.WriteFile(name, data[:len(data)-1], perm)
	}
	if err := restore(originals, truncate); err == nil || !strings.Contains(err.Error(), "restored file differs") {
		t.Fatal("changed restore was not detected", err)
	}
}
func TestPatchModifiesExistingFilesOnly(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	diff := filepath.Join(dir, "create.patch")
	if err := os.WriteFile(diff, []byte("diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+package guard\n"), 0600); err != nil {
		t.Fatal(err)
	}
	originals := map[string][]byte{}
	if err := patch(tree, diff, originals); err == nil || len(originals) != 0 {
		t.Fatal("patch created a file", err)
	}
	if _, err := os.Stat(filepath.Join(tree, "new.go")); !os.IsNotExist(err) {
		t.Fatal("refused patch changed the tree", err)
	}
}
func TestNumstatRowsFailCleanly(t *testing.T) {
	files, err := numstatFiles("1\t1\ta.go\x00-\t-\tb/c.bin\x00")
	if err != nil || len(files) != 2 || files[0] != "a.go" || files[1] != "b/c.bin" {
		t.Fatal("numstat paths", files, err)
	}
	for _, stat := range []string{"1\t1\x00", "a.go\x00", "1\t1\t\x00old.go\x00new.go\x00"} {
		if _, err := numstatFiles(stat); err == nil {
			t.Errorf("accepted %q", stat)
		}
	}
}
func TestCaseFileShape(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"patch-and-edit": `[{"ID":"a","Patch":"a.patch","File":"a.go","Old":"x","New":"y","Test":"TestA","Marker":"m","Package":"./a"}]`,
		"no-change":      `[{"ID":"a","Test":"TestA","Marker":"m","Package":"./a"}]`,
		"no-marker":      `[{"ID":"a","File":"a.go","Old":"x","New":"y","Test":"TestA","Package":"./a"}]`,
		"unknown-field":  `[{"ID":"a","File":"a.go","Old":"x","New":"y","Test":"TestA","Marker":"m","Package":"./a","Func":"F"}]`,
		"patch-and-old":  `[{"ID":"a","Patch":"a.patch","Old":"x","Test":"TestA","Marker":"m","Package":"./a"}]`,
		"patch-and-func": `[{"ID":"a","Patch":"a.patch","Function":"F","Test":"TestA","Marker":"m","Package":"./a"}]`,
		"patch-and-recv": `[{"ID":"a","Patch":"a.patch","Receiver":"T","Test":"TestA","Marker":"m","Package":"./a"}]`,
		"edit-no-file":   `[{"ID":"a","Edits":[{"Old":"x","New":"y"}],"Test":"TestA","Marker":"m","Package":"./a"}]`,
		"edit-no-old":    `[{"ID":"a","Edits":[{"File":"a.go","New":"y"}],"Test":"TestA","Marker":"m","Package":"./a"}]`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadCases(path); err == nil {
			t.Error("accepted", name)
		}
	}
	if _, err := loadCases(""); err != nil {
		t.Fatal("embedded operator cases", err)
	}
	cases, err := loadCases("testdata/example-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cases {
		if m.Patch != "" && !filepath.IsAbs(m.Patch) {
			t.Fatal("patch path not resolved against the case file", m.Patch)
		}
	}
}
