package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/callharness"
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
	// Coverage scenarios run once against the identical executable; production
	// and sanitized projections run at both pins.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
	timing := rawPoolSample()
	editPoolCalls(t, &timing, func(c *callharness.Report) {
		c.Moves[0].Measurement.FirstContentVerdict = callharness.MetricVerdict{Reasons: []string{"first content classification changes within event-time interval; width=1ms"}}
	})
	cases = append(cases, timing)
	timingRaw, err := json.Marshal(timing)
	if err != nil {
		t.Fatal(err)
	}
	var timingPool operatorevidence.Pool
	if err = json.Unmarshal(timingRaw, &timingPool); err != nil {
		t.Fatal(err)
	}
	cleanTiming, err := operatorrehearsal.SanitizeMediaEvidence(timingPool)
	if err != nil {
		t.Fatal(err)
	}
	sanitizedTiming, err := decodePool(cleanTiming)
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, sanitizedTiming)
	for _, name := range []string{"covered", "missing", "lease-missing", "bound", "break", "kill-bracket", "falsified", "independent-failure", "independent-gap-failure", "repair-gap-with-later-sample", "omit-frontend-error", "omit-final-coverage"} {
		cases = append(cases, coveragePoolFixture(t, name))
	}
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
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			judge, err := operatorevidence.BuildPinned(ctx, root, commit, goExecutable, filepath.Join(t.TempDir(), "judge"))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("pin %s executable SHA256 %s", commit, judge.SHA256)
			if executableSHA != "" && executableSHA != judge.SHA256 {
				t.Fatal("byte-identical fixed judge sources produced different executables")
			}
			// Both pins build byte-identical executables (checked above), so the
			// broad case sweep runs once; the lease-missing case and production
			// projection below run at both pins.
			sweep := cases
			if executableSHA != "" {
				sweep = []previewPoolInput{coveragePoolFixture(t, "lease-missing")}
			}
			executableSHA = judge.SHA256
			for _, input := range sweep {
				// The raw constructors and existing judge policy are unchanged frozen
				// synthetic evidence. Compare every summary field.
				expected := pooledPreviewMedia(&input)
				wantBytes, _ := json.Marshal(expected)
				want, err := operatorevidence.DecodeSummary(wantBytes)
				if err != nil {
					t.Fatal(err)
				}
				wire, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				// Execute the exact compact transport against the pinned executable.
				result, err := exec.CommandContext(ctx, judge.Binary, writePinnedInput(t, wire)).Output()
				actual, decodeErr := operatorevidence.DecodeSummary(result)
				if err == nil {
					err = decodeErr
				}
				if err != nil || !reflect.DeepEqual(actual, want) {
					t.Fatalf("pinned judge differs: got %+v (%v), want %+v", actual, err, want)
				}

				// Compact qualification contains only event indexes, fixed codes
				// and the latch. It needs no clock-domain shift during sanitization.
				archival := input
				archival.ClockCoverage = nil
				rawArchive, _ := json.Marshal(archival)
				var archive operatorevidence.Pool
				if err := json.Unmarshal(rawArchive, &archive); err != nil {
					t.Fatal(err)
				}
				publicRaw, err := operatorrehearsal.SanitizeMediaEvidence(archive)
				if err != nil {
					t.Fatal(err)
				}
				publicInput, err := decodePool(publicRaw)
				if err != nil {
					t.Fatal(err)
				}
				publicInput.ClockCoverage = input.ClockCoverage
				publicWire, _ := json.Marshal(publicInput)
				publicResult, err := exec.CommandContext(ctx, judge.Binary, writePinnedInput(t, publicWire)).Output()
				publicVerdict, publicErr := operatorevidence.DecodeSummary(publicResult)
				if err != nil || publicErr != nil || !reflect.DeepEqual(actual, publicVerdict) {
					t.Fatalf("all-case sanitized judgment differs: %+v (%v, %v), want %+v", publicVerdict, err, publicErr, actual)
				}

			}
			// Exercise PinnedJudge.Judge's production projection as well as the
			// compact command entry above, including sanitized coverage input.
			for _, supplied := range []int{0, 1, 2} {
				raw, _ := json.Marshal(rawPoolSample())
				var pool operatorevidence.Pool
				if err := json.Unmarshal(raw, &pool); err != nil {
					t.Fatal(err)
				}
				pool.SourceCommit, pool.PreCoverageEvidence = commit, false
				if supplied > 0 {
					pool.ClockCoverage = coverageEvidenceFixture(t)
				}
				if supplied == 2 {
					_, e := boundPoolFixture(t)
					pool.ClockCoverage = e
					pool.ClockReceiptSHA256 = e.ClockReceiptSHA256
				}
				got, err := judge.Judge(ctx, pool)
				want := "inconclusive"
				if supplied > 0 {
					want = "passed"
				}
				if err != nil || string(got.Status) != want {
					t.Fatalf("production coverage projection: %+v (%v), want %s", got, err, want)
				}
				sanitized, err := operatorrehearsal.SanitizeMediaEvidence(pool)
				if err != nil {
					t.Fatal(err)
				}
				var public operatorevidence.Pool
				if err := json.Unmarshal(sanitized, &public); err != nil {
					t.Fatal(err)
				}
				public.SourceCommit = commit
				projected, err := judge.Judge(ctx, public)
				if err != nil || !reflect.DeepEqual(got, projected) {
					t.Fatalf("sanitized coverage projection differs: %+v (%v), want %+v", projected, err, got)
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

func writePinnedInput(t *testing.T, raw []byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return name
}
