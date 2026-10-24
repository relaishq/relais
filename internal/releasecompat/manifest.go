// Package releasecompat checks release artifacts and offline rollout plans.
// It never contacts services, reads credentials, or changes running processes.
package releasecompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const ManifestVersion = 2

// Format separates decoding from continuing an existing format's writes/lifecycle.
type Format struct {
	Reads     []int `json:"reads"`
	Writes    []int `json:"writes"`
	Continues []int `json:"continues"`
}

type Profile struct {
	Formats map[string]Format `json:"formats"`
}

type Agent struct {
	Name     string   `json:"name"`
	Identity string   `json:"identity"`
	Limits   string   `json:"limits"`
	Reads    []uint32 `json:"reads"`
	Writes   []uint32 `json:"writes"`
}

type Script struct {
	SHA             string `json:"sha"`
	UnchangedTuples string `json:"unchanged_tuples,omitempty"`
}

type Manifest struct {
	Schema         int                           `json:"manifest_version"`
	Version        string                        `json:"version"`
	Commit         string                        `json:"commit"`
	OS             string                        `json:"os"`
	Arch           string                        `json:"arch"`
	ArchiveLayout  int                           `json:"archive_layout"`
	StoreContract  int                           `json:"store_contract"`
	Scripts        map[string]Script             `json:"scripts"`
	HTTPContract   int                           `json:"private_http_contract"`
	KeyContract    int                           `json:"redis_key_contract"`
	KeyGoldens     map[string]string             `json:"redis_key_golden_sha256"`
	UDPContract    int                           `json:"relay_udp_contract"`
	UDPGolden      string                        `json:"relay_udp_golden_sha256"`
	HTTPGolden     string                        `json:"private_http_golden_sha256"`
	Roles          map[string]map[string]Profile `json:"roles"`
	Agents         []Agent                       `json:"agents"`
	OldestLivePeer string                        `json:"oldest_live_peer"`
	OldestRollback string                        `json:"oldest_rollback"`
	TestedPeers    []string                      `json:"tested_peers"`
}

// Policy is reviewed with the release-pair evidence. "self" is stamped at build.
type Policy struct {
	Comment        string   `json:"_comment,omitempty"`
	OldestLivePeer string   `json:"oldest_live_peer"`
	OldestRollback string   `json:"oldest_rollback"`
	TestedPeers    []string `json:"tested_peers"`
}

// Refusal is a definite offline result; it never means an operation was attempted.
type Refusal struct {
	Code        string `json:"code"`
	Reason      string `json:"reason"`
	Participant string `json:"participant,omitempty"`
	Format      string `json:"format,omitempty"`
	Step        int    `json:"step,omitempty"`
}

func (e *Refusal) Error() string          { return e.Code + ": " + e.Reason }
func refuse(code, reason string) *Refusal { return &Refusal{Code: code, Reason: reason} }

// Decode rejects unknown fields, duplicate keys and trailing input, including
// pre-manifest/partial inputs. Offline inputs must be bounded by their caller.
func Decode(data []byte, value any) error {
	if len(data) > 16<<20 {
		return refuse("invalid_input", "JSON exceeds 16 MiB")
	}
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return refuse("invalid_input", err.Error())
	}
	switch value.(type) {
	case *Manifest, *Pool:
		var raw any
		if err := json.Unmarshal(data, &raw); err == nil && newerManifest(raw) {
			return refuse("unsupported_manifest", "unsupported newer manifest; use the target release's relais-compat")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return refuse("invalid_input", err.Error())
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return refuse("invalid_input", "expected one JSON value")
	}
	return nil
}
func newerManifest(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		if schema, ok := v["manifest_version"].(float64); ok && schema > ManifestVersion {
			return true
		}
		for _, child := range v {
			if newerManifest(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if newerManifest(child) {
				return true
			}
		}
	}
	return false
}
func uniqueJSON(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			keys := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				if !ok {
					return fmt.Errorf("invalid object key")
				}
				if keys[key] {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				keys[key] = true
				if e = uniqueJSON(dec); e != nil {
					return e
				}
			}
		case '[':
			for dec.More() {
				if e := uniqueJSON(dec); e != nil {
					return e
				}
			}
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = dec.Token()
	}
	return err
}

var releasePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var scriptPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Compare uses semantic release ordering, including numeric prerelease identifiers.
func Compare(a, b string) (int, error) {
	x, y := releasePattern.FindStringSubmatch(a), releasePattern.FindStringSubmatch(b)
	if x == nil || y == nil {
		return 0, refuse("unknown_version", "expected a version without v prefix")
	}
	for _, v := range []string{a, b} {
		m := releasePattern.FindStringSubmatch(v)
		for _, p := range strings.Split(m[4], ".") {
			if p == "" && m[4] != "" {
				return 0, refuse("unknown_version", "empty prerelease identifier")
			}
			if numeric(p) && len(p) > 1 && p[0] == '0' {
				return 0, refuse("unknown_version", "leading zero in prerelease")
			}
		}
	}
	for i := 1; i <= 3; i++ {
		if c := compareNumber(x[i], y[i]); c != 0 {
			return c, nil
		}
	}
	if x[4] == y[4] {
		return 0, nil
	}
	if x[4] == "" {
		return 1, nil
	}
	if y[4] == "" {
		return -1, nil
	}
	xp, yp := strings.Split(x[4], "."), strings.Split(y[4], ".")
	for i := 0; i < min(len(xp), len(yp)); i++ {
		if xp[i] == yp[i] {
			continue
		}
		xn, yn := numeric(xp[i]), numeric(yp[i])
		if xn && yn {
			return compareNumber(xp[i], yp[i]), nil
		}
		if xn {
			return -1, nil
		}
		if yn {
			return 1, nil
		}
		return strings.Compare(xp[i], yp[i]), nil
	}
	return compareNumber(strconv.Itoa(len(xp)), strconv.Itoa(len(yp))), nil
}
func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func compareNumber(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// Unpublished build/CI artifacts use a placeholder version. Their release
// floors still describe real releases, and they are never live rollout targets.
func developmentVersion(v string) bool {
	return v == "0.0.0-dev" || strings.HasPrefix(v, "0.0.0-dev+")
}

func (m Manifest) Validate() error {
	if m.Schema == 0 {
		return refuse("missing_manifest", "missing compatibility manifest; pre-manifest releases cannot roll live")
	}
	if m.Schema > ManifestVersion {
		return refuse("unsupported_manifest", "unsupported newer manifest; use the target release's relais-compat")
	}
	if m.Schema < ManifestVersion {
		return refuse("unsupported_manifest", "unsupported older manifest; rebuild both releases with the complete contracts")
	}
	if m.ArchiveLayout > 1 {
		return refuse("unsupported_manifest", "unsupported newer manifest archive layout")
	}
	if _, err := Compare(m.Version, m.Version); err != nil {
		return err
	}
	if m.Commit != "" && !commitPattern.MatchString(m.Commit) {
		return refuse("invalid_manifest", "commit must be a full SHA")
	}
	if m.OS != "any" && m.OS != "linux" && m.OS != "darwin" {
		return refuse("invalid_manifest", "unsupported OS")
	}
	if m.Arch != "any" && m.Arch != "amd64" && m.Arch != "arm64" {
		return refuse("invalid_manifest", "unsupported architecture")
	}
	if m.ArchiveLayout != 1 || m.StoreContract < 1 || m.HTTPContract < 1 || !digestPattern.MatchString(m.HTTPGolden) || len(m.Scripts) == 0 || m.KeyContract < 1 || len(m.KeyGoldens) == 0 || m.UDPContract < 1 || !digestPattern.MatchString(m.UDPGolden) {
		return refuse("invalid_manifest", "missing archive or contract metadata")
	}
	for _, name := range []string{"sessionstore", "framecache"} {
		if !digestPattern.MatchString(m.KeyGoldens[name]) {
			return refuse("invalid_manifest", "missing Redis key golden: "+name)
		}
	}

	for n, s := range m.Scripts {
		if n == "" || !scriptPattern.MatchString(s.SHA) {
			return refuse("invalid_manifest", "invalid script golden")
		}
	}
	for _, floor := range []string{m.OldestLivePeer, m.OldestRollback} {
		c, err := Compare(floor, m.Version)
		if err != nil {
			return err
		}
		if c > 0 && !developmentVersion(m.Version) {
			return refuse("invalid_manifest", "release floor exceeds own version")
		}
	}
	if len(m.TestedPeers) == 0 || !slices.Contains(m.TestedPeers, m.Version) {
		return refuse("invalid_manifest", "tested peers must include self")
	}
	for _, peer := range m.TestedPeers {
		if _, err := Compare(peer, peer); err != nil {
			return err
		}
	}
	if len(m.Roles) != 4 {
		return refuse("unsupported_manifest", "unsupported manifest platform roles")
	}
	for _, role := range []string{"worker", "relay", "control", "signal"} {
		profiles := m.Roles[role]
		if len(profiles) == 0 {
			return refuse("invalid_manifest", "missing role "+role)
		}
		for name, p := range profiles {
			required := []string{"private_http"}
			if role != "signal" {
				required = append(required, "store_contract", "redis_keys")
			}
			if role == "worker" || role == "relay" {
				required = append(required, "relay_udp")
			}
			if role == "worker" || role == "control" {
				required = append(required, "snapshot", "component", "metadata", "encryption_header", "legacy_encryption_header")
			}
			if len(p.Formats) != len(required) {
				return refuse("unsupported_manifest", "unsupported newer manifest format domains")
			}
			for _, domain := range required {
				if _, ok := p.Formats[domain]; !ok {
					return refuse("invalid_manifest", "missing format domain "+domain)
				}
			}
			if name == "" || len(p.Formats) == 0 {
				return refuse("invalid_manifest", "empty profile")
			}
			for domain, f := range p.Formats {
				if domain == "" || len(f.Reads) == 0 {
					return refuse("invalid_manifest", "empty format capability")
				}
				for _, v := range append(slices.Clone(f.Reads), append(slices.Clone(f.Writes), f.Continues...)...) {
					if v < 1 {
						return refuse("invalid_manifest", "invalid format version")
					}
				}
				for _, v := range f.Writes {
					if !slices.Contains(f.Reads, v) || !slices.Contains(f.Continues, v) {
						return refuse("invalid_manifest", "writer cannot continue its own format")
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, a := range m.Agents {
		if a.Name == "" || a.Identity == "" || a.Limits == "" || len(a.Reads) == 0 || len(a.Writes) == 0 || seen[a.Name] {
			return refuse("invalid_manifest", "invalid or duplicate agent contract")
		}
		seen[a.Name] = true
		for _, v := range a.Writes {
			if v == 0 || !slices.Contains(a.Reads, v) {
				return refuse("invalid_manifest", "agent cannot read its own schema")
			}
		}
	}
	return nil
}

// CheckEvolution binds fixture changes to deliberate contract revisions.
func CheckEvolution(old, next Manifest) error {
	if old.HTTPContract > next.HTTPContract || old.StoreContract > next.StoreContract || old.KeyContract > next.KeyContract || old.UDPContract > next.UDPContract {
		return refuse("contract_regression", "contract version decreased")
	}
	if old.HTTPGolden != next.HTTPGolden && old.HTTPContract == next.HTTPContract {
		return refuse("http_contract_drift", "private HTTP fixture changed without a contract version bump")
	}
	if old.UDPContract == next.UDPContract && old.UDPGolden != next.UDPGolden {
		return refuse("udp_contract_drift", "UDP fixtures changed without a contract version bump")
	}
	if old.KeyContract == next.KeyContract && !equalGoldens(old.KeyGoldens, next.KeyGoldens) {
		return refuse("key_contract_drift", "Redis key fixtures changed without a contract version bump")
	}

	if old.StoreContract == next.StoreContract {
		for name := range old.Scripts {
			if _, ok := next.Scripts[name]; !ok {
				return refuse("store_contract_drift", "removed Lua script requires a contract version bump: "+name)
			}
		}
		for name, s := range next.Scripts {
			prev, ok := old.Scripts[name]
			if !ok || prev.SHA != s.SHA {
				if strings.TrimSpace(s.UnchangedTuples) == "" || s.UnchangedTuples == prev.UnchangedTuples {
					return refuse("store_contract_drift", "changed Lua SHA: bump the contract version, or record a reviewed note that persisted state and wire behaviour are unchanged: "+name)
				}
			}
		}
	}
	return nil
}

func equalGoldens(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, v := range a {
		if b[name] != v {
			return false
		}
	}
	return true
}
