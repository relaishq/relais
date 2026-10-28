package operatorrehearsal

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowUsesPublicRunnerAndNarrowArtifacts(t *testing.T) {
	raw, e := os.ReadFile("../../.github/workflows/operator-rehearsal.yml")
	if e != nil {
		t.Fatal(e)
	}
	var w struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			RunsOn string `yaml:"runs-on"`
			Steps  []struct {
				Name, Run, Uses string
				Env             map[string]string
				With            map[string]any
			}
		}
	}
	if e = yaml.Unmarshal(raw, &w); e != nil {
		t.Fatal(e)
	}
	if len(w.Permissions) != 1 || w.Permissions["contents"] != "read" {
		t.Fatal(w.Permissions)
	}
	for _, trigger := range []string{"pull_request", "push", "schedule", "workflow_dispatch"} {
		if _, ok := w.On[trigger]; !ok {
			t.Fatal("missing trigger", trigger)
		}
	}
	schedule := w.On["schedule"].([]any)[0].(map[string]any)["cron"].(string)
	if strings.Fields(schedule)[1] == "8" {
		t.Fatal("nightly clashes with existing CI")
	}
	job := w.Jobs["rehearsal"]
	if job.RunsOn != "ubuntu-latest" {
		t.Fatal(job.RunsOn)
	}
	tokens, uploads, execute := 0, 0, false
	for _, step := range job.Steps {
		for key, value := range step.Env {
			if strings.Contains(value, "github.token") {
				tokens++
				if step.Name != "Read and verify published releases" || key != "RELAIS_RELEASE_TOKEN" || !strings.Contains(step.Run, "-prepare-only") {
					t.Fatal("token outside release-read stage", step.Name)
				}
			}
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			uploads++
			paths := strings.Fields(step.With["path"].(string))
			if len(paths) != 2 || paths[0] != "runtime/operator-report/report.json" || paths[1] != "runtime/operator-report/host-diagnostics.log" {
				t.Fatal(paths)
			}
		}
		execute = execute || strings.Contains(step.Run, "make operator-rehearsal OPERATOR_REHEARSAL_RUNNER=") && strings.Contains(step.Run, "-prepared")
	}
	if tokens != 1 || uploads != 1 || !execute || strings.Contains(string(raw), "secrets.") {
		t.Fatal(tokens, uploads, execute)
	}
	paths := w.On["pull_request"].(map[string]any)["paths"].([]any)
	for _, expected := range []string{"internal/operator*/**", "deploy/aws/**", "cmd/relais-gates/**", "pkg/operator/**", "scripts/ci/gate-mutation/**", ".github/workflows/operator-rehearsal.yml"} {
		found := false
		for _, p := range paths {
			found = found || p == expected
		}
		if !found {
			t.Fatal("missing PR path", expected)
		}
	}
}
