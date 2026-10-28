package operatorevidence

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedBuildRejectsAmbientConfiguration(t *testing.T) {
	for _, key := range []string{"GOWORK", "GOENV", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOFLAGS", "GOROOT", "GOTOOLDIR", "GOEXPERIMENT", "AZURE_CLIENT_SECRET", "OPENAI_API_KEY"} {
		t.Setenv(key, "hostile-value")
	}
	for _, key := range []string{"GOPRIVATE", "GONOPROXY", "GONOSUMDB"} {
		t.Setenv(key, "*")
	}
	config := filepath.Join(t.TempDir(), "go-config")
	if err := os.WriteFile(config, []byte("GOEXPERIMENT=hostile-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", config)
	env := buildEnvironment()
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for key, want := range map[string]string{"GOWORK": "off", "GOENV": "off", "GOPROXY": "off", "GOSUMDB": "off", "GOPRIVATE": "", "GONOPROXY": "none", "GONOSUMDB": "none", "GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly -buildvcs=false"} {
		if values[key] != want {
			t.Fatal("MUTATION: isolated-build-environment", key, values[key])
		}
	}
	for _, key := range []string{"GOROOT", "GOTOOLDIR", "GOEXPERIMENT", "AZURE_CLIENT_SECRET", "OPENAI_API_KEY"} {
		if _, exists := values[key]; exists {
			t.Fatal("MUTATION: isolated-build-environment", key)
		}
	}
	compiler, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(compiler, "env", "-json", "GOVERSION", "GOWORK", "GOENV", "GOPROXY", "GONOPROXY")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]string
	if json.Unmarshal(out, &actual) != nil {
		t.Fatal(string(out))
	}
	for key, want := range map[string]string{"GOVERSION": pinnedGoVersion, "GOWORK": "off", "GOENV": "", "GOPROXY": "off", "GONOPROXY": "none"} {
		if actual[key] != want {
			t.Fatal("MUTATION: isolated-build-environment", key, actual)
		}
	}
	if err := verifyCompiler(context.Background(), compiler); err != nil {
		t.Fatal(err)
	}
}
func TestPinnedBuildChecksCompilerBeforeArchive(t *testing.T) {
	compiler := filepath.Join(t.TempDir(), "compiler")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\nprintf 'go1.25.0\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyCompiler(context.Background(), compiler); err == nil {
		t.Fatal("MUTATION: pinned-compiler-version")
	}
	output := filepath.Join(t.TempDir(), "judge")
	if _, err := BuildPinned(context.Background(), "unopened", strings.Repeat("a", 40), compiler, output); err == nil || err.Error() != "judge-compiler-version" {
		t.Fatal("MUTATION: pinned-compiler-version", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("compiler refusal created build output", err)
	}
	module, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(module), "toolchain "+pinnedGoVersion+"\n") {
		t.Fatal("compiler pin differs from module")
	}
}
