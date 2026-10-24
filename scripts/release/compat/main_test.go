package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/releasecompat"
)

func copyContractRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	paths := []string{"scripts/release/compatibility-policy.json", "scripts/release/compatibility.json", "pkg/sessionstore/testdata/script-contract.json", "internal/privateapi/testdata/contract-v2.json", "pkg/mediaworker/state.go", "pkg/mediaworker/agent_v2.go", "pkg/mediaworker/component_snapshot.go", "pkg/mediaworker/handover.go", "internal/componentcodec/binary.go", "internal/componentcodec/auth.go", "pkg/sessionstore/components.go", "pkg/agent/agent.go", "pkg/framecache/redis.go", "cmd/relais-load/soak.go", "pkg/sessionstore/testdata/key-contract-v1.json", "pkg/framecache/testdata/key-contract-v1.json", "internal/relayleg/testdata/contract-v1.json"}
	scripts, err := filepath.Glob("../../../pkg/sessionstore/redis*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range scripts {
		if !strings.HasSuffix(p, "_test.go") {
			rel, err := filepath.Rel("../../..", p)
			if err != nil {
				t.Fatal(err)
			}
			paths = append(paths, rel)
		}
	}
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join("../../..", p))
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(root, p)
		if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(dest, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	component := filepath.Join(root, "cmd/relais-worker")
	if err := os.MkdirAll(component, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(component, "main.go"), []byte(`package main;import(_ "github.com/relaishq/relais/pkg/sessionstore";_ "github.com/relaishq/relais/pkg/framecache")`), 0644); err != nil {
		t.Fatal(err)
	}
	return root
}
func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
func TestCheckRejectsFormatMutationWithoutManifestUpdate(t *testing.T) {
	for _, mutation := range []struct{ name, path, from, to string }{
		{"snapshot", "pkg/mediaworker/state.go", "const sessionStateVersion = 7", "const sessionStateVersion = 11"},
		{"v2", "pkg/mediaworker/agent_v2.go", "v2SessionStateVersion      = 8", "v2SessionStateVersion      = 11"},
		{"control", "pkg/mediaworker/agent_v2.go", "controlSessionStateVersion = 9", "controlSessionStateVersion = 11"},
		{"split", "pkg/mediaworker/component_snapshot.go", "Version: 10", "Version: 11"},
		{"component", "internal/componentcodec/binary.go", "[]byte{10, kind}", "[]byte{11, kind}"},
		{"metadata", "internal/componentcodec/auth.go", "m.Format != 10", "m.Format != 11"},
		{"encryption", "internal/componentcodec/auth.go", "header := []byte{1, keyID}", "header := []byte{2, keyID}"},
		{"legacy-encryption", "pkg/sessionstore/redis.go", "header := []byte{1, r.activeKey}", "header := []byte{2, r.activeKey}"},
		{"component-reader", "internal/componentcodec/binary.go", "data[0] != 10", "data[0] != 11"},
		{"metadata-reader", "pkg/sessionstore/components.go", "m.Format != 10", "m.Format != 11"},
		{"encryption-reader", "internal/componentcodec/auth.go", "sealed[0] != 1", "sealed[0] != 2"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			root := copyContractRoot(t)
			var out bytes.Buffer
			if code := run([]string{"check", "--root", root}, &out); code != 0 {
				t.Fatal(out.String())
			}
			path := filepath.Join(root, mutation.path)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), mutation.from, mutation.to, 1)
			if changed == string(data) {
				t.Fatal("mutation did not hit the production format")
			}
			if err = os.WriteFile(path, []byte(changed), 0644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if run([]string{"check", "--root", root}, &out) == 0 {
				t.Fatalf("format drift did not fail manifest gate: %s", out.String())
			}
		})
	}
}

func TestPreflightRefusalHasNoFileOrProcessChanges(t *testing.T) {
	root := t.TempDir()
	m, err := releasecompat.Generate("../../..", "0.3.0", strings.Repeat("a", 40), "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	// A hostile binary-directory marker must never be run by the offline path.
	marker := filepath.Join(root, "executed")
	program := filepath.Join(root, "relais-worker")
	if err = os.WriteFile(program, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	pool := filepath.Join(root, "pool.json")
	target := filepath.Join(root, "target.json")
	writeJSON(t, pool, releasecompat.Pool{Schema: 1, Complete: false})
	writeJSON(t, target, m)
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := run([]string{"preflight", "--running", pool, "--target", target, "--operation", "rollback"}, &out)
	if code == 0 {
		t.Fatal("reported refusal as success")
	}
	var verdict releasecompat.Verdict
	if err = releasecompat.Decode(out.Bytes(), &verdict); err != nil {
		t.Fatal(err)
	}
	if verdict.Allowed || verdict.Refusal == nil || verdict.Refusal.Code != "unknown_inventory" {
		t.Fatal(out.String())
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("refusal changed files")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("refusal ran a process")
	}
}
func TestMissingAndPreManifestFilesRefuseClearly(t *testing.T) {
	for _, data := range []string{`{}`, `{"version":"0.2.0"}`} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			pool, target := filepath.Join(root, "pool.json"), filepath.Join(root, "target.json")
			writeJSON(t, pool, releasecompat.Pool{})
			if err := os.WriteFile(target, []byte(data), 0644); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if run([]string{"preflight", "--running", pool, "--target", target, "--operation", "upgrade"}, &out) == 0 || !strings.Contains(out.String(), "missing_manifest") {
				t.Fatal(out.String())
			}
		})
	}
}
func TestArtifactIdentityMismatchPrecedesBinaryExecution(t *testing.T) {
	root := t.TempDir()
	m, err := releasecompat.Generate("../../..", "0.3.0", strings.Repeat("a", 40), "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "compatibility.json")
	writeJSON(t, manifest, m)
	var out bytes.Buffer
	if run([]string{"artifact", "--manifest", manifest, "--version", "0.3.1", "--os", "linux", "--arch", "arm64", "--binary-dir", root}, &out) == 0 || !strings.Contains(out.String(), "identity_mismatch") {
		t.Fatal(out.String())
	}
}

func TestRealPeerPolicyKeepsDevelopmentManifestGeneratable(t *testing.T) {
	root := copyContractRoot(t)
	policy := releasecompat.Policy{OldestLivePeer: "0.3.0", OldestRollback: "0.3.0", TestedPeers: []string{"self", "0.3.0"}}
	writeJSON(t, filepath.Join(root, "scripts/release/compatibility-policy.json"), policy)
	for _, version := range []string{"0.0.0-dev", "0.0.0-dev+abc123", "0.3.1"} {
		var out bytes.Buffer
		if code := run([]string{"generate", "--root", root, "--version", version}, &out); code != 0 {
			t.Fatalf("%s: %s", version, out.String())
		}
	}
}

func TestBinaryIdentityTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-binary")
	// exec leaves no child holding the inherited stdout pipe.
	if err := os.WriteFile(path, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := binaryIdentity(path, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded identity refusal, got %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("identity timeout was not bounded")
	}
}

func TestCompatibilityEvolutionIsRequiredCIGate(t *testing.T) {
	data, err := os.ReadFile("../../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	check := func(source string) bool {
		required := []string{"needs: [quality, unit-tests, timing-smoke, call-harness, compatibility]", `test "$COMPATIBILITY_RESULT" = success`, "COMPATIBILITY_RESULT: ${{ needs.compatibility.result }}", "CONTRACT_BASE: ${{ github.event.pull_request.base.sha }}", "git rev-parse HEAD^", `go run ./scripts/release/compat check --base "$base"`}
		for _, s := range required {
			if !strings.Contains(source, s) {
				return false
			}
		}
		job := strings.SplitN(strings.SplitN(source, "  compatibility:\n", 2)[1], "  # The build job", 2)[0]
		return !strings.Contains(job, "    if:") && !strings.Contains(strings.SplitN(source, "jobs:", 2)[0], "paths:")
	}
	if !check(source) {
		t.Fatal("contract evolution is not an unfiltered required CI gate")
	}
	for _, from := range []string{", compatibility]", `test "$COMPATIBILITY_RESULT" = success`, `check --base "$base"`} {
		if check(strings.Replace(source, from, "", 1)) {
			t.Fatalf("known-bad CI bypass was accepted: %s", from)
		}
	}
}
