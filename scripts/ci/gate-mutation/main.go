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
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed cases.json
var specifications []byte

type mutation struct{ ID, GuardFingerprint, File, Function, Old, New, Test, Marker string }
type event struct{ Action, Test, Output string }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
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
	for _, name := range []string{"go.mod", "go.sum", "pkg/operator", "pkg/buildinfo", "internal/operatortransport", "internal/operatorgates", "cmd/relais-gates"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
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
			return os.WriteFile(target, b, 0600)
		})
		if err != nil {
			return err
		}
	}
	cache := filepath.Join(temp, "cache")
	baseline, err := test(tree, cache, "", true)
	if err != nil {
		return fmt.Errorf("INVALID baseline: %w", err)
	}
	seen := map[string]bool{}
	for _, m := range cases {
		if seen[m.ID] || !has(baseline, m.Test, "pass") {
			return fmt.Errorf("INVALID %s: missing baseline test or duplicate ID", m.ID)
		}
		seen[m.ID] = true
		path := filepath.Join(tree, m.File)
		original, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		changed, err := edit(original, m)
		if err != nil {
			return fmt.Errorf("INVALID %s: %w", m.ID, err)
		}
		if err = os.WriteFile(path, changed, 0600); err != nil {
			return err
		}
		events, testErr := test(tree, cache, m.Test, false)
		if err = os.WriteFile(path, original, 0600); err != nil {
			return err
		}
		output := ""
		for _, e := range events {
			output += e.Output
		}
		invalid := strings.Contains(output, "panic:") || strings.Contains(output, "fatal error:") || strings.Contains(output, "build failed") || strings.Contains(output, "test timed out")
		assertion := false
		for _, e := range events {
			if e.Test == m.Test && strings.Contains(e.Output, m.Marker) {
				assertion = true
			}
		}
		if testErr == nil {
			return fmt.Errorf("SURVIVED %s", m.ID)
		}
		if invalid || !has(events, m.Test, "run") || !has(events, m.Test, "fail") || !assertion {
			return fmt.Errorf("INVALID %s: %v\n%s", m.ID, testErr, output)
		}
		fmt.Printf("KILLED %s (%s)\n", m.ID, m.Test)
	}
	ledger, protocol := 0, 0
	for _, m := range cases {
		if m.GuardFingerprint != "" {
			ledger++
		} else {
			protocol++
		}
	}
	fmt.Printf("VERIFIED: %d owned ledger mutants and %d new protocol mutants killed; other ledger cases pending\n", ledger, protocol)
	return nil
}
func edit(source []byte, m mutation) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, m.File, source, 0)
	if err != nil {
		return nil, err
	}
	var body *ast.FuncDecl
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Name.Name == m.Function {
			if body != nil {
				return nil, errors.New("ambiguous function")
			}
			body = f
		}
	}
	if body == nil {
		return nil, errors.New("function missing")
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
func test(tree, cache, name string, baseline bool) ([]event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"test", "-json", "-count=1", "-timeout=60s"}
	if !baseline {
		args = append(args, "-run", "^"+name+"$")
	}
	args = append(args, "./pkg/operator", "./internal/operatortransport", "./internal/operatorgates", "./cmd/relais-gates")
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
