package operatorgates

import (
	"encoding/json"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestReadiness(t *testing.T) {
	if err := CheckReadiness(); err != nil {
		t.Fatal(err)
	}
}

// Construct a complete accounting control only inside this validator test. Any
// pending integration rows remain pending in the shipped embedded inventory.
func readinessControl(t *testing.T) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	if err := fs.WalkDir(readinessFiles, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(readinessFiles, path)
		if err != nil {
			return err
		}
		files[path] = &fstest.MapFile{Data: data}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rewrite := func(name string, update func(map[string]any)) {
		var value map[string]any
		if err := json.Unmarshal(files[name].Data, &value); err != nil {
			t.Fatal(err)
		}
		update(value)
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: data}
	}
	rewrite("testdata/coverage.json", func(value map[string]any) {
		for _, g := range value["groups"].([]any) {
			group := g.(map[string]any)
			if group["id"] == "strict-verdict" {
				group["evidence"] = "tested"
				group["tests"] = []any{"validator-control"}
			}
		}
	})
	rewrite("testdata/mutation-coverage.json", func(value map[string]any) {
		for _, row := range value["pending"].([]any) {
			record := row.(map[string]any)
			record["evidence"] = "retired"
			record["owner"] = "removed"
			record["reason"] = "validator test control only"
			record["successorContract"] = "validator test control"
			record["successorMutation"] = "caller-single-launch"
			record["test"] = "TestSingleLaunchAndUnknownStop"
			value["retired"] = append(value["retired"].([]any), record)
		}
		value["pending"] = []any{}
	})
	return files
}
func TestReadinessRejectsIncompleteAccounting(t *testing.T) {
	if err := checkReadiness(readinessControl(t)); err != nil {
		t.Fatal("complete validator control", err)
	}
	for _, fault := range []string{"required-group", "missing-group", "disabled-group", "pending-case", "unbound-delta", "unbound-representation", "wrong-invocation", "missing-reason", "changed-guard", "unproved-retirement"} {
		t.Run(fault, func(t *testing.T) {
			source := readinessControl(t)
			file := "testdata/coverage.json"
			if fault == "pending-case" || fault == "changed-guard" || fault == "unproved-retirement" {
				file = "testdata/mutation-coverage.json"
			}
			if fault == "unbound-delta" || fault == "unbound-representation" || fault == "wrong-invocation" {
				file = "testdata/equivalence-deltas.json"
			}
			var value map[string]any
			if err := json.Unmarshal(source[file].Data, &value); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "required-group":
				value["groups"].([]any)[0].(map[string]any)["evidence"] = "pending"
			case "missing-group":
				value["groups"] = value["groups"].([]any)[1:]
			case "disabled-group":
				value["groups"].([]any)[0].(map[string]any)["required"] = false
			case "pending-case":
				value["pending"] = []any{map[string]any{"id": "unresolved", "owner": "public-go", "evidence": "pending"}}
			case "unbound-delta":
				delete(value["deltas"].([]any)[0].(map[string]any), "bindings")
			case "unbound-representation":
				delete(value["representations"].([]any)[0].(map[string]any), "bindings")
			case "wrong-invocation":
				value["deltas"].([]any)[0].(map[string]any)["bindings"].([]any)[0].(map[string]any)["invocation"] = 999999
			case "missing-reason":
				found := false
				for _, row := range value["fixtures"].([]any) {
					r := row.(map[string]any)
					if r["fullReplay"] == "non-replayable" {
						delete(r, "reason")
						found = true
						break
					}
				}
				if !found {
					t.Fatal("no non-replayable control")
				}
			case "changed-guard":
				value["killed"].([]any)[0].(map[string]any)["guardFingerprint"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "unproved-retirement":
				value["retired"].([]any)[0].(map[string]any)["successorMutation"] = "missing"
			}
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			source[file] = &fstest.MapFile{Data: data}
			if err := checkReadiness(source); err == nil {
				t.Fatal("MUTATION: readiness accepted " + fault)
			}
		})
	}
}
