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

// A declaration position plus its name distinguishes lexical bindings without
// treating unrelated variables with the same spelling as the same command.
type bindingID struct {
	name        string
	declaration token.Pos
}

func bindingKey(id *ast.Ident) bindingID { return bindingID{id.Name, id.Obj.Pos()} }

type scriptSource struct {
	path       string
	file       *ast.File
	constants  map[string]ast.Expr
	scripts    map[string]ast.Expr
	redisNames map[string]bool
	bindings   map[bindingID]ast.Expr
}

// LuaScripts inventories call sites rather than filenames or naming conventions.
// Only packages transitively imported by relais-worker/control/relay/signal
// participate (all non-test files, conservatively including every build tag).
// Diagnostic binaries are outside this contract. EvalSha/RO accept only the
// Hash of a statically resolved NewScript; raw script/function commands refuse.
// Dynamic script bodies/hashes are deliberately a generation error.
func LuaScripts(root string) (map[string]string, error) {
	var sources []scriptSource
	graph := map[string][]string{}
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
		for _, imp := range f.Imports {
			imported, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(imported, "github.com/relaishq/relais/") {
				dependency := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(imported, "github.com/relaishq/relais/")))
				graph[dir] = append(graph[dir], dependency)
			}
		}
		bindings := map[bindingID]ast.Expr{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if i < len(x.Values) && name.Obj != nil {
						if _, seen := bindings[bindingKey(name)]; seen {
							bindings[bindingKey(name)] = nil
						} else {
							bindings[bindingKey(name)] = x.Values[i]
						}
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Obj != nil && i < len(x.Rhs) {
						if _, seen := bindings[bindingKey(id)]; seen {
							bindings[bindingKey(id)] = nil
						} else {
							bindings[bindingKey(id)] = x.Rhs[i]
						}
					}
				}
			}
			return true
		})
		// Setters and borrowed Args slices make a literal command mutable.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (!strings.HasPrefix(sel.Sel.Name, "Set") && sel.Sel.Name != "Args") {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Obj != nil {
				bindings[bindingKey(id)] = nil
			}
			return true
		})
		src := scriptSource{path, f, packages[dir], scripts[dir], names, bindings}
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
	reachable := map[string]bool{}
	var visit func(string)
	visit = func(dir string) {
		if reachable[dir] {
			return
		}
		reachable[dir] = true
		for _, dependency := range graph[dir] {
			visit(dependency)
		}
	}
	for _, component := range []string{"worker", "control", "relay", "signal"} {
		visit(filepath.Join(root, "cmd/relais-"+component))
	}
	out := map[string]string{}
	for _, src := range sources {
		if !reachable[filepath.Dir(src.path)] {
			continue
		}
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
			case sel.Sel.Name == "Do" && len(call.Args) >= 2:
				// Package functions such as privateapi.Do are not Redis methods.
				if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil {
					imported := false
					for _, imp := range src.file.Imports {
						name := filepath.Base(strings.Trim(imp.Path.Value, "\""))
						if imp.Name != nil {
							name = imp.Name.Name
						}
						if name == id.Name {
							imported = true
						}
					}
					if imported {
						return true
					}
				}
				err = src.checkRawCommand(call.Args[1], "Do")
				return err == nil
			case sel.Sel.Name == "Process" && len(call.Args) >= 2:
				arg := src.commandExpression(call.Args[1], map[string]bool{})
				constructor, ok := arg.(*ast.CallExpr)
				if !ok {
					if !src.legacyAgentInput(call.Args[1]) {
						err = fmt.Errorf("unresolved Redis Process command in %s", src.path)
					}
					return err == nil
				}
				factory, ok := constructor.Fun.(*ast.SelectorExpr)
				if !ok || !strings.HasPrefix(factory.Sel.Name, "New") || !strings.HasSuffix(factory.Sel.Name, "Cmd") {
					err = fmt.Errorf("unresolved Redis Process command in %s", src.path)
					return false
				}
				qualifier, trusted := factory.X.(*ast.Ident)
				if !trusted || !src.redisNames[qualifier.Name] {
					err = fmt.Errorf("unresolved Redis Process command constructor in %s", src.path)
					return false
				}
				if len(constructor.Args) < 2 {
					err = fmt.Errorf("unresolved Redis Process command in %s", src.path)
					return false
				}
				err = src.checkRawCommand(constructor.Args[1], "Process")
				return err == nil
			case sel.Sel.Name == "Eval" || sel.Sel.Name == "EvalSha" || sel.Sel.Name == "EvalRO" || sel.Sel.Name == "EvalShaRO":
				if len(call.Args) < 2 {
					err = fmt.Errorf("invalid %s in %s", sel.Sel.Name, src.path)
					return false
				}
				arg = call.Args[1]
				if sel.Sel.Name == "EvalSha" || sel.Sel.Name == "EvalShaRO" {
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

// Process receives a command object rather than a command string. Follow local
// initializers/assignments to its New*Cmd constructor; never execute it.
func (s scriptSource) commandExpression(expr ast.Expr, seen map[string]bool) ast.Expr {
	if id, ok := expr.(*ast.Ident); ok && id.Obj != nil && !seen[id.Name] {
		seen[id.Name] = true
		if s.bindings[bindingKey(id)] != nil {
			return s.commandExpression(s.bindings[bindingKey(id)], seen)
		}
	}
	return expr
}
func (s scriptSource) checkRawCommand(expr ast.Expr, method string) error {
	command, err := s.stringConstant(expr, map[string]bool{})
	if err != nil {
		return fmt.Errorf("unresolved Redis %s command in %s: command must resolve to a string constant", method, s.path)
	}
	switch strings.ToUpper(command) {
	case "EVAL", "EVALSHA", "EVAL_RO", "EVALSHA_RO", "FCALL", "FCALL_RO":
		return fmt.Errorf("raw Redis %s command %s in %s bypasses the script contract; use a statically resolved NewScript/Eval API", method, command, s.path)
	}
	return nil
}

// Process also names the legacy audio-agent callback. Its agent.Input argument
// is not a Redis command. Prove that type from the declaration rather than
// exempting packages that happen not to import go-redis (clients can be returned
// by other packages). All other unresolved Process arguments fail closed.
func (s scriptSource) legacyAgentInput(expr ast.Expr) bool {
	var typ ast.Expr
	switch x := expr.(type) {
	case *ast.Ident:
		if x.Obj != nil {
			if field, ok := x.Obj.Decl.(*ast.Field); ok {
				typ = field.Type
			}
		}
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok || id.Obj == nil {
			return false
		}
		parameter, ok := id.Obj.Decl.(*ast.Field)
		if !ok {
			return false
		}
		local, ok := parameter.Type.(*ast.Ident)
		if !ok || local.Obj == nil {
			return false
		}
		spec, ok := local.Obj.Decl.(*ast.TypeSpec)
		if !ok {
			return false
		}
		object, ok := spec.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, field := range object.Fields.List {
			for _, name := range field.Names {
				if name.Name == x.Sel.Name {
					typ = field.Type
				}
			}
		}
	}
	named, ok := typ.(*ast.SelectorExpr)
	if !ok || named.Sel.Name != "Input" {
		return false
	}
	qualifier, ok := named.X.(*ast.Ident)
	if !ok {
		return false
	}
	for _, imp := range s.file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		alias := filepath.Base(path)
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		if alias == qualifier.Name && path == "github.com/relaishq/relais/pkg/agent" {
			return true
		}
	}
	return false
}
