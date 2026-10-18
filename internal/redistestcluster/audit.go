package redistestcluster

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// AuditScripts makes adding a script or atomic multi-key call require an
// explicit update to the slot audit. Independent pipelines are allowed.
func AuditScripts(t *testing.T, allowed ...string) {
	t.Helper()
	known := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		known[name] = true
	}
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range files {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			index := -1
			switch selector.Sel.Name {
			case "NewScript":
				index = 0
			case "Eval", "EvalSha":
				index = 1
			case "Del", "Unlink":
				if len(call.Args) > 2 || call.Ellipsis.IsValid() {
					t.Errorf("%s: atomic multi-key delete needs a slot audit", fset.Position(call.Pos()))
				}
			case "MGet", "MSet", "MSetNX", "Rename", "RenameNX", "SMove", "RPopLPush", "BRPopLPush", "TxPipeline", "TxPipelined":
				t.Errorf("%s: new atomic multi-key operation %s needs a slot audit", fset.Position(call.Pos()), selector.Sel.Name)
			}
			if index >= 0 && len(call.Args) > index {
				var expression bytes.Buffer
				if err := format.Node(&expression, fset, call.Args[index]); err != nil {
					t.Fatal(err)
				}
				if !known[expression.String()] {
					t.Errorf("%s: script %s is missing from the slot audit", fset.Position(call.Pos()), expression.String())
				}
			}
			return true
		})
	}
}
