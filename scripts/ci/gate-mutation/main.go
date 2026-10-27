// gate-mutation applies exact function-scoped edits to an isolated source copy.
// A mutant is killed only by the named test's assertion marker, never by a
// compiler error, missing test, panic, timeout or unrelated failure.
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
	"slices"
	"sort"
	"strings"
	"time"
)

//go:embed cases.json
var specifications []byte

type change struct{ File, Function, Old, New string }
type mutation struct {
	ID, GuardFingerprint, File, Function, Old, New, Test, Marker, Package, PythonTest string
	Edits                                                                             []change
}
type event struct{ Action, Test, Output string }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	only := flag.String("only", "", "one mutation ID")
	flag.Parse()
	var cases []mutation
	if err := json.Unmarshal(specifications, &cases); err != nil {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "relais-gate-mutation-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	tree := filepath.Join(temp, "source")
	if err := os.Mkdir(tree, 0700); err != nil {
		return err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
	if err != nil {
		return err
	}
	cache := filepath.Join(temp, "cache")
	packages := []string{}
	for _, m := range cases {
		packages = append(packages, m.Package)
	}
	sort.Strings(packages)
	packages = slices.Compact(packages)
	baseline, err := test(tree, cache, "", packages)
	if err != nil {
		return fmt.Errorf("INVALID baseline: %w", err)
	}
	seen := map[string]bool{}
	ledger, additional, selected := 0, 0, 0
	for _, m := range cases {
		if seen[m.ID] || !has(baseline, m.Test, "pass") {
			return fmt.Errorf("INVALID %s: missing baseline test or duplicate ID", m.ID)
		}
		seen[m.ID] = true
		if *only != "" && m.ID != *only {
			continue
		}
		selected++
		edits := m.Edits
		if len(edits) == 0 {
			edits = []change{{m.File, m.Function, m.Old, m.New}}
		}
		originals := map[string][]byte{}
		for _, editSpec := range edits {
			path := filepath.Join(tree, editSpec.File)
			original, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if _, ok := originals[path]; !ok {
				originals[path] = original
			}
			changed, err := edit(original, editSpec)
			if err != nil {
				return fmt.Errorf("INVALID %s: %w", m.ID, err)
			}
			if err = os.WriteFile(path, changed, 0600); err != nil {
				return err
			}
		}
		events, testErr := test(tree, cache, m.Test, []string{m.Package})
		for path, original := range originals {
			if err = os.WriteFile(path, original, 0600); err != nil {
				return err
			}
		}
		output := ""
		for _, e := range events {
			output += e.Output
		}
		if testErr == nil {
			return fmt.Errorf("SURVIVED %s", m.ID)
		}
		var exit *exec.ExitError
		if !errors.As(testErr, &exit) || exit.ExitCode() != 1 || !assertionFailure(events, m.Test, m.Marker) {
			return fmt.Errorf("INVALID %s: %v\n%s", m.ID, testErr, output)
		}
		fmt.Printf("KILLED %s (%s)\n", m.ID, m.Test)
		if m.GuardFingerprint != "" {
			ledger++
		} else {
			additional++
		}
	}
	if selected == 0 {
		return errors.New("no mutation selected")
	}
	fmt.Printf("VERIFIED: %d owned ledger mutants and %d additional mutants killed; other ledger cases pending\n", ledger, additional)
	return nil
}
func edit(source []byte, m change) ([]byte, error) {
	if strings.HasSuffix(m.File, ".py") {
		if strings.Count(string(source), m.Old) != 1 {
			return nil, errors.New("python edit must match exactly once")
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
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	args := []string{"test", "-json", "-count=1", "-timeout=60s"}
	if name != "" {
		args = append(args, "-run", "^"+name+"$")
	}
	args = append(args, packages...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = tree
	for _, e := range os.Environ() {
		key := strings.SplitN(e, "=", 2)[0]
		switch key {
		case "PATH", "HOME", "TMPDIR", "GOMODCACHE", "GOROOT":
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "GOCACHE="+cache, "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly")
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
