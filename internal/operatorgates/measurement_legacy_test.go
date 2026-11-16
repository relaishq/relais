package operatorgates

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run the six original success/refusal/error characterizations against the
// actual pre-accounting implementation and diagnostics, offline. The fixture
// exposes its two inlined conversions without changing their behavior.
func TestMeasurementAccountingLegacyCharacterization(t *testing.T) {
	if _, err := os.Stat("../../.git"); os.IsNotExist(err) {
		t.Skip("legacy characterization requires local Git history; isolated mutation copies have none")
	} else if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOROOT":
			env = append(env, entry)
		}
	}
	env = append(env, "GOTOOLCHAIN=local", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOMAXPROCS=2", "GOFLAGS=-mod=readonly -buildvcs=false -p=1")
	archive := exec.CommandContext(ctx, "git", "archive", "--format=tar", "51759133a89f18ebce7a5333fdda0c20bbaaff7f", "go.mod", "go.sum", "internal", "pkg")
	archive.Dir = "../.."
	archive.Env = env
	raw, err := archive.Output()
	if err != nil {
		t.Fatalf("local legacy archive: %v", err)
	}
	root := t.TempDir()
	reader := tar.NewReader(bytes.NewReader(raw))
	for {
		entry, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsLocal(entry.Name) {
			t.Fatal("non-local legacy archive path")
		}
		skip := false
		for _, part := range strings.Split(entry.Name, "/") {
			if strings.HasPrefix(part, ".env") {
				skip = true
			}
		}
		if skip {
			continue
		}
		dest := filepath.Join(root, entry.Name)
		if entry.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dest, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if entry.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Verify that the fixture exposes the actual baseline conversion literals.
	for file, literal := range map[string]string{
		"resource_sequence.go":  `ProcedureResult{Status: m.Status, Reason: m.Reason, Media: m.Media, Process: ProcessEvidence{ExpiredWait: m.ExpiredWait}}`,
		"resource_admission.go": `ProcedureResult{Status: admitted.Status, Reason: admitted.Reason, Media: admitted.Survivor.Media}`,
	} {
		data, err := os.ReadFile(filepath.Join(root, "internal/operatorgates", file))
		if err != nil || !strings.Contains(string(data), literal) {
			t.Fatal("legacy conversion fixture differs from production", file, err)
		}
	}
	fixture, err := os.ReadFile("testdata/measurement-accounting-v1_test.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal/operatorgates/measurement_legacy_snapshot_test.go"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	run := exec.CommandContext(ctx, "go", "test", "-count=1", "-v", "-run", "^TestLegacyMeasurementProcedureAccounting$", "./internal/operatorgates")
	run.Dir = root
	run.Env = env
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("legacy characterization: %v\n%s", err, output)
	}
	if strings.Count(string(output), "--- PASS: TestLegacyMeasurementProcedureAccounting/") != 6 {
		t.Fatalf("legacy characterization did not run all six paths:\n%s", output)
	}
	t.Log(string(output))
}
