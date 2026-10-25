package releasecompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManifest(t *testing.T, version string) Manifest {
	t.Helper()
	m, err := Generate("../..", version, strings.Repeat("a", 40), "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	m.OldestLivePeer = "0.3.0"
	m.OldestRollback = "0.3.0"
	m.TestedPeers = []string{"0.3.0", "0.3.1", "0.4.0", version}
	if version != "0.3.0" {
		m.Commit = strings.Repeat("b", 40)
	}
	return m
}
func testPool(m Manifest) Pool {
	p := Pool{Schema: 1, Complete: true, TargetWorkerProfile: "legacy"}
	for _, r := range []string{"control", "worker", "relay", "worker", "signal", "relay"} {
		profile := "default"
		var agents []string
		if r == "worker" {
			profile = "legacy"
			agents = []string{"echo"}
		}
		p.Participants = append(p.Participants, Participant{Name: r + string(rune('a'+len(p.Participants))), Role: r, Profile: profile, Version: m.Version, Commit: m.Commit, ArchiveSHA256: strings.Repeat("c", 64), Agents: agents, DefaultBudgets: true, Manifest: m})
	}
	return p
}
func requireRefused(t *testing.T, v Verdict, code string) {
	t.Helper()
	if v.Allowed || v.Refusal == nil || v.Refusal.Code != code || len(v.Steps) != 0 {
		t.Fatalf("expected definite %s refusal with no executable steps, got %+v", code, v)
	}
}

func TestUpgradeAndRollbackPlanEveryParticipant(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	for _, tc := range []struct {
		operation string
		from, to  Manifest
	}{{"upgrade", old, next}, {"rollback", next, old}} {
		t.Run(tc.operation, func(t *testing.T) {
			v := Preflight(testPool(tc.from), tc.to, tc.operation)
			if !v.Allowed || len(v.Steps) != 6 {
				t.Fatalf("%+v", v)
			}
			roles := []string{"worker", "worker", "relay", "relay", "control", "signal"}
			if tc.operation == "rollback" {
				roles = []string{"signal", "control", "relay", "relay", "worker", "worker"}
			}
			for i, s := range v.Steps {
				if s.Role != roles[i] {
					t.Fatalf("unsafe rollout order: %+v", v.Steps)
				}
			}
		})
	}
}
func TestRefusesUnsupportedUpgradeBeforeAnyStep(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	next.TestedPeers = []string{next.Version}
	requireRefused(t, Preflight(testPool(old), next, "upgrade"), "unsupported_pair")
}
func TestRollbackRefusesPersistedV10EvenWithAdmissionOff(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	for _, role := range []string{"worker", "control"} {
		for profile, p := range old.Roles[role] {
			f := p.Formats["snapshot"]
			f.Reads = []int{6, 7, 8, 9}
			f.Writes = []int{7, 8, 9}
			if role == "control" {
				f.Writes = nil
			}
			f.Continues = []int{7, 8, 9}
			p.Formats["snapshot"] = f
			old.Roles[role][profile] = p
		}
	}
	// A real full bridge may still write adopted v10 even with legacy admission.
	pool := testPool(next)
	pool.TargetWorkerProfile = "legacy"
	requireRefused(t, Preflight(pool, old, "rollback"), "format_incompatible")
}
func TestRefusesReaderOnlyBridge(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	p := old.Roles["worker"]["legacy"]
	f := p.Formats["snapshot"]
	f.Continues = []int{7, 8, 9}
	p.Formats["snapshot"] = f
	old.Roles["worker"]["legacy"] = p
	requireRefused(t, Preflight(testPool(next), old, "rollback"), "format_incompatible")
}
func TestEveryIntermediateStateMustReadTargetWireProtocol(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	next.HTTPContract++
	for role, profiles := range next.Roles {
		for name, p := range profiles {
			p.Formats["private_http"] = Format{Reads: []int{next.HTTPContract}, Writes: []int{next.HTTPContract}, Continues: []int{next.HTTPContract}}
			next.Roles[role][name] = p
		}
	}
	v := Preflight(testPool(old), next, "upgrade")
	requireRefused(t, v, "format_incompatible")
	if v.Refusal.Step != 1 {
		t.Fatal("must refuse at first worker, before all services reach target")
	}
}
func TestCompatibleScriptSHAOptimizationDoesNotBlockRollout(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	s := next.Scripts["componentCommitLua"]
	s.SHA = strings.Repeat("d", 40)
	s.UnchangedTuples = "ADR 0012 #238: persisted tuples unchanged"
	next.Scripts["componentCommitLua"] = s
	if err := CheckEvolution(old, next); err != nil {
		t.Fatal(err)
	}
	if v := Preflight(testPool(old), next, "upgrade"); !v.Allowed {
		t.Fatal(v.Refusal)
	}
}
func TestAdmissionActivationNeedsAllReadersAndLifecycle(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	pool := testPool(old)
	pool.TargetWorkerProfile = "split"
	if v := Preflight(pool, next, "upgrade"); !v.Allowed {
		t.Fatal(v.Refusal)
	}
	for _, p := range old.Roles["control"] {
		f := p.Formats["metadata"]
		f.Continues = nil
		p.Formats["metadata"] = f
	}
	requireRefused(t, Preflight(pool, next, "upgrade"), "format_incompatible")
}
func TestUnknownInventoryAndManifestRefuse(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	cases := []struct {
		name, code string
		change     func(*Pool, *Manifest)
	}{
		{"incomplete", "unknown_inventory", func(p *Pool, _ *Manifest) { p.Complete = false }},
		{"pre-manifest", "missing_manifest", func(_ *Pool, m *Manifest) { m.Schema = 0 }},
		{"identity", "identity_mismatch", func(p *Pool, _ *Manifest) { p.Participants[0].Commit = "unknown" }},
		{"unknown-profile", "unknown_profile", func(p *Pool, _ *Manifest) { p.TargetWorkerProfile = "unknown" }},
		{"custom-agent", "unknown_agent", func(p *Pool, _ *Manifest) { p.Participants[1].Agents = []string{"custom"} }},
		{"custom-budgets", "unknown_configuration", func(p *Pool, _ *Manifest) { p.Participants[1].DefaultBudgets = false }},
		{"rollback-floor", "rollback_boundary", func(_ *Pool, m *Manifest) { m.OldestRollback = m.Version }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testPool(old)
			target := testManifest(t, "0.3.1")
			if c.name == "rollback-floor" {
				pool = testPool(next)
				target = testManifest(t, "0.3.0")
				for i := range pool.Participants {
					pool.Participants[i].Manifest.OldestRollback = next.Version
				}
				requireRefused(t, Preflight(pool, target, "rollback"), c.code)
				return
			}
			c.change(&pool, &target)
			requireRefused(t, Preflight(pool, target, "upgrade"), c.code)
		})
	}
}
func TestContractEvolutionRequiresVersionOrUnchangedTupleNote(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	next.HTTPGolden = strings.Repeat("e", 64)
	if err := CheckEvolution(old, next); err == nil {
		t.Fatal("HTTP fixture changed without version bump")
	}
	next.HTTPContract++
	if err := CheckEvolution(old, next); err != nil {
		t.Fatal(err)
	}
	next = testManifest(t, "0.3.1")
	s := next.Scripts["sessionLua"]
	s.SHA = strings.Repeat("f", 40)
	next.Scripts["sessionLua"] = s
	if err := CheckEvolution(old, next); err == nil {
		t.Fatal("Lua changed without note or bump")
	}
	next.StoreContract++
	if err := CheckEvolution(old, next); err != nil {
		t.Fatal(err)
	}
}
func TestGeneratedManifestMatchesCheckedInContract(t *testing.T) {
	m, err := Generate("../..", "0.0.0-dev", "", "any", "any")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := JSON(m)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("../../scripts/release/compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatal("format version/golden drift: regenerate scripts/release/compatibility.json")
	}
}
func TestDecodeRejectsDuplicateUnknownAndTrailingFields(t *testing.T) {
	for _, input := range []string{`{"manifest_version":1,"manifest_version":1}`, `{"unknown":1}`, `{} {}`} {
		var m Manifest
		if Decode([]byte(input), &m) == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
func TestSemanticVersionOrdering(t *testing.T) {
	for _, p := range [][2]string{{"0.3.9", "0.3.10"}, {"0.4.0-2", "0.4.0-10"}, {"0.4.0-rc.1", "0.4.0"}} {
		c, err := Compare(p[0], p[1])
		if err != nil || c >= 0 {
			t.Fatalf("%v: %d %v", p, c, err)
		}
	}
	if _, err := Compare("unknown", "0.3.0"); err == nil {
		t.Fatal("accepted unknown version")
	}
}
func TestStoreGoldenRejectsChangedLua(t *testing.T) {
	root := t.TempDir()
	paths, err := filepath.Glob("../../pkg/sessionstore/redis*.go")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pkg/sessionstore/testdata")
	if err = os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "pkg/sessionstore", filepath.Base(path)), source, 0644); err != nil {
			t.Fatal(err)
		}
	}

	for _, rel := range []string{"pkg/framecache/redis.go", "cmd/relais-load/soak.go"} {
		data, e := os.ReadFile(filepath.Join("../..", rel))
		if e != nil {
			t.Fatal(e)
		}
		dest := filepath.Join(root, rel)
		if e = os.MkdirAll(filepath.Dir(dest), 0755); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(dest, data, 0644); e != nil {
			t.Fatal(e)
		}
	}
	component := filepath.Join(root, "cmd/relais-worker")
	if err = os.MkdirAll(component, 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(component, "main.go"), []byte(`package main;import(_ "github.com/relaishq/relais/pkg/sessionstore";_ "github.com/relaishq/relais/pkg/framecache")`), 0644); err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("../../pkg/sessionstore/testdata/script-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "script-contract.json"), golden, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = CheckStoreGolden(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "pkg/sessionstore/redis.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(source), "return redis.call('TIME')", "return redis.call('TIME') -- changed", 1)
	if changed == string(source) {
		t.Fatal("mutation missed production Lua")
	}
	if err = os.WriteFile(path, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = CheckStoreGolden(root); err == nil || !strings.Contains(err.Error(), "lua SHA drift for storeClockLua") {
		t.Fatalf("expected actual SHA mutation rejection, got %v", err)
	}
}

func TestDevelopmentArtifactsCannotAuthorizeLiveRollout(t *testing.T) {
	old := testManifest(t, "0.3.0")
	dev, err := Generate("../..", "0.0.0-dev+abc123", strings.Repeat("a", 40), "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, Preflight(testPool(old), dev, "upgrade"), "unknown_build")
	requireRefused(t, Preflight(testPool(dev), old, "upgrade"), "unknown_build")
}

func TestOptionalBridgeDomainSupportsPriorManifests(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	for _, profiles := range old.Roles {
		for _, p := range profiles {
			delete(p.Formats, "bridge_output")
		}
	}
	delete(old.Roles["worker"], "bridge")
	if err := old.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		op       string
		from, to Manifest
	}{{"upgrade", old, next}, {"rollback", next, old}} {
		pool := testPool(tc.from)
		pool.TargetWorkerProfile = "split"
		for i := range pool.Participants {
			if pool.Participants[i].Role == "worker" {
				pool.Participants[i].Profile = "split"
			}
		}
		if v := Preflight(pool, tc.to, tc.op); !v.Allowed {
			t.Fatalf("%s: %+v", tc.op, v.Refusal)
		}
	}
}

// Bridge capabilities can be decoded before a bridge agent is shipped. Tests
// construct such a release explicitly; Generate must describe real writers.
func enableBridgeManifest(m *Manifest) {
	m.Agents = append(m.Agents, Agent{Name: "bridge", Identity: "bridge", Limits: "bridge", Reads: []uint32{1}, Writes: []uint32{1}})
	for _, role := range []string{"worker", "control"} {
		for _, p := range m.Roles[role] {
			p.Formats["bridge_output"] = Format{Reads: []int{1}, Continues: []int{1}}
		}
	}
	split := m.Roles["worker"]["split"]
	formats := make(map[string]Format)
	for name, f := range split.Formats {
		formats[name] = f
	}
	formats["bridge_output"] = Format{Reads: []int{1}, Writes: []int{1}, Continues: []int{1}}
	m.Roles["worker"]["bridge"] = Profile{Formats: formats}
}
func TestBridgeManifestDecoderAcceptsRegisteredCapability(t *testing.T) {
	m := testManifest(t, "0.3.2")
	enableBridgeManifest(&m)
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := JSON(m)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	if err := Decode(data, &decoded); err != nil {
		t.Fatal(err)
	}
}
func TestMissingBridgeReaderRefusesBridgePool(t *testing.T) {
	old, next := testManifest(t, "0.3.0"), testManifest(t, "0.3.1")
	for _, profiles := range old.Roles {
		for _, p := range profiles {
			delete(p.Formats, "bridge_output")
		}
	}
	// A bridge-enabled worker's retained format must never be silently ignored
	// just because an older control or worker manifest omits the domain.
	enableBridgeManifest(&next)
	pool := testPool(next)
	for i := range pool.Participants {
		if pool.Participants[i].Role == "worker" {
			pool.Participants[i].Profile = "bridge"
			pool.Participants[i].Agents = []string{"bridge"}
		}
	}
	pool.TargetWorkerProfile = "bridge"
	requireRefused(t, Preflight(pool, old, "rollback"), "format_incompatible")
	// Exercise the format gate without an app-contract mismatch.
	old.Agents = next.Agents
	p := old.Roles["worker"]["split"]
	old.Roles["worker"]["bridge"] = p
	requireRefused(t, Preflight(pool, old, "rollback"), "format_incompatible")
}

func TestBridgeAgentRequiresEnabledProfile(t *testing.T) {
	m := testManifest(t, "0.3.1")
	m.Agents = append(m.Agents, Agent{Name: "bridge", Identity: "bridge", Limits: "bridge", Reads: []uint32{1}, Writes: []uint32{1}})
	pool := testPool(m)
	pool.TargetWorkerProfile = "split"
	for i := range pool.Participants {
		if pool.Participants[i].Role == "worker" {
			pool.Participants[i].Agents = []string{"bridge"}
		}
	}
	requireRefused(t, Preflight(pool, m, "upgrade"), "unknown_profile")
}

// v0.3.0 validates an exact format-domain count in every profile. Until an
// actual bridge registration ships, preserve that archive's emitted domains.
func TestUnregisteredBridgePreservesPublishedManifestDomains(t *testing.T) {
	m := testManifest(t, "0.3.2")
	for _, a := range m.Agents {
		if a.Name == "bridge" {
			t.Fatal("fixture assumes no registered bridge agent")
		}
	}
	if _, ok := m.Roles["worker"]["bridge"]; ok {
		t.Error("unregistered bridge profile emitted")
	}
	expected := map[string][]string{
		"signal":  {"private_http"},
		"relay":   {"private_http", "redis_keys", "store_contract", "relay_udp"},
		"control": {"private_http", "redis_keys", "store_contract", "snapshot", "component", "metadata", "encryption_header", "legacy_encryption_header"},
		"worker":  {"private_http", "redis_keys", "store_contract", "relay_udp", "snapshot", "component", "metadata", "encryption_header", "legacy_encryption_header"},
	}
	for role, profiles := range m.Roles {
		for name, p := range profiles {
			if len(p.Formats) != len(expected[role]) {
				t.Errorf("%s/%s: v0.3.0 rejects %d domains, wants %d", role, name, len(p.Formats), len(expected[role]))
			}
			for _, domain := range expected[role] {
				if _, ok := p.Formats[domain]; !ok {
					t.Errorf("%s/%s: missing %s", role, name, domain)
				}
			}
		}
	}
}
