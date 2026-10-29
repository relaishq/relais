package operatorrehearsal

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/operator"
)

type scenarioBootRunner struct {
	report  *Report
	atImage string
}

func (r *scenarioBootRunner) Run(_ context.Context, c Command) CommandResult {
	if c.Label == "image" {
		r.atImage = r.report.ScenarioSHA256
		return CommandResult{Code: code(1), Stderr: []byte("image fixture failed")}
	}
	return CommandResult{Code: code(0)}
}
func TestBootFailureRetainsScenarioIdentity(t *testing.T) {
	dir := t.TempDir()
	runner := &scenarioBootRunner{}
	report, err := rehearse(context.Background(), RehearseConfig{Commit: strings.Repeat("a", 40), Output: filepath.Join(dir, "report")}, io.Discard, func(r *rehearsal, _ context.Context) error {
		r.topology = fixtureTopology(t)
		data := []byte("scenario archive fixture")
		archive := filepath.Join(dir, "release.tar.gz")
		if e := os.WriteFile(archive, data, 0600); e != nil {
			return e
		}
		r.topology.Config.ArchiveSHA256 = hash(data)
		r.report.TemplateSHA256 = r.topology.TemplateSHA256
		r.report.CallerSHA256 = strings.Repeat("b", 64)
		r.judge = &recordingJudge{pinned: operatorevidence.PinnedJudge{Commit: ReleaseACommit, SHA256: strings.Repeat("c", 64)}}
		r.report.JudgeSHA256 = []string{r.judge.pinned.SHA256, r.judge.pinned.SHA256}
		runner.report = r.report
		r.runner = runner
		layout, e := Layout([]int{0, 1, 2, 3}, nil)
		if e != nil {
			return e
		}
		r.deployment, e = NewDeployment(r.topology, filepath.Join(r.work, "topology"), archive, layout, runner, r.diagnostics)
		return e
	})
	if err == nil || report.Verdict != operator.Failure || !fullDigest.MatchString(report.ScenarioSHA256) || runner.atImage != report.ScenarioSHA256 {
		t.Fatal("MUTATION: scenario-before-boot", report.ScenarioSHA256, runner.atImage, err)
	}
	raw, e := os.ReadFile(filepath.Join(dir, "report/report.json"))
	if e != nil {
		t.Fatal(e)
	}
	var saved Report
	if json.Unmarshal(raw, &saved) != nil || saved.ScenarioSHA256 != report.ScenarioSHA256 {
		t.Fatal("boot failure lost scenario identity")
	}
}
