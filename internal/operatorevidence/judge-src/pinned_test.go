package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
)

func TestPinnedReleaseJudgesMatchFrozenEvidence(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	relay := relayInput()
	mixed := rawPoolSample()
	relay.Sessions = []string{"relay-session"}
	relay.CallerJSONL = strings.ReplaceAll(relay.CallerJSONL, `"session":"session"`, `"session":"relay-session"`)
	mixed.Phases = append(mixed.Phases, relay)
	cases := []previewPoolInput{rawPoolSample(), {RunID: "run-fixture", Missing: true}, {RunID: relay.RunID, Phases: []previewMediaInput{relay}}, mixed}
	lost := rawPoolSample()
	lost.Missing = true
	lost.Phases[0].Receipts[0].Result.Lost = true
	cases = append(cases, lost)
	commits := []string{"a0d9744ff1223a8d7978447c907041973e915b08", "85e10436eb6adbbf577b6538b0bbb799aab823b2"}
	for _, commit := range commits {
		if err := pinnedCommit(ctx, root, commit); err != nil {
			t.Fatal(err)
		}
	}
	// These fixed releases differ only in release metadata, not Go sources,
	// module locks or embedded media. The shared glue and decoder are identical.
	diff := exec.CommandContext(ctx, "git", "--no-optional-locks", "diff", "--name-only", commits[0], commits[1])
	diff.Dir, diff.Env = root, operatorcaller.PublicEnvironment(nil)
	changed, err := diff.Output()
	if err != nil || string(changed) != "scripts/release/compatibility-policy.json\nscripts/release/compatibility.json\n" {
		t.Fatalf("fixed release judge input trees differ: %s (%v)", changed, err)
	}
	var executableSHA string
	for _, commit := range commits {
		t.Run(commit, func(t *testing.T) {
			judge, err := operatorevidence.BuildPinned(ctx, root, commit, goExecutable, filepath.Join(t.TempDir(), "judge"))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("pin %s executable SHA256 %s", commit, judge.SHA256)
			if executableSHA != "" && executableSHA != judge.SHA256 {
				t.Fatal("byte-identical fixed judge sources produced different executables")
			}
			executableSHA = judge.SHA256
			for _, input := range cases {
				raw, _ := json.Marshal(input)
				var pool operatorevidence.Pool
				if err = json.Unmarshal(raw, &pool); err != nil {
					t.Fatal(err)
				}
				// The raw constructors and existing judge policy are unchanged frozen
				// synthetic evidence. Compare every summary field at both release pins.
				expected := pooledPreviewMedia(&input)
				wantBytes, _ := json.Marshal(expected)
				want, err := operatorevidence.DecodeSummary(wantBytes)
				if err != nil {
					t.Fatal(err)
				}
				pool.SourceCommit = commit
				actual, err := judge.Judge(ctx, pool)
				if err != nil || !reflect.DeepEqual(actual, want) {
					t.Fatalf("pinned judge differs: got %+v (%v), want %+v", actual, err, want)
				}
			}
		})
	}
}

func pinnedCommit(ctx context.Context, root, commit string) error {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", commit+"^{commit}")
	cmd.Dir = root
	cmd.Env = operatorcaller.PublicEnvironment(nil)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("required pinned release commit %s is unavailable: %w; checkout must use fetch-depth: 0", commit, err)
	}
	return nil
}

func TestMissingPinnedCommitExplainsHistoryRequirement(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	commit := strings.Repeat("0", 40)
	err := pinnedCommit(ctx, t.TempDir(), commit)
	if err == nil || !strings.Contains(err.Error(), commit) || !strings.Contains(err.Error(), "fetch-depth: 0") {
		t.Fatal("missing pinned commit must identify the commit and checkout requirement", err)
	}
}
