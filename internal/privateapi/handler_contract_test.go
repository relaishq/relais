package privateapi_test

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/relay"
)

type handlerWire struct {
	Service     string   `json:"service"`
	Route       string   `json:"route"`
	RequestType string   `json:"decoded_request_type,omitempty"`
	Queries     []string `json:"query_parameters"`
	Statuses    []string `json:"explicit_statuses"`
	Anonymous   []string `json:"anonymous_response_types"`
}
type httpWire struct {
	Handlers    []handlerWire       `json:"registered_handlers"`
	Statuses    map[string][]string `json:"status_codes_including_helpers"`
	Codes       []string            `json:"failure_codes"`
	Unavailable statusResponse      `json:"control_status_503"`
}

func syntax(n ast.Node) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, token.NewFileSet(), n); err != nil {
		panic(err)
	}
	return b.String()
}
func text(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
func compact(s []string) []string { slices.Sort(s); return slices.Compact(s) }

// Read the actual registration and decoder arguments. These are semantic wire
// facts, not source fingerprints: unrelated handler edits do not change them.
func readHandler(service, pkg string, f *ast.File, route string, body ast.Node) (handlerWire, error) {
	h := handlerWire{Service: service, Route: route}
	declarations := map[string]ast.Expr{}
	imports := map[string]string{}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = path
	}
	var problem error
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			for _, id := range x.Names {
				declarations[id.Name] = x.Type
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if syntax(sel.X) == "privateapi" && sel.Sel.Name == "Read" {
				if len(x.Args) != 3 {
					problem = fmt.Errorf("unrecognized decoder at %s", route)
					return false
				}
				arg := x.Args[2]
				if u, ok := arg.(*ast.UnaryExpr); ok {
					arg = u.X
				}
				var typ ast.Expr
				switch v := arg.(type) {
				case *ast.Ident:
					typ = declarations[v.Name]
				case *ast.CompositeLit:
					typ = v.Type
				}
				switch v := typ.(type) {
				case *ast.Ident:
					h.RequestType = pkg + "." + v.Name
				case *ast.SelectorExpr:
					h.RequestType = imports[syntax(v.X)] + "." + v.Sel.Name
				default:
					problem = fmt.Errorf("cannot resolve decoded request type at %s", route)
				}
			}
			if sel.Sel.Name == "Get" && strings.HasSuffix(syntax(sel.X), ".Query()") {
				if len(x.Args) != 1 {
					problem = fmt.Errorf("unrecognized query at %s", route)
					return false
				}
				key, ok := text(x.Args[0])
				if !ok {
					problem = fmt.Errorf("dynamic query at %s", route)
				} else {
					h.Queries = append(h.Queries, key)
				}
			}
			if sel.Sel.Name == "WriteHeader" || syntax(sel) == "http.Error" {
				h.Statuses = append(h.Statuses, syntax(x.Args[len(x.Args)-1]))
			}
		case *ast.CompositeLit:
			if typ, ok := x.Type.(*ast.StructType); ok {
				h.Anonymous = append(h.Anonymous, syntax(typ))
			}
		}
		return problem == nil
	})
	h.Queries = compact(h.Queries)
	h.Statuses = compact(h.Statuses)
	h.Anonymous = compact(h.Anonymous)
	return h, problem
}

func actualHTTPWire(t *testing.T) httpWire {
	t.Helper()
	result := httpWire{Statuses: map[string][]string{}, Unavailable: controlUnavailableResponse(t)}
	for service, dir := range map[string]string{"worker": "pkg/mediaworker", "relay": "pkg/relay", "control": "pkg/controlplane", "transport": "internal/privateapi"} {
		paths, err := filepath.Glob(filepath.Join("../..", dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			// The worker WHIP listener is public signaling, not the private API.
			if service == "worker" && filepath.Base(path) == "signaling.go" {
				continue
			}
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			muxNames := map[string]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, rhs := range assign.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok || syntax(call.Fun) != "http.NewServeMux" || i >= len(assign.Lhs) {
						continue
					}
					if id, ok := assign.Lhs[i].(*ast.Ident); ok {
						muxNames[id.Name] = true
					}
				}
				return true
			})

			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncDecl:
					if x.Name.Name == "SignalingHandler" {
						return false
					}
				case *ast.SelectorExpr:
					if syntax(x.X) == "http" && strings.HasPrefix(x.Sel.Name, "Status") {
						result.Statuses[service] = append(result.Statuses[service], x.Sel.Name)
					}
				case *ast.KeyValueExpr:
					if syntax(x.Key) == "Code" {
						if s, ok := text(x.Value); ok {
							result.Codes = append(result.Codes, s)
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Code" && i < len(x.Rhs) {
							if s, ok := text(x.Rhs[i]); ok {
								result.Codes = append(result.Codes, s)
							}
						}
					}
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok || !muxNames[syntax(sel.X)] || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
						return true
					}
					if len(x.Args) != 2 {
						t.Fatalf("unrecognized handler registration in %s", path)
					}
					route, ok := text(x.Args[0])
					if !ok {
						t.Fatalf("dynamic private route in %s", path)
					}
					if route == "/" {
						return true
					}
					body := ast.Node(x.Args[1])
					if fn, ok := body.(*ast.FuncLit); ok {
						body = fn.Body
					}
					h, e := readHandler(service, "github.com/relaishq/relais/"+dir, f, route, body)
					if e != nil {
						t.Fatal(e)
					}
					result.Handlers = append(result.Handlers, h)
				}
				return true
			})
		}
		result.Statuses[service] = compact(result.Statuses[service])
	}
	for _, codes := range []map[string]error{privateapi.CommonErrors, mediaworker.RemoteErrors, relay.RemoteErrors, controlplane.RemoteErrors} {
		for code := range codes {
			result.Codes = append(result.Codes, code)
		}
	}
	result.Codes = compact(result.Codes)
	slices.SortFunc(result.Handlers, func(a, b handlerWire) int { return strings.Compare(a.Service+" "+a.Route, b.Service+" "+b.Route) })
	return result
}
func verifyDecodedRequests(fixtures []endpoint, handlers []handlerWire) error {
	var covered, registered []string
	for _, e := range fixtures {
		if e.Service == "worker" || e.Service == "relay" || e.Service == "control" {
			covered = append(covered, e.Service+" "+e.Route)
		}
	}
	for _, h := range handlers {
		registered = append(registered, h.Service+" "+h.Route)
		for _, e := range fixtures {
			if e.Service != h.Service || e.Route != h.Route {
				continue
			}
			expected := ""
			if ty := reflect.TypeOf(e.Request); ty != nil && ty.Kind() == reflect.Struct {
				expected = ty.PkgPath() + "." + ty.Name()
			}
			if h.RequestType != expected {
				return fmt.Errorf("%s %s decodes %s, fixture describes %s", h.Service, h.Route, h.RequestType, expected)
			}
		}
	}
	return endpointCoverage(covered, registered)
}
func TestHTTPHandlerRequestTypeTracksDecoder(t *testing.T) {
	source := `package mediaworker; import "github.com/relaishq/relais/internal/privateapi"; func handler(){var req CreateRequest; privateapi.Read(w,r,&req)}`
	fixture := []endpoint{{Service: "worker", Route: "POST /sessions", Request: mediaworker.CreateRequest{}}}
	for _, changed := range []bool{false, true} {
		src := source
		if changed {
			src = strings.Replace(src, "var req CreateRequest", "var req ResumeRequest", 1)
		}
		f, err := parser.ParseFile(token.NewFileSet(), "http.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		h, err := readHandler("worker", "github.com/relaishq/relais/pkg/mediaworker", f, "POST /sessions", f)
		if err != nil {
			t.Fatal(err)
		}
		err = verifyDecodedRequests(fixture, []handlerWire{h})
		if (err != nil) != changed {
			t.Fatalf("changed=%v: %v", changed, err)
		}
	}
}

func TestHTTPHandlerWireFactsTrackProtocolEdits(t *testing.T) {
	original := "package controlplane;func handler(){ q:=r.URL.Query().Get(\"worker\");w.WriteHeader(http.StatusServiceUnavailable);writeJSON(w,struct{Ready bool `json:\"ready\"`}{}) }"
	parse := func(src string) handlerWire {
		f, err := parser.ParseFile(token.NewFileSet(), "http.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		h, err := readHandler("control", "github.com/relaishq/relais/pkg/controlplane", f, "GET /status", f)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	before := parse(original)
	for _, mutation := range [][2]string{{`"worker"`, `"to"`}, {"StatusServiceUnavailable", "StatusConflict"}, {`json:"ready"`, `json:"available"`}} {
		changed := strings.Replace(original, mutation[0], mutation[1], 1)
		if changed == original {
			t.Fatal("wire mutation missed")
		}
		if reflect.DeepEqual(before, parse(changed)) {
			t.Fatalf("wire change escaped golden: %v", mutation)
		}
	}
}
