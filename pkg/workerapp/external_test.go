package workerapp_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Build outside the Relais module so Go enforces the public import boundary.
// The generated main imports only workerapp and uses the stock entrypoint.
func TestExternalModuleBuild(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	module := "module example.com/external-worker\n\ngo 1.26.0\n\nrequire github.com/relaishq/relais v0.0.0\n\nreplace github.com/relaishq/relais => " + strconv.Quote(root) + "\n"
	main := `package main
import "github.com/relaishq/relais/pkg/workerapp"
func main() { workerapp.Main(workerapp.Options{}) }
`
	for name, data := range map[string]string{"go.mod": module, "main.go": main} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return string(out)
	}
	binary := filepath.Join(dir, "relais-worker")
	run("go", "build", "-mod=mod", "-o", binary, ".")
	if out := run(binary, "-version"); !strings.HasPrefix(out, "relais-worker version=") {
		t.Fatalf("unexpected version output: %s", out)
	}
	if out := run(binary, "-h"); !strings.Contains(out, "-agent string") || !strings.Contains(out, `(default "echo")`) {
		t.Fatalf("unexpected help output: %s", out)
	}
}
