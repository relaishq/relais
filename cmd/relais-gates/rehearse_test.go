package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/artifacts"
)

func TestRehearseRefusesIncompleteReadiness(t *testing.T) {
	dir := t.TempDir()
	var diagnostics bytes.Buffer
	checked := false
	status := rehearseWithReadiness([]string{"-prepared", "-assets", filepath.Join(dir, "assets"), "-output", filepath.Join(dir, "report"), "-go", filepath.Join(dir, "missing-go")}, func() error {
		checked = true
		return errors.New("operator-readiness: pending mutation fixture")
	}, &diagnostics)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if status != 1 || !checked || len(entries) != 0 || !strings.Contains(diagnostics.String(), "operator-readiness: pending mutation fixture") {
		t.Fatal("MUTATION: rehearsal-readiness-bypass", status, checked, len(entries), diagnostics.String())
	}
}

func TestRehearseReleaseReadFailurePrintsRefusalCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&artifacts.Refusal{Code: "read-rate-limited", Detail: "secret-token https://x.invalid"}, "release-refusal: read-rate-limited"},
		{&artifacts.Refusal{Code: "token-ghp_abc", Detail: "d"}, "release-refusal: unclassified"},
		{errors.New("get https://x.invalid?token=secret"), "release-refusal: unclassified"},
	}
	for _, c := range cases {
		old := prepareReleases
		prepareReleases = func(context.Context, string, string) error { return c.err }
		var diagnostics bytes.Buffer
		status := rehearseWithReadiness([]string{"-prepare-only"}, func() error { return nil }, &diagnostics)
		prepareReleases = old
		out := diagnostics.String()
		if status != 1 || !strings.Contains(out, "release-read-failed") || !strings.Contains(out, c.want) || strings.Contains(out, "secret") || strings.Contains(out, "invalid-scenario-identity") {
			t.Fatal(status, out)
		}
	}
}
