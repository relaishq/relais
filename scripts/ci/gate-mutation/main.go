// gate-mutation applies exact function-scoped edits, whole-file edits or
// unified patches to an isolated source copy. A mutant is killed only by the
// named test's assertion marker, never by a compiler error, missing test,
// panic, timeout or unrelated failure. -cases runs an external case file for
// any package instead of the embedded operator cases.
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed cases.json
var specifications []byte

type change struct {
	File, Function, Old, New string
	Receiver                 *string
}
type mutation struct {
	ID, GuardFingerprint, File, Function, Old, New, Test, Marker, Package, PythonTest, Patch string
	Edits                                                                                    []change
	Receiver                                                                                 *string
}
type event struct{ Action, Test, Output string }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	only := flag.String("only", "", "one mutation ID or comma-separated IDs")
	jobs := flag.Int("jobs", 2, "parallel isolated mutation workers (1-4)")
	file := flag.String("cases", "", "JSON case file to run instead of the embedded operator cases")
	flag.Parse()
	if *jobs < 1 || *jobs > 4 {
		return errors.New("jobs must be between 1 and 4")
	}
	cases, err := loadCases(*file)
	if err != nil {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	return runCases(root, cases, *only, *jobs)
}

// loadCases reads the embedded operator cases, or path when it is set. A
// relative Patch path is relative to the case file.
func loadCases(path string) ([]mutation, error) {
	data, dir := specifications, ""
	if path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
		if dir, err = filepath.Abs(filepath.Dir(path)); err != nil {
			return nil, err
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cases []mutation
	if err := decoder.Decode(&cases); err != nil {
		return nil, err
	}
	for i, m := range cases {
		if m.ID == "" || m.Test == "" || m.Marker == "" || m.Package == "" || (m.Patch == "") == (m.File == "" && len(m.Edits) == 0) {
			return nil, fmt.Errorf("INVALID case %d %q: needs ID, Test, Marker, Package and either Patch or edits", i, m.ID)
		}
		if m.Patch != "" && (m.Old != "" || m.New != "" || m.Function != "" || m.Receiver != nil) {
			return nil, fmt.Errorf("INVALID case %d %q: Patch cannot be combined with Old, New, Function or Receiver", i, m.ID)
		}
		for _, e := range m.Edits {
			if e.File == "" || e.Old == "" {
				return nil, fmt.Errorf("INVALID case %d %q: every edit needs File and Old", i, m.ID)
			}
		}
		if m.Patch != "" && !filepath.IsAbs(m.Patch) {
			cases[i].Patch = filepath.Join(dir, m.Patch)
		}
	}
	return cases, nil
}
func runCases(root string, cases []mutation, only string, jobs int) error {
	started := time.Now()
	temp, err := os.MkdirTemp("", "relais-gate-mutation-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	tree := filepath.Join(temp, "source")
	if err := os.Mkdir(tree, 0700); err != nil {
		return err
	}
	if err := copyTree(root, tree); err != nil {
		return err
	}
	cache := filepath.Join(temp, "cache")
	packages := []string{}
	for _, m := range cases {
		packages = append(packages, m.Package)
	}
	sort.Strings(packages)
	packages = slices.Compact(packages)
	fmt.Println("BASELINE: every named mutation assertion; full safety and pinned-release suites run in the unit job")
	baseline, err := test(tree, cache, baselinePattern(cases), packages)
	if err != nil {
		failed := map[string]bool{}
		for _, e := range baseline {
			if e.Action == "fail" && e.Test != "" {
				failed[e.Test] = true
			}
		}
		var output strings.Builder
		for _, e := range baseline {
			if len(failed) == 0 || failed[e.Test] {
				output.WriteString(e.Output)
			}
		}
		detail := output.String()
		if len(detail) > 4000 {
			detail = detail[len(detail)-4000:]
		}
		return fmt.Errorf("INVALID baseline: %w\n%s", err, detail)
	}
	seen := map[string]bool{}
	selected := []mutation{}
	for _, m := range cases {
		if seen[m.ID] || !has(baseline, m.Test, "pass") {
			return fmt.Errorf("INVALID %s: missing baseline test or duplicate ID", m.ID)
		}
		seen[m.ID] = true
		if only == "" || slices.Contains(strings.Split(only, ","), m.ID) {
			selected = append(selected, m)
		}
	}
	if len(selected) == 0 {
		return errors.New("no mutation selected")
	}
	// Exact same edit, assertion and package may satisfy several original guards.
	// Keep every ID in the report, but execute that identical mutant only once.
	groups := groupMutations(selected)
	results := make([]error, len(groups))
	queue := make(chan int, len(groups))
	for i := range groups {
		queue <- i
	}
	close(queue)
	var wg sync.WaitGroup
	for worker := 0; worker < min(jobs, len(groups)); worker++ {
		workerTree := filepath.Join(temp, fmt.Sprintf("worker-%d", worker))
		if err := copyTree(tree, workerTree); err != nil {
			return err
		}
		wg.Go(func() {
			for i := range queue {
				results[i] = kill(workerTree, cache, groups[i][0])
			}
		})
	}
	wg.Wait()
	ledger, additional := 0, 0
	for i, group := range groups {
		if results[i] != nil {
			return results[i]
		}
		for _, m := range group {
			fmt.Printf("KILLED %s (%s)\n", m.ID, m.Test)
			if m.GuardFingerprint != "" {
				ledger++
			} else {
				additional++
			}
		}
	}
	fmt.Printf("VERIFIED: %d owned ledger mutants and %d additional mutants killed; %d executions, %d workers, %s\n", ledger, additional, len(groups), jobs, time.Since(started).Round(time.Millisecond))
	return nil
}
func copyTree(root, tree string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".env") || name == ".git" || name == ".agents" || name == ".codex" || name == "bin" || name == "dist" || name == "node_modules" || name == "__pycache__" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("mutation-source-symlink")
		}
		target := filepath.Join(tree, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}
func baselinePattern(cases []mutation) string {
	names := []string{}
	for _, m := range cases {
		names = append(names, regexp.QuoteMeta(strings.SplitN(m.Test, "/", 2)[0]))
	}
	sort.Strings(names)
	return "^(" + strings.Join(slices.Compact(names), "|") + ")$"
}
func groupMutations(cases []mutation) [][]mutation {
	var groups [][]mutation
	index := map[string]int{}
	for _, m := range cases {
		identity := m
		identity.ID = ""
		identity.GuardFingerprint = ""
		key, _ := json.Marshal(identity)
		i, exists := index[string(key)]
		if !exists {
			i = len(groups)
			index[string(key)] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], m)
	}
	return groups
}

// kill restores every touched file before it returns, so a worker tree is
// clean for its next mutant. A restore that differs fails the whole run.
func kill(tree, cache string, m mutation) (err error) {
	originals := map[string][]byte{}
	defer func() {
		if restoreErr := restore(originals, os.WriteFile); restoreErr != nil {
			err = fmt.Errorf("INVALID %s: %w", m.ID, restoreErr)
		}
	}()
	if err := mutate(tree, m, originals); err != nil {
		return fmt.Errorf("INVALID %s: %w", m.ID, err)
	}
	events, testErr := test(tree, cache, "^"+m.Test+"$", []string{m.Package})
	if testErr == nil {
		return fmt.Errorf("SURVIVED %s", m.ID)
	}
	var failure *exec.ExitError
	if !errors.As(testErr, &failure) || failure.ExitCode() != 1 || !assertionFailure(events, m.Test, m.Marker) {
		var output strings.Builder
		for _, e := range events {
			output.WriteString(e.Output)
		}
		return fmt.Errorf("INVALID %s: %v\n%s", m.ID, testErr, output.String())
	}
	return nil
}

// mutate records each file's original bytes before its first change.
func mutate(tree string, m mutation, originals map[string][]byte) error {
	if m.Patch != "" {
		return patch(tree, m.Patch, originals)
	}
	edits := m.Edits
	if len(edits) == 0 {
		edits = []change{{File: m.File, Function: m.Function, Old: m.Old, New: m.New, Receiver: m.Receiver}}
	}
	for _, spec := range edits {
		path := filepath.Join(tree, spec.File)
		original, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, exists := originals[path]; !exists {
			originals[path] = original
		}
		changed, err := edit(original, spec)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, changed, 0600); err != nil {
			return err
		}
	}
	return nil
}

// patch applies a unified diff with git apply, which refuses context that does
// not match exactly. Only changes to existing files are accepted, so every
// touched file has original bytes to restore and compare.
func patch(tree, diff string, originals map[string][]byte) error {
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", append(append([]string{"apply"}, args...), diff)...)
		cmd.Dir = tree
		// The source copy has no .git; never resolve paths against a parent repository.
		cmd.Env = append(allowedEnv(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(tree))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git apply %s: %w\n%s", strings.Join(args, " "), err, out)
		}
		return out, nil
	}
	summary, err := git("--summary")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(summary)) != 0 {
		return fmt.Errorf("patch may only modify existing files:\n%s", summary)
	}
	stat, err := git("--numstat", "-z")
	if err != nil {
		return err
	}
	files, err := numstatFiles(string(stat))
	if err != nil {
		return err
	}
	for _, file := range files {
		path := filepath.Join(tree, file)
		original, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		originals[path] = original
	}
	_, err = git()
	return err
}

// numstatFiles reads the paths from git apply --numstat -z output.
func numstatFiles(stat string) ([]string, error) {
	files := []string{}
	for _, row := range strings.Split(stat, "\x00") {
		if row == "" {
			continue
		}
		fields := strings.SplitN(row, "\t", 3)
		if len(fields) != 3 || fields[2] == "" {
			return nil, fmt.Errorf("unexpected git apply --numstat row %q", row)
		}
		files = append(files, fields[2])
	}
	return files, nil
}

// restore writes the original bytes back, then rereads every touched file and
// fails when one differs.
func restore(originals map[string][]byte, write func(string, []byte, os.FileMode) error) error {
	for path, original := range originals {
		if err := write(path, original, 0600); err != nil {
			return err
		}
	}
	for path, original := range originals {
		restored, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(restored, original) {
			return fmt.Errorf("restored file differs: %s", path)
		}
	}
	return nil
}

// edit replaces Old once inside the named declaration, or once in the whole
// file when Function is empty. Python edits are always whole-file.
func edit(source []byte, m change) ([]byte, error) {
	if strings.HasSuffix(m.File, ".py") || m.Function == "" {
		if strings.Count(string(source), m.Old) != 1 {
			return nil, errors.New("file edit must match exactly once")
		}
		return []byte(strings.Replace(string(source), m.Old, m.New, 1)), nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, m.File, source, 0)
	if err != nil {
		return nil, err
	}
	var body ast.Node
	count := 0
	ast.Inspect(file, func(n ast.Node) bool {
		match := false
		switch decl := n.(type) {
		case *ast.FuncDecl:
			match = decl.Name.Name == m.Function
			if match && m.Receiver != nil {
				receiver := ""
				if decl.Recv != nil {
					node := decl.Recv.List[0].Type
					if star, ok := node.(*ast.StarExpr); ok {
						node = star.X
					}
					ident, ok := node.(*ast.Ident)
					if !ok {
						return true
					}
					receiver = ident.Name
				}
				match = receiver == *m.Receiver
			}
		case *ast.TypeSpec:
			match = "type:"+decl.Name.Name == m.Function
		case *ast.ValueSpec:
			for _, name := range decl.Names {
				if "var:"+name.Name == m.Function {
					match = true
				}
			}
		}
		if match {
			body = n
			count++
		}
		return true
	})
	if count != 1 {
		return nil, errors.New("declaration missing or ambiguous")
	}
	start, end := fset.Position(body.Pos()).Offset, fset.Position(body.End()).Offset
	fragment := string(source[start:end])
	if strings.Count(fragment, m.Old) != 1 {
		return nil, errors.New("edit must match exactly once")
	}
	result := append([]byte{}, source[:start]...)
	result = append(result, strings.Replace(fragment, m.Old, m.New, 1)...)
	result = append(result, source[end:]...)
	return format.Source(result)
}
func has(events []event, test, action string) bool {
	for _, e := range events {
		if e.Test == test && e.Action == action {
			return true
		}
	}
	return false
}
func test(tree, cache, name string, packages []string) ([]event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	timeout := "60s"
	if strings.HasPrefix(name, "^(") {
		timeout = "4m"
	} // The short baseline includes every named safety regression.
	// The isolated source has no Git history. Real-checkout unit tests always
	// run the pinned release comparison, including PR short mode.
	args := []string{"test", "-p=2", "-short", "-parallel=8", "-json", "-count=1", "-timeout=" + timeout, "-skip=^TestPinnedReleaseJudgesMatchFrozenEvidence$"}
	if name != "" {
		args = append(args, "-run", name)
	}
	args = append(args, packages...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = tree
	cmd.Env = append(allowedEnv(), "GOCACHE="+cache, "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly -buildvcs=false -trimpath")
	out, err := cmd.CombinedOutput()
	events := []event{}
	scan := bufio.NewScanner(bytes.NewReader(out))
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var e event
		if json.Unmarshal(scan.Bytes(), &e) != nil {
			return events, fmt.Errorf("non-test output: %s", scan.Bytes())
		}
		events = append(events, e)
	}
	if scan.Err() != nil {
		return events, scan.Err()
	}
	if ctx.Err() != nil {
		return events, ctx.Err()
	}
	return events, err
}

// allowedEnv keeps only the variables child go and git commands need, so an
// exported GOFLAGS, GIT_DIR or GIT_WORK_TREE cannot change their behaviour.
func allowedEnv() []string {
	env := []string{}
	for _, e := range os.Environ() {
		switch strings.SplitN(e, "=", 2)[0] {
		case "PATH", "HOME", "TMPDIR", "GOMODCACHE", "GOROOT":
			env = append(env, e)
		}
	}
	return env
}

func assertionFailure(events []event, name, marker string) bool {
	assertion := false
	for _, e := range events {
		if strings.Contains(e.Output, "panic:") || strings.Contains(e.Output, "fatal error:") || strings.Contains(e.Output, "build failed") || strings.Contains(e.Output, "test timed out") {
			return false
		}
		if e.Test == name && strings.Contains(e.Output, marker) {
			assertion = true
		}
	}
	return has(events, name, "run") && has(events, name, "fail") && assertion
}
