package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
