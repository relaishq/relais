package operatorevidence

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/pkg/operator"
)

func TestRelayLossScenarioBindingIsRSpecific(t *testing.T) {
	p := RelayLossJudge{pinned: PinnedJudge{Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64)}}
	for _, g := range []operator.Gate{operator.Rollout, operator.Failover, operator.RelayDeploy, operator.RelayHostLoss} {
		s, err := operator.BuildScenarioIdentity(operator.Scenario{SchemaVersion: 1, RelayTwo: true, Gates: []operator.Gate{g}, Caller: operator.Identity{SourceCommit: p.pinned.Commit}}, "")
		if err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(s)
		bound, err := p.BindScenario(s)
		if g != operator.RelayHostLoss {
			if err == nil {
				t.Fatal("accepted non-R scenario", g)
			}
		} else if err != nil || bound.Judge != p.Identity() || bound.Digest == s.Digest {
			t.Fatal(bound, err)
		}
		after, _ := json.Marshal(s)
		if string(before) != string(after) {
			t.Fatal("binding mutated input")
		}
	}
	s := operator.Scenario{RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, Caller: operator.Identity{SourceCommit: p.pinned.Commit}, ClockReceiptSHA256: strings.Repeat("f", 64)}
	if _, err := p.BindScenario(s); err == nil {
		t.Fatal("bound after clock receipt")
	}
}

func TestRelayLossPinnedBuildAndHash(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = root
	head, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	compiler, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "judge")
	p, err := BuildRelayLossPinned(ctx, root, strings.TrimSpace(string(head)), compiler, output, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// The R build must leave the archived default judge and decoder untouched.
	for _, path := range []string{"internal/operatorevidence/judge-src/main.go", "internal/operatorgates/observe/required.go"} {
		cmd := exec.Command("git", "show", p.pinned.Commit+":"+path)
		cmd.Dir = root
		want, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(output, "source", path))
		if err != nil || string(got) != string(want) {
			t.Fatal("changed archive boundary", path, err)
		}
	}
	scenario, err := p.BindScenario(operator.Scenario{SchemaVersion: 1, RelayTwo: true, Gates: []operator.Gate{operator.RelayHostLoss}, Caller: operator.Identity{SourceCommit: p.pinned.Commit}})
	if err != nil {
		t.Fatal(err)
	}
	in := relayjudge.Input{Version: 1, RunID: "R-build", Scenario: scenario, TargetInsufficient: true, Restore: "failed"}
	got, err := p.Judge(ctx, in)
	if err != nil || len(got.Calls) != 4 || got.Outcome != "fail" {
		t.Fatal(got, err)
	}
	in.Scenario.Judge.SHA256 = strings.Repeat("0", 64)
	if _, err = p.Judge(ctx, in); err == nil {
		t.Fatal("accepted different R binary identity")
	}
	in.Scenario = scenario
	f, err := os.OpenFile(p.pinned.Binary, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte("changed"))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
	if _, err = p.Judge(ctx, in); err == nil || err.Error() != "judge-binary-differs" {
		t.Fatal("accepted tampered binary", err)
	}
}
