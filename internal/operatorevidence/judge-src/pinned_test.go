package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
)

func TestPinnedReleaseJudgesMatchFrozenEvidence(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("offline release snapshot builds")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("isolated mutation tree has no release history")
	}
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cases := []previewPoolInput{rawPoolSample(), {RunID: "run-fixture", Missing: true}}
	lost := rawPoolSample()
	lost.Missing = true
	lost.Phases[0].Receipts[0].Result.Lost = true
	cases = append(cases, lost)
	for _, commit := range []string{"a0d9744ff1223a8d7978447c907041973e915b08", "85e10436eb6adbbf577b6538b0bbb799aab823b2"} {
		t.Run(commit, func(t *testing.T) {
			judge, err := operatorevidence.BuildPinned(ctx, root, commit, goExecutable, filepath.Join(t.TempDir(), "judge"))
			if err != nil {
				t.Fatal(err)
			}
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
