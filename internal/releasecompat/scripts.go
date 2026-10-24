package releasecompat

import (
	"crypto/sha1" // Redis script identifiers, never authentication.
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

type scriptSource struct {
	path       string
	file       *ast.File
	constants  map[string]ast.Expr
	scripts    map[string]ast.Expr
	redisNames map[string]bool
}

// LuaScripts inventories call sites rather than filenames or naming conventions.
// EvalSha is accepted only for the Hash of a statically resolved NewScript.
// Dynamic script bodies/hashes are deliberately a generation error.
func LuaScripts(root string) (map[string]string, error) {
	var sources []scriptSource
	packages := map[string]map[string]ast.Expr{}
	scripts := map[string]map[string]ast.Expr{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		if packages[dir] == nil {
			packages[dir] = map[string]ast.Expr{}
			scripts[dir] = map[string]ast.Expr{}
		}
		names := map[string]bool{}
		for _, imp := range f.Imports {
			name, _ := strconv.Unquote(imp.Path.Value)
			if name == "github.com/redis/go-redis/v9" {
				alias := "redis"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
				names[alias] = true
			}
		}
		src := scriptSource{path, f, packages[dir], scripts[dir], names}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range g.Specs {
				v, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range v.Names {
					if i >= len(v.Values) {
						continue
					}
					if g.Tok == token.CONST {
						src.constants[n.Name] = v.Values[i]
					}
					if call, ok := v.Values[i].(*ast.CallExpr); ok && src.newScript(call) {
						if len(call.Args) != 1 {
							return fmt.Errorf("invalid NewScript in %s", path)
						}
						src.scripts[n.Name] = call.Args[0]
					}
				}
			}
		}
		sources = append(sources, src)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, src := range sources {
		inline := 0
		ast.Inspect(src.file, func(n ast.Node) bool {
			if err != nil {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var arg ast.Expr
			switch {
			case src.newScript(call):
				if len(call.Args) != 1 {
					err = fmt.Errorf("invalid NewScript in %s", src.path)
					return false
				}
				arg = call.Args[0]
			case sel.Sel.Name == "Eval" || sel.Sel.Name == "EvalSha":
				if len(call.Args) < 2 {
					err = fmt.Errorf("invalid %s in %s", sel.Sel.Name, src.path)
					return false
				}
				arg = call.Args[1]
				if sel.Sel.Name == "EvalSha" {
					hash, ok := arg.(*ast.CallExpr)
					if !ok || len(hash.Args) != 0 {
						err = fmt.Errorf("unresolved EvalSha script in %s", src.path)
						return false
					}
					method, ok := hash.Fun.(*ast.SelectorExpr)
					if !ok || method.Sel.Name != "Hash" {
						err = fmt.Errorf("unresolved EvalSha hash in %s", src.path)
						return false
					}
					id, ok := method.X.(*ast.Ident)
					if !ok || src.scripts[id.Name] == nil {
						err = fmt.Errorf("unresolved EvalSha receiver in %s", src.path)
						return false
					}
					arg = src.scripts[id.Name]
				}
			default:
				return true
			}
			body, e := src.stringConstant(arg, map[string]bool{})
			if e != nil {
				err = fmt.Errorf("unresolved %s script in %s: %w", sel.Sel.Name, src.path, e)
				return false
			}
			rel, e := filepath.Rel(root, filepath.Dir(src.path))
			if e != nil {
				err = e
				return false
			}
			name := ""
			if id, ok := arg.(*ast.Ident); ok {
				name = id.Name
			} else {
				inline++
				name = filepath.Base(src.path) + ":" + sel.Sel.Name + ":" + strconv.Itoa(inline)
			}
			if filepath.ToSlash(rel) != "pkg/sessionstore" {
				name = filepath.ToSlash(rel) + "/" + name
			}
			sum := sha1.Sum([]byte(body))
			sha := hex.EncodeToString(sum[:])
			if old, ok := out[name]; ok && old != sha {
				err = fmt.Errorf("ambiguous script identity %s", name)
				return false
			}
			out[name] = sha
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s scriptSource) newScript(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewScript" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && s.redisNames[id.Name]
}
func (s scriptSource) stringConstant(expr ast.Expr, seen map[string]bool) (string, error) {
	switch x := expr.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			return strconv.Unquote(x.Value)
		}
	case *ast.ParenExpr:
		return s.stringConstant(x.X, seen)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			a, err := s.stringConstant(x.X, seen)
			if err != nil {
				return "", err
			}
			b, err := s.stringConstant(x.Y, seen)
			return a + b, err
		}
	case *ast.Ident:
		if x.Obj != nil && x.Obj.Kind == ast.Con {
			if decl, ok := x.Obj.Decl.(*ast.ValueSpec); ok && len(decl.Values) == 1 && !seen[x.Name] {
				seen[x.Name] = true
				defer delete(seen, x.Name)
				return s.stringConstant(decl.Values[0], seen)
			}
		}
		if !seen[x.Name] && s.constants[x.Name] != nil {
			seen[x.Name] = true
			defer delete(seen, x.Name)
			return s.stringConstant(s.constants[x.Name], seen)
		}
	}
	return "", fmt.Errorf("script must resolve to a string constant")
}
