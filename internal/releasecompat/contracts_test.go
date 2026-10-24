package releasecompat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scriptTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, src := range files {
		dest := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestLuaDiscoveryFollowsEveryCallSite(t *testing.T) {
	root := scriptTree(t, map[string]string{
		"pkg/arbitrary/contract.go":     `package arbitrary; const part="return "; const body=part+"1"`,
		"pkg/arbitrary/run.go":          `package arbitrary; import r "github.com/redis/go-redis/v9"; var script=r.NewScript(body); func run(){client.Eval(ctx,body,keys);pipe.EvalSha(ctx,script.Hash(),keys); const local="return 2";client.Eval(ctx,local,keys)}`,
		"pkg/framecache/redis.go":       `package framecache; import "github.com/redis/go-redis/v9"; const appendLua="return 3";var appendScript=redis.NewScript(appendLua)`,
		"cmd/driver/run.go":             `package main;func run(){client.Eval(ctx,"return 4",keys)}`,
		"pkg/arbitrary/ignored_test.go": `package arbitrary;func test(){client.Eval(ctx,dynamic,keys)}`,
	})
	actual, err := LuaScripts(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pkg/arbitrary/body", "pkg/arbitrary/local", "pkg/framecache/appendLua", "cmd/driver/run.go:Eval:1"} {
		if _, ok := actual[name]; !ok {
			t.Fatalf("missing %s: %v", name, actual)
		}
	}
	if len(actual) != 4 {
		t.Fatalf("expected deduplicated complete scripts, got %v", actual)
	}
}
func TestLuaDiscoveryRejectsUnresolvedScript(t *testing.T) {
	for _, call := range []string{`redis.NewScript(dynamic)`, `client.Eval(ctx,dynamic,keys)`, `pipe.EvalSha(ctx,"unknown-sha",keys)`, `client.EvalSha(ctx,unknown.Hash(),keys)`} {
		t.Run(call, func(t *testing.T) {
			root := scriptTree(t, map[string]string{"newpkg/arbitrary.go": `package arbitrary;import "github.com/redis/go-redis/v9";func run(){` + call + `}`})
			if _, err := LuaScripts(root); err == nil {
				t.Fatal("unresolved script bypassed manifest")
			}
		})
	}
}
func TestNewerManifestRefusesBeforeUnknownFields(t *testing.T) {
	for _, input := range []string{`{"manifest_version":3,"future_contract":true}`, `{"inventory_version":1,"participants":[{"manifest":{"manifest_version":3,"future_contract":true}}]}`} {
		var err error
		if strings.Contains(input, "inventory_version") {
			var p Pool
			err = Decode([]byte(input), &p)
		} else {
			var m Manifest
			err = Decode([]byte(input), &m)
		}
		var r *Refusal
		if !errors.As(err, &r) || r.Code != "unsupported_manifest" || !strings.Contains(r.Reason, "unsupported newer manifest") {
			t.Fatalf("%s: %v", input, err)
		}
	}
}
func TestManifestScriptCountIsNotAReaderLimit(t *testing.T) {
	m := testManifest(t, "0.3.0")
	m.Scripts["new-script"] = Script{SHA: strings.Repeat("f", 40)}
	if err := m.Validate(); err != nil {
		t.Fatalf("additional script broke structural validation: %v", err)
	}
}
func TestKeyAndUDPFixtureEvolutionRequiresVersionBump(t *testing.T) {
	for _, domain := range []string{"keys", "udp"} {
		t.Run(domain, func(t *testing.T) {
			old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
			if domain == "keys" {
				next.KeyGoldens["framecache"] = strings.Repeat("f", 64)
			} else {
				next.UDPGolden = strings.Repeat("f", 64)
			}
			if err := CheckEvolution(old, next); err == nil {
				t.Fatal("changed golden bypassed version bump")
			}
			if domain == "keys" {
				next.KeyContract++
			} else {
				next.UDPContract++
			}
			if err := CheckEvolution(old, next); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestRollbackRetainsRedisKeyLayout(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	next.KeyContract++
	for role, profiles := range next.Roles {
		if role == "signal" {
			continue
		}
		for name, p := range profiles {
			p.Formats["redis_keys"] = Format{Reads: []int{1, 2}, Writes: []int{2}, Continues: []int{1, 2}}
			next.Roles[role][name] = p
		}
	}
	requireRefused(t, Preflight(testPool(next), old, "rollback"), "format_incompatible")
}
func TestStoreGoldenIncludesFrameCache(t *testing.T) {
	actual, err := LuaScripts("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pkg/framecache/appendLua", "pkg/framecache/currentLua"} {
		if actual[name] == "" {
			t.Fatalf("frame cache escaped inventory: %s", name)
		}
	}
	raw, err := os.ReadFile("../../pkg/sessionstore/testdata/script-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var g ScriptGolden
	if err = json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(g.Scripts) {
		t.Fatal("golden omitted discovered script")
	}
}
