package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SDK implementations have internal dependencies. The APP may import only
// public packages. Copy its production sources to a genuinely external module
// as a second check that Go's internal-package boundary remains enforced.
func TestPublicImportsAndExternalBuild(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "list", "-json", "./...")
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	out, err := command.Output()
	require.NoError(t, err)
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var pkg struct {
			ImportPath                         string
			Imports, TestImports, XTestImports []string
		}
		err := decoder.Decode(&pkg)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		for _, imports := range [][]string{pkg.Imports, pkg.TestImports, pkg.XTestImports} {
			for _, path := range imports {
				require.NotContains(t, path, "github.com/relaishq/relais/internal/")
			}
		}
	}
	dir := t.TempDir()
	err = filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, path), 0700)
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		data = []byte(strings.ReplaceAll(string(data), "github.com/relaishq/relais/examples/voice-agent/", "example.com/voice/"))
		return os.WriteFile(filepath.Join(dir, path), data, 0600)
	})
	require.NoError(t, err)
	module := "module example.com/voice\n\ngo 1.26.0\n\nrequire github.com/relaishq/relais v0.0.0\n\nreplace github.com/relaishq/relais => " + strconv.Quote(root) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0600))
	binary := filepath.Join(dir, "voice-agent")
	command = exec.CommandContext(ctx, "go", "build", "-mod=mod", "-o", binary, ".")
	command.Dir = dir
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	out, err = command.CombinedOutput()
	require.NoError(t, err, "%s", out)
	command = exec.CommandContext(ctx, binary, "-h")
	out, err = command.CombinedOutput()
	require.NoError(t, err, "%s", out)
	for _, flag := range []string{"(default \"voice-agent/v1\")", "-agent-max-snapshot-bytes", "-agent-snapshot-bytes-per-second", "-agent-snapshot-burst-bytes", "-agent-move-bytes-per-session", "-agent-move-bytes-total", "-drain-timeout"} {
		require.Contains(t, string(out), flag)
	}
}
