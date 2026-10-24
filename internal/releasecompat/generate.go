package releasecompat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/relaishq/relais/internal/privateapi"
	"github.com/relaishq/relais/internal/relayleg"
	"github.com/relaishq/relais/pkg/sessionstore"
)

type ScriptGolden struct {
	Contract int               `json:"contract_version"`
	Scripts  map[string]Script `json:"scripts"`
}

func parse(root, path string) (*ast.File, error) {
	return parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(path)), nil, 0)
}
func number(expr ast.Expr, file *ast.File) (int, error) {
	if v, ok := expr.(*ast.BasicLit); ok && v.Kind == token.INT {
		n, err := strconv.ParseInt(v.Value, 0, 64)
		return int(n), err
	}
	if id, ok := expr.(*ast.Ident); ok {
		for _, d := range file.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				v, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range v.Names {
					if n.Name == id.Name && i < len(v.Values) {
						return number(v.Values[i], file)
					}
				}
			}
		}
	}
	return 0, fmt.Errorf("unrecognized version expression; update the version extractor")
}
func constant(root, path, name string) (int, error) {
	f, err := parse(root, path)
	if err != nil {
		return 0, err
	}
	return number(ast.NewIdent(name), f)
}
func function(root, path, name string) (*ast.File, *ast.FuncDecl, error) {
	f, err := parse(root, path)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return f, fn, nil
		}
	}
	return nil, nil, fmt.Errorf("missing function %s", name)
}

// literalVersion handles the existing v10/header literals without changing their
// runtime code. It reads only the specific encoded field/header, never hashes source.
func literalVersion(root, path, fnName, kind string) (int, error) {
	f, fn, err := function(root, path, fnName)
	if err != nil {
		return 0, err
	}
	var values []int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		var value ast.Expr
		switch x := n.(type) {
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && kind == "Version" && id.Name == kind {
				value = x.Value
			}
		case *ast.CompositeLit:
			if kind == "header" {
				if a, ok := x.Type.(*ast.ArrayType); ok {
					if id, ok := a.Elt.(*ast.Ident); ok && id.Name == "byte" && len(x.Elts) > 0 {
						value = x.Elts[0]
					}
				}
			}
		case *ast.BinaryExpr:
			if selector, ok := x.X.(*ast.SelectorExpr); ok && x.Op == token.NEQ {
				if kind == "Format" && selector.Sel.Name == kind || kind == "read-Version" && selector.Sel.Name == "Version" {
					value = x.Y
				}
			}
			if index, ok := x.X.(*ast.IndexExpr); ok && x.Op == token.NEQ && strings.HasPrefix(kind, "read-") {
				if id, ok := index.X.(*ast.Ident); ok && kind == "read-"+id.Name {
					if at, err := number(index.Index, f); err == nil && at == 0 {
						value = x.Y
					}
				}
			}
		}
		if value != nil {
			if v, e := number(value, f); e == nil {
				values = append(values, v)
			}
		}
		return true
	})
	if len(values) != 1 {
		return 0, fmt.Errorf("ambiguous %s version in %s:%s", kind, path, fnName)
	}
	return values[0], nil
}

func CheckStoreGolden(root string) (ScriptGolden, error) {
	var g ScriptGolden
	data, err := os.ReadFile(filepath.Join(root, "pkg/sessionstore/testdata/script-contract.json"))
	if err != nil {
		return g, err
	}
	if err = Decode(data, &g); err != nil {
		return g, err
	}
	if g.Contract != sessionstore.ScriptContractVersion {
		return g, fmt.Errorf("store golden/version mismatch")
	}
	actual, err := LuaScripts(root)
	if err != nil {
		return g, err
	}
	if len(actual) != len(g.Scripts) {
		return g, fmt.Errorf("lua script inventory changed; bump the contract version, or record a reviewed note that persisted state and wire behaviour are unchanged")
	}
	for name, sha := range actual {
		if g.Scripts[name].SHA != sha {
			return g, fmt.Errorf("lua SHA drift for %s; bump the contract version, or record a reviewed note that persisted state and wire behaviour are unchanged", name)
		}
	}
	return g, nil
}

// Generate reads production version values, script goldens, HTTP fixtures and
// the reviewed pair policy. The repository template uses any/any, empty commit.
func Generate(root, version, commit, osName, arch string) (Manifest, error) {
	m := Manifest{Schema: ManifestVersion, Version: version, Commit: commit, OS: osName, Arch: arch, ArchiveLayout: 1, StoreContract: sessionstore.ScriptContractVersion, HTTPContract: privateapi.ContractVersion}
	g, err := CheckStoreGolden(root)
	if err != nil {
		return m, err
	}
	m.Scripts = g.Scripts
	m.KeyContract = sessionstore.KeyContractVersion
	m.UDPContract = relayleg.ContractVersion
	m.KeyGoldens = map[string]string{}
	for name, path := range map[string]string{
		"sessionstore": fmt.Sprintf("pkg/sessionstore/testdata/key-contract-v%d.json", m.KeyContract),
		"framecache":   fmt.Sprintf("pkg/framecache/testdata/key-contract-v%d.json", m.KeyContract),
	} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return m, err
		}
		sum := sha256.Sum256(data)
		m.KeyGoldens[name] = hex.EncodeToString(sum[:])
	}
	udpData, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("internal/relayleg/testdata/contract-v%d.json", m.UDPContract)))
	if err != nil {
		return m, err
	}
	udpSum := sha256.Sum256(udpData)
	m.UDPGolden = hex.EncodeToString(udpSum[:])

	httpData, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("internal/privateapi/testdata/contract-v%d.json", privateapi.ContractVersion)))
	if err != nil {
		return m, err
	}
	sum := sha256.Sum256(httpData)
	m.HTTPGolden = hex.EncodeToString(sum[:])
	var p Policy
	data, err := os.ReadFile(filepath.Join(root, "scripts/release/compatibility-policy.json"))
	if err != nil {
		return m, err
	}
	if err = Decode(data, &p); err != nil {
		return m, err
	}
	stamp := func(v string) string {
		if v == "self" {
			return version
		}
		return v
	}
	m.OldestLivePeer, m.OldestRollback = stamp(p.OldestLivePeer), stamp(p.OldestRollback)
	for _, v := range p.TestedPeers {
		m.TestedPeers = append(m.TestedPeers, stamp(v))
	}
	legacy, err := constant(root, "pkg/mediaworker/state.go", "sessionStateVersion")
	if err != nil {
		return m, err
	}
	v2, err := constant(root, "pkg/mediaworker/agent_v2.go", "v2SessionStateVersion")
	if err != nil {
		return m, err
	}
	control, err := constant(root, "pkg/mediaworker/agent_v2.go", "controlSessionStateVersion")
	if err != nil {
		return m, err
	}
	split, err := literalVersion(root, "pkg/mediaworker/component_snapshot.go", "encodeComponentSnapshot", "Version")
	if err != nil {
		return m, err
	}
	component, err := literalVersion(root, "internal/componentcodec/binary.go", "Marshal", "header")
	if err != nil {
		return m, err
	}
	metadata, err := literalVersion(root, "internal/componentcodec/auth.go", "Marshal", "Format")
	if err != nil {
		return m, err
	}
	header, err := literalVersion(root, "internal/componentcodec/auth.go", "Seal", "header")
	if err != nil {
		return m, err
	}
	legacyHeader, err := literalVersion(root, "pkg/sessionstore/redis.go", "PutState", "header")
	if err != nil {
		return m, err
	}
	// Readers must agree with their encoder values. A future bridge with more
	// than one accepted version requires an explicit extractor/capability update.
	for _, site := range []struct {
		path, function, kind string
		want                 int
	}{
		{"pkg/mediaworker/component_snapshot.go", "decodeComponentSnapshot", "read-Version", split},
		{"internal/componentcodec/binary.go", "Unmarshal", "read-data", component},
		{"internal/componentcodec/auth.go", "Open", "read-sealed", header},
		{"pkg/sessionstore/redis.go", "openLegacyState", "read-blob", legacyHeader},
		{"pkg/sessionstore/components.go", "validateComponentMetadata", "Format", metadata},
	} {
		actual, err := literalVersion(root, site.path, site.function, site.kind)
		if err != nil {
			return m, err
		}
		if actual != site.want {
			return m, fmt.Errorf("format reader/writer version mismatch at %s:%s", site.path, site.function)
		}
	}
	f, fn, err := function(root, "pkg/mediaworker/handover.go", "validateSnapshotVersion")
	if err != nil {
		return m, err
	}
	var reads []int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.NEQ {
			if id, ok := b.X.(*ast.Ident); ok && id.Name == "version" {
				if v, e := number(b.Y, f); e == nil {
					reads = append(reads, v)
				} else if id, ok := b.Y.(*ast.Ident); ok {
					versions := map[string]int{"sessionStateVersion": legacy, "v2SessionStateVersion": v2, "controlSessionStateVersion": control}
					if v, ok := versions[id.Name]; ok {
						reads = append(reads, v)
					}
				}
			}
		}
		return true
	})
	reads = append(reads, split)
	slices.Sort(reads)
	reads = slices.Compact(reads)
	state := func(writes []int) Format {
		return Format{Reads: slices.Clone(reads), Writes: writes, Continues: []int{legacy, v2, control, split}}
	}
	common := func() map[string]Format {
		return map[string]Format{"private_http": {Reads: []int{m.HTTPContract}, Writes: []int{m.HTTPContract}, Continues: []int{m.HTTPContract}}}
	}
	platform := func() map[string]Format {
		fs := common()
		fs["redis_keys"] = Format{Reads: []int{m.KeyContract}, Writes: []int{m.KeyContract}, Continues: []int{m.KeyContract}}
		fs["store_contract"] = Format{Reads: []int{m.StoreContract}, Writes: []int{m.StoreContract}, Continues: []int{m.StoreContract}}
		return fs
	}
	worker := func(enabled bool) Profile {
		fs := platform()
		fs["relay_udp"] = Format{Reads: []int{m.UDPContract}, Writes: []int{m.UDPContract}, Continues: []int{m.UDPContract}}
		writes := []int{legacy, v2, control}
		if enabled {
			writes = append(writes, split)
		}
		fs["snapshot"] = state(writes)
		fs["encryption_header"] = Format{Reads: []int{header}, Writes: []int{header}, Continues: []int{header}}
		fs["legacy_encryption_header"] = Format{Reads: []int{legacyHeader}, Writes: []int{legacyHeader}, Continues: []int{legacyHeader}}
		for name, v := range map[string]int{"component": component, "metadata": metadata} {
			var w []int
			if enabled {
				w = []int{v}
			}
			fs[name] = Format{Reads: []int{v}, Writes: w, Continues: []int{v}}
		}
		return Profile{Formats: fs}
	}
	cp := worker(false)
	cp.Formats["snapshot"] = state(nil)
	relayProfile := Profile{Formats: platform()}
	relayProfile.Formats["relay_udp"] = Format{Reads: []int{m.UDPContract}, Writes: []int{m.UDPContract}, Continues: []int{m.UDPContract}}
	delete(cp.Formats, "relay_udp")
	m.Roles = map[string]map[string]Profile{"worker": {"legacy": worker(false), "split": worker(true)}, "control": {"default": cp}, "relay": {"default": relayProfile}, "signal": {"default": {Formats: common()}}}
	// Echo's app marker is separate from the host snapshot version.
	echo, err := constant(root, "pkg/agent/agent.go", "echoStateVersion")
	if err != nil {
		return m, err
	}
	m.Agents = []Agent{{Name: "echo", Identity: "builtin-echo", Limits: "builtin-echo", Reads: []uint32{uint32(echo)}, Writes: []uint32{uint32(echo)}}}
	return m, m.Validate()
}

func JSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	return append(data, '\n'), err
}
