package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/operator"
)

func TestTemplateShimScope(t *testing.T) {
	script, err := os.ReadFile("../../../internal/operatorrehearsal/assets/template-read.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, value := range map[string]string{
		"template-read.py":         string(script),
		"scope.json":               `{"stack":"fixture","region":"local","resource":"WorkerOne","metadata":["BootFiles","WorkerOne"],"secrets":["WorkerPassword"]}`,
		"metadata/SharedHost.json": "must-not-read",
		"secrets/ControlPassword":  "must-not-read",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := [][]string{
		{"cloudformation", "describe-stack-resource", "--stack-name", "fixture", "--logical-resource-id", "SharedHost", "--region", "local", "--query", "StackResourceDetail.Metadata", "--output", "text"},
		{"secretsmanager", "get-secret-value", "--secret-id", "ControlPassword", "--region", "local", "--query", "SecretString", "--output", "text"},
	}
	for _, args := range args {
		cmd := exec.Command("python3", append([]string{"-I", filepath.Join(dir, "template-read.py")}, args...)...)
		output, err := cmd.CombinedOutput()
		if err == nil || strings.Contains(string(output), "must-not-read") || string(output) != "local template read refused\n" {
			t.Fatal("MUTATION: template-shim-scope", err, string(output))
		}
	}
}

type scopeRunner struct{ calls int }

func (r *scopeRunner) Run(context.Context, operatorrehearsal.Command) operatorrehearsal.CommandResult {
	r.calls++
	return operatorrehearsal.CommandResult{Code: new(int)}
}
func TestAdapterCapturedHostScope(t *testing.T) {
	runner := &scopeRunner{}
	adapter := operatorrehearsal.Adapter{Topology: &operatorrehearsal.Topology{Hosts: map[string]operatorrehearsal.Host{"w1": {Slot: "w1"}}}, Containers: map[string]string{"foreign": strings.Repeat("a", 64)}, Runner: runner}
	req := operator.Request{ID: "1", Operation: operator.HostExecute, Target: "foreign", Action: "worker-status", Script: []byte("printf observation"), DeadlineUnixMS: strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10), RemainingMS: 60000}
	reply := adapter.Execute(context.Background(), req)
	if runner.calls != 0 || !reply.NotSubmitted || reply.RefusalCode != "invalid-target" {
		t.Fatal("MUTATION: captured-host-scope", reply, runner.calls)
	}
}
