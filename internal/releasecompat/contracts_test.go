package releasecompat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/pkg/sessionstore"
)

func scriptTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files["cmd/relais-worker/main.go"] = `package main;import(_ "github.com/relaishq/relais/pkg/arbitrary";_ "github.com/relaishq/relais/pkg/framecache";_ "github.com/relaishq/relais/newpkg";_ "github.com/relaishq/relais/pkg/sessionstore")`
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
	for _, name := range []string{"pkg/arbitrary/body", "pkg/arbitrary/local", "pkg/framecache/appendLua"} {
		if _, ok := actual[name]; !ok {
			t.Fatalf("missing %s: %v", name, actual)
		}
	}
	if len(actual) != 3 {
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

func TestLuaReadOnlyCallPatterns(t *testing.T) {
	for _, method := range []string{"EvalRO", "EvalShaRO"} {
		t.Run(method, func(t *testing.T) {
			call := `c.` + method + `(ctx,body,nil)`
			constructor := ""
			if method == "EvalShaRO" {
				constructor = `var script=redis.NewScript(body);`
				call = `c.EvalShaRO(ctx,script.Hash(),nil)`
			}
			root := scriptTree(t, map[string]string{"pkg/arbitrary/scripts.go": `package arbitrary;import "github.com/redis/go-redis/v9";const body="return 2";` + constructor + `func run(){` + call + `}`})
			actual, err := LuaScripts(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(actual) != 1 || actual["pkg/arbitrary/body"] == "" {
				t.Fatalf("%s escaped inventory: %v", method, actual)
			}
		})
	}
	for _, call := range []string{`c.EvalRO(ctx,dynamic,nil)`, `c.EvalShaRO(ctx,"unknown",nil)`} {
		t.Run(call, func(t *testing.T) {
			root := scriptTree(t, map[string]string{"pkg/arbitrary/raw.go": `package arbitrary;func run(){` + call + `}`})
			if _, err := LuaScripts(root); err == nil {
				t.Fatal("unresolved read-only script escaped")
			}
		})
	}
}
func TestLuaRawCommandsRefuse(t *testing.T) {
	for _, command := range []string{"EVAL", "EVALSHA", "EVAL_RO", "EVALSHA_RO", "FCALL", "FCALL_RO"} {
		for _, method := range []string{"Do", "Process-inline", "Process-variable"} {
			t.Run(command+"/"+method, func(t *testing.T) {
				call := `c.Do(ctx,command,"return 2",0)`
				if method == "Process-inline" {
					call = `c.Process(ctx,redis.NewCmd(ctx,command,"return 2",0))`
				}
				if method == "Process-variable" {
					call = `cmd:=redis.NewCmd(ctx,command,"return 2",0);c.Process(ctx,cmd)`
				}
				root := scriptTree(t, map[string]string{"pkg/arbitrary/raw.go": `package arbitrary;import "github.com/redis/go-redis/v9";const command="` + strings.ToLower(command) + `";func run(){` + call + `}`})
				if _, err := LuaScripts(root); err == nil || !strings.Contains(err.Error(), "raw Redis") {
					t.Fatalf("%s escaped: %v", call, err)
				}
			})
		}
	}
	for _, call := range []string{`c.Do(ctx,dynamic)`, `c.Process(ctx,redis.NewCmd(ctx,dynamic))`, `c.Process(ctx,dynamic)`, `c.Process(ctx,factory.NewCmd(ctx,"PING"))`} {
		t.Run(call, func(t *testing.T) {
			root := scriptTree(t, map[string]string{"pkg/arbitrary/raw.go": `package arbitrary;import "github.com/redis/go-redis/v9";func run(){` + call + `}`})
			if _, err := LuaScripts(root); err == nil || !strings.Contains(err.Error(), "unresolved Redis") {
				t.Fatalf("unknown command escaped: %v", err)
			}
		})
	}
}
func TestLuaDiagnosticBinariesAreExcludedUnlessImported(t *testing.T) {
	root := scriptTree(t, map[string]string{"cmd/diagnostic/run.go": `package main;import _ "github.com/relaishq/relais/internal/diagnostic"`, "internal/diagnostic/run.go": `package diagnostic;func run(){c.EvalRO(ctx,dynamic,nil);c.Do(ctx,"FCALL","function",0)}`})
	actual, err := LuaScripts(root)
	if err != nil || len(actual) != 0 {
		t.Fatalf("diagnostic affected service contract: %v %v", actual, err)
	}
	if err = os.WriteFile(filepath.Join(root, "cmd/relais-worker/main.go"), []byte(`package main;import _ "github.com/relaishq/relais/internal/diagnostic"`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LuaScripts(root); err == nil {
		t.Fatal("imported scripts escaped reachability closure")
	}
}
func TestLuaInventoryNamesEveryAddedAndRemovedScript(t *testing.T) {
	root := scriptTree(t, map[string]string{"pkg/arbitrary/run.go": `package arbitrary;func run(){c.EvalRO(ctx,"return 2",nil);c.EvalRO(ctx,"return 3",nil)}`})
	golden := ScriptGolden{Contract: sessionstore.ScriptContractVersion, Scripts: map[string]Script{"old-a": {SHA: strings.Repeat("a", 40)}, "old-b": {SHA: strings.Repeat("b", 40)}}}
	data, err := JSON(golden)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "pkg/sessionstore/testdata/script-contract.json")
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = CheckStoreGolden(root)
	if err == nil {
		t.Fatal("inventory drift passed")
	}
	for _, name := range []string{"pkg/arbitrary/run.go:EvalRO:1", "pkg/arbitrary/run.go:EvalRO:2", "old-a", "old-b", "pkg/sessionstore/testdata/script-contract.json"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("missing %s in %v", name, err)
		}
	}
}

func TestLuaUnresolvedProcessWithoutRedisImportRefuses(t *testing.T) {
	root := scriptTree(t, map[string]string{"pkg/arbitrary/run.go": `package arbitrary;func run(){c.Process(ctx,commandFromAnotherPackage())}`})
	if _, err := LuaScripts(root); err == nil || !strings.Contains(err.Error(), "unresolved Redis Process") {
		t.Fatalf("indirect client bypassed command resolution: %v", err)
	}
	root = scriptTree(t, map[string]string{"pkg/arbitrary/run.go": `package arbitrary;import "github.com/relaishq/relais/pkg/agent";type input struct{audio agent.Input};func run(in input){h.instance.Process(ctx,in.audio)}`})
	if _, err := LuaScripts(root); err != nil {
		t.Fatalf("non-Redis agent callback was classified as a command: %v", err)
	}
}

func TestLuaMutatedProcessCommandsRefuse(t *testing.T) {
	for _, body := range []string{
		`cmd:=redis.NewCmd(ctx,"EVAL","return 2",0);c.Process(ctx,cmd);cmd=redis.NewCmd(ctx,"PING")`,
		`cmd:=redis.NewCmd(ctx,"PING");cmd.Args()[0]="FCALL";c.Process(ctx,cmd)`,
	} {
		t.Run(body, func(t *testing.T) {
			root := scriptTree(t, map[string]string{"pkg/arbitrary/run.go": `package arbitrary;import "github.com/redis/go-redis/v9";func run(){` + body + `}`})
			if _, err := LuaScripts(root); err == nil || !strings.Contains(err.Error(), "unresolved Redis Process") {
				t.Fatalf("mutable command was treated as a literal: %v", err)
			}
		})
	}
}

func TestLuaUnresolvedCrossFileProcessCommandsRefuse(t *testing.T) {
	root := scriptTree(t, map[string]string{
		"pkg/arbitrary/shared.go": `package arbitrary;import "github.com/redis/go-redis/v9";var cmd=redis.NewCmd(ctx,"FCALL","function",0)`,
		"pkg/arbitrary/run.go":    `package arbitrary;import "github.com/redis/go-redis/v9";func run(){_=redis.NewCmd(ctx,"PING");c.Process(ctx,cmd)}`,
	})
	if _, err := LuaScripts(root); err == nil || !strings.Contains(err.Error(), "unresolved Redis Process") {
		t.Fatalf("unresolved global borrowed an unrelated initializer: %v", err)
	}
}
