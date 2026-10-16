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
// The generated main registers v2 logic using public SDK imports only.
func TestExternalModuleBuild(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	module := "module example.com/external-worker\n\ngo 1.26.0\n\nrequire github.com/relaishq/relais v0.0.0\n\nreplace github.com/relaishq/relais => " + strconv.Quote(root) + "\n"
	main := `package main
import (
 "context"
 "encoding/json"
 "os"
 agent "github.com/relaishq/relais/pkg/agent/v2"
 "github.com/relaishq/relais/pkg/workerapp"
)
type custom struct { count byte }
func (a *custom) Handle(_ context.Context,e agent.Event)(agent.Effects,error){
 switch e.Kind {case agent.AudioInput:a.count++;case agent.Started,agent.Resumed:
  json.NewEncoder(os.Stdout).Encode(map[string]any{"event":e.Kind,"count":a.count})
 }
 return agent.Effects{},nil
}
func(a *custom)Save(context.Context)(agent.State,error){return agent.State{Version:1,Bytes:[]byte{a.count}},nil}
func(a *custom)Restore(_ context.Context,s agent.State)error{if s.Version!=1{return agent.ErrVersion};a.count=s.Bytes[0];return nil}
func main(){
 name:=os.Getenv("EXTERNAL_AGENT_NAME");if name==""{name="external/v1"}
 workerapp.Main(workerapp.Options{Agents:[]workerapp.Registration{{Name:name,Factory:func(a agent.FactoryArgs)agent.Agent{
  json.NewEncoder(os.Stdout).Encode(map[string]any{"factory":a.AgentName,"budget":a.MaxStateBytes})
  return &custom{}
 }}}})
}
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
	run("go", "build", "-mod=mod", "-o", filepath.Join(dir, "stock-worker"), "github.com/relaishq/relais/cmd/relais-worker")
	if out := run(binary, "-h"); !strings.Contains(out, "-agent string") || !strings.Contains(out, `(default "echo")`) || !strings.Contains(out, "audio agent: echo, demo or a registered name") || strings.Contains(out, "agent-drain-task-budget") || strings.Contains(out, "agent-drain-move-reserve") {
		t.Fatalf("unexpected help output: %s", out)
	}
	t.Run("registered-call-move-takeover", func(t *testing.T) { testExternalRegisteredCall(t, binary) })

}
