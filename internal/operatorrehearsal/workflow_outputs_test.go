package operatorrehearsal

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Include default CLI output directories as well as explicit workflow paths.
// A temporary repository isolates this check from global ignores and also works
// in the mutation runner's source copies, which deliberately omit .git.
func TestWorkflowCheckoutOutputsAreIgnored(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/operator-rehearsal.yml")
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile("../../.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"bin/", "runtime/operator-releases/", "runtime/operator-report/"}
	for _, path := range regexp.MustCompile(`\b(?:runtime|bin)/[a-zA-Z0-9_./-]+`).FindAllString(string(workflow), -1) {
		if !strings.Contains(filepath.Base(path), ".") {
			path += "/"
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	dir := t.TempDir()
	git := func(args ...string) *exec.Cmd {
		command := exec.Command("git", append([]string{"-c", "core.excludesFile=/dev/null", "-c", "init.templateDir=", "-C", dir}, args...)...)
		return command
	}
	if out, err := git("init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("fixture repository: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), ignore, 0600); err != nil {
		t.Fatal(err)
	}
	command := git("check-ignore", "--no-index", "--stdin")
	command.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git check-ignore: %v", err)
	}
	ignored := strings.Fields(string(output))
	for _, path := range paths {
		if !slices.Contains(ignored, path) {
			t.Errorf("workflow checkout output is not ignored: %s", path)
		}
	}
}
