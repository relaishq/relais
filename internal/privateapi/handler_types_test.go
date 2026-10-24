package privateapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
)

type checkedHTTPPackage struct {
	files map[string]*ast.File
	info  *types.Info
}

var checkedHTTP struct {
	once     sync.Once
	packages map[string]checkedHTTPPackage
	err      error
}

// Use compiler export data and go/types rather than a parallel table of method
// return types. This binds inferred local variables and direct Encode calls to
// the actual production signatures. No service or provider is contacted.
func checkedHTTPPackages(t *testing.T) map[string]checkedHTTPPackage {
	t.Helper()
	checkedHTTP.once.Do(func() { checkedHTTP.packages, checkedHTTP.err = checkHTTPPackages() })
	if checkedHTTP.err != nil {
		t.Fatal(checkedHTTP.err)
	}
	return checkedHTTP.packages
}
func checkHTTPPackages() (map[string]checkedHTTPPackage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-export", "-json", "./pkg/mediaworker", "./pkg/relay", "./pkg/controlplane", "./internal/privateapi")
	cmd.Dir = "../.."
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("load HTTP compiler exports: %w", err)
	}
	type pkgInfo struct {
		Dir, ImportPath, Export string
		GoFiles                 []string
	}
	var packages []pkgInfo
	exports := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var p pkgInfo
		err := decoder.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		packages = append(packages, p)
		exports[p.ImportPath] = p.Export
	}
	fset := token.NewFileSet()
	imported := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) })
	result := map[string]checkedHTTPPackage{}
	for _, p := range packages {
		rel := strings.TrimPrefix(p.ImportPath, "github.com/relaishq/relais/")
		if rel != "pkg/mediaworker" && rel != "pkg/relay" && rel != "pkg/controlplane" && rel != "internal/privateapi" {
			continue
		}
		files := map[string]*ast.File{}
		var source []*ast.File
		for _, name := range p.GoFiles {
			path := filepath.Join(p.Dir, name)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil, err
			}
			files[name] = f
			source = append(source, f)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}}
		config := types.Config{Importer: imported}
		if _, err := config.Check(p.ImportPath, fset, source, info); err != nil {
			return nil, err
		}
		result[rel] = checkedHTTPPackage{files, info}
	}
	return result, nil
}
func wireType(t types.Type) string {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	return types.TypeString(t, func(p *types.Package) string { return p.Path() })
}
func fixtureGoType(t reflect.Type) types.Type {
	if t.Name() != "" && t.PkgPath() != "" {
		pkg := types.NewPackage(t.PkgPath(), filepath.Base(t.PkgPath()))
		return types.NewNamed(types.NewTypeName(token.NoPos, pkg, t.Name(), nil), types.NewStruct(nil, nil), nil)
	}
	switch t.Kind() {
	case reflect.Pointer:
		return types.NewPointer(fixtureGoType(t.Elem()))
	case reflect.Slice:
		return types.NewSlice(fixtureGoType(t.Elem()))
	case reflect.Map:
		return types.NewMap(fixtureGoType(t.Key()), fixtureGoType(t.Elem()))
	case reflect.Struct:
		var fields []*types.Var
		var tags []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fields = append(fields, types.NewField(token.NoPos, nil, f.Name, fixtureGoType(f.Type), f.Anonymous))
			tags = append(tags, string(f.Tag))
		}
		return types.NewStruct(fields, tags)
	default:
		return types.Universe.Lookup(t.String()).Type()
	}
}
func fixtureWireType(value any) string {
	if value == nil {
		return ""
	}
	return wireType(fixtureGoType(reflect.TypeOf(value)))
}
func verifyResponseTypes(e endpoint, h handlerWire) error {
	expected := fixtureWireType(e.Response)
	allowed := []string{expected, fixtureWireType(privateapi.Failure{}), fixtureWireType(controlplane.MoveFailure{})}
	if e.Service == "control" && e.Route == "GET /status" {
		allowed = append(allowed, fixtureWireType(struct {
			Ready   bool                        `json:"ready"`
			Rebuild *controlplane.RebuildStatus `json:"rebuild,omitempty"`
			Pool    controlplane.PoolSample     `json:"pool"`
			Error   string                      `json:"error"`
		}{}))
	}
	found := false
	for _, actual := range h.ResponseTypes {
		if actual == expected {
			found = true
		}
		ok := false
		for _, want := range allowed {
			if actual == want {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("%s %s writes %s, fixture describes %s", h.Service, h.Route, actual, expected)
		}
	}
	// SDP and Prometheus bodies use other writers; JSON structs must actually
	// reach a writer, including endpoints with only an empty struct response.
	if ty := reflect.TypeOf(e.Response); ty != nil && ty.Kind() == reflect.Struct && !found {
		return fmt.Errorf("%s %s no longer writes fixture response %s", h.Service, h.Route, expected)
	}
	return nil
}

func checkedFixtureHandler(t *testing.T, body string) handlerWire {
	t.Helper()
	source := `package mediaworker
    type WorkerStatus struct{ Sessions int }; type ResumeReply struct{ID string}
    func writeJSON(any,any){}
    type encoder struct{};func(encoder)Encode(any)error{return nil}
    type decoder struct{};func(decoder)Decode(any)error{return nil}
    var privateapi struct{Write func(any,any)}
    var json struct{NewDecoder func(any)decoder;Unmarshal func([]byte,any)error}
    var rw any;var r struct{Body any}
    ` + body
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "http.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	pkg := "github.com/relaishq/relais/pkg/mediaworker"
	config := types.Config{}
	if _, err = config.Check(pkg, fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "handler" {
			h, err := readHandler("worker", pkg, f, "GET /status", fn.Body, info)
			if err != nil {
				t.Fatal(err)
			}
			return h
		}
	}
	t.Fatal("missing fixture handler")
	return handlerWire{}
}
func TestHTTPHandlerResponseTypesTrackWriters(t *testing.T) {
	fixture := []endpoint{{Service: "worker", Route: "GET /status", Response: mediaworker.WorkerStatus{}}}
	for _, body := range []string{
		`func handler(){writeJSON(rw,REPLY{})}`,
		`func handler(){privateapi.Write(rw,REPLY{})}`,
		`func handler(){encoder{}.Encode(REPLY{})}`,
		`func handler(){value:=REPLY{};writeJSON(rw,value)}`,
		`func reply()REPLY{return REPLY{}};func handler(){value:=reply();writeJSON(rw,value)}`,
		`func handler(){value:=new(REPLY);writeJSON(rw,value)}`,
	} {
		t.Run(body, func(t *testing.T) {
			for _, reply := range []string{"WorkerStatus", "ResumeReply"} {
				h := checkedFixtureHandler(t, strings.ReplaceAll(body, "REPLY", reply))
				err := verifyDecodedRequests(fixture, []handlerWire{h})
				if (err != nil) != (reply == "ResumeReply") {
					t.Fatalf("reply=%s: %v", reply, err)
				}
			}
		})
	}
}
func TestHTTPHandlerRejectsUnfixturedBodyDecoding(t *testing.T) {
	fixture := []endpoint{{Service: "worker", Route: "GET /status"}}
	for _, body := range []string{
		`func handler(){var req ResumeReply;json.NewDecoder(r.Body).Decode(&req)}`,
		`func handler(){var req ResumeReply;d:=json.NewDecoder(r.Body);d.Decode(&req)}`,
		`func handler(){var req ResumeReply;json.Unmarshal([]byte("{}"),&req)}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := checkedFixtureHandler(t, body)
			if err := verifyDecodedRequests(fixture, []handlerWire{h}); err == nil || !strings.Contains(err.Error(), "decodes a body absent") {
				t.Fatalf("raw decoder escaped request fixture: %v", err)
			}
		})
	}
	h := checkedFixtureHandler(t, `func handler(){}`)
	if err := verifyDecodedRequests(fixture, []handlerWire{h}); err != nil {
		t.Fatal(err)
	}
}
