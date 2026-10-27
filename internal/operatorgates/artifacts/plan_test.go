package artifacts

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/releasecompat"
)

func rawJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func header(name string, size int, mode string, kind byte) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], mode+"\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], "00000000000\x00")
	h[156] = kind
	copy(h[257:], "ustar  \x00")
	copy(h[265:], "runner")
	copy(h[297:], "runner")
	seal(h)
	return h
}
func seal(h []byte) {
	copy(h[148:], "        ")
	sum := 0
	for _, b := range h {
		sum += int(b)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
}
func tarBytes(base string, manifest []byte) []byte {
	out := header(base+"/", 0, "0000755", '5')
	names := []string{"relais-relay", "relais-worker", "relais-control", "relais-signal", "relais-demo", "relais-load", "relais-compat", "compatibility.json", "LICENSE", "NOTICE", "README.md", "start.sh", "stop.sh"}
	sort.Strings(names)
	for _, name := range names {
		body := []byte("binary")
		if name == "compatibility.json" {
			body = manifest
		}
		mode := "0000755"
		switch name {
		case "compatibility.json", "LICENSE", "NOTICE", "README.md":
			mode = "0000644"
		}
		out = append(out, header(base+"/"+name, len(body), mode, '0')...)
		out = append(out, body...)
		out = append(out, make([]byte, (512-len(body)%512)%512)...)
	}
	return append(out, make([]byte, 1024)...)
}
func zipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	_, _ = z.Write(data)
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type fakeReader struct {
	releases map[string]ReleaseReply
	assets   map[string][]Asset
	data     map[string][]byte
	files    map[string][]byte
	reads    map[string]int
	hook     func(string)
	page     func(string, string) AssetPage
}

func (r *fakeReader) Release(_ context.Context, tag string) (ReleaseReply, error) {
	r.reads[tag]++
	if r.hook != nil {
		r.hook(tag)
	}
	return r.releases[tag], nil
}
func (r *fakeReader) Assets(_ context.Context, tag, token string) (AssetPage, error) {
	if r.page != nil {
		return r.page(tag, token), nil
	}
	return AssetPage{Tag: tag, Token: token, Assets: r.assets[tag], Complete: true}, nil
}
func (r *fakeReader) Download(_ context.Context, tag string, a Asset) (Download, error) {
	return Download{tag, a.ID, r.data[tag+"/"+a.Name]}, nil
}
func (r *fakeReader) File(_ context.Context, commit, path string) (File, error) {
	return File{"found", commit, path, r.files[commit+"/"+path]}, nil
}

func setup(t *testing.T) (*fakeReader, Request) {
	t.Helper()
	r := &fakeReader{releases: map[string]ReleaseReply{}, assets: map[string][]Asset{}, data: map[string][]byte{}, files: map[string][]byte{}, reads: map[string]int{}}
	templateBytes, err := os.ReadFile("../../../scripts/release/compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	var template releasecompat.Manifest
	if err := releasecompat.Decode(templateBytes, &template); err != nil {
		t.Fatal(err)
	}
	policy := releasecompat.Policy{OldestLivePeer: "0.3.0", OldestRollback: "0.3.0", TestedPeers: []string{"0.3.0", "0.3.1"}}
	template.OldestLivePeer, template.OldestRollback, template.TestedPeers = "0.3.0", "0.3.0", policy.TestedPeers
	templateBytes = rawJSON(t, template)
	request := Request{From: PinRequest{Version: "0.3.0"}, To: PinRequest{Version: "0.3.1"}, Arch: "arm64"}
	for i, version := range []string{"0.3.0", "0.3.1"} {
		commit := strings.Repeat([]string{"a", "b"}[i], 40)
		tag := "v" + version
		manifest, e := ExpectedManifest(template, policy, version, commit, "arm64")
		if e != nil {
			t.Fatal(e)
		}
		archive := "relais_" + version + "_linux_arm64.tar.gz"
		data := zipped(t, tarBytes(strings.TrimSuffix(archive, ".tar.gz"), rawJSON(t, manifest)))
		r.data[tag+"/"+archive] = data
		var lines []string
		for _, os := range []string{"linux", "darwin"} {
			for _, arch := range []string{"amd64", "arm64"} {
				name := "relais_" + version + "_" + os + "_" + arch + ".tar.gz"
				sum := strings.Repeat("c", 64)
				if name == archive {
					sum = digest(data)
				}
				lines = append(lines, sum+"  "+name)
				r.assets[tag] = append(r.assets[tag], Asset{ID: int64(len(r.assets[tag]) + 1), Name: name, Size: int64(len(data)), State: "uploaded", Digest: "sha256:" + sum})
			}
		}
		checks := []byte(strings.Join(lines, "\n") + "\n")
		r.data[tag+"/checksums.txt"] = checks
		r.assets[tag] = append(r.assets[tag], Asset{ID: 5, Name: "checksums.txt", Size: int64(len(checks)), State: "uploaded"})
		r.releases[tag] = ReleaseReply{State: "found", Release: Release{Tag: tag, TagCommit: commit}}
		r.files[commit+"/scripts/release/compatibility.json"] = templateBytes
		r.files[commit+"/scripts/release/compatibility-policy.json"] = rawJSON(t, policy)
		if i == 0 {
			request.From.ArchiveSHA256 = digest(data)
		} else {
			request.To.ArchiveSHA256 = digest(data)
		}
	}
	return r, request
}

func TestReleasePlan(t *testing.T) {
	r, request := setup(t)
	plan, err := PlanArtifacts(context.Background(), r, request)
	if err != nil || len(plan.Steps) != 3 || plan.From.Version != "0.3.0" || plan.To.Version != "0.3.1" || plan.Steps[2].Parameters.ArchiveSHA256 != plan.From.ArchiveSHA256 {
		t.Fatalf("release plan failed: %+v %v", plan, err)
	}
	if r.reads["v0.3.0"] != 2 || r.reads["v0.3.1"] != 2 {
		t.Fatal("release snapshots not both rechecked")
	}
	if err := VerifyStageDigest(context.Background(), r, plan.From); err != nil {
		t.Fatal(err)
	}
	r.data[plan.From.Tag+"/"+plan.From.Archive][0] ^= 1
	if code(VerifyStageDigest(context.Background(), r, plan.From)) != "archive-digest-mismatch" {
		t.Fatal("stage digest not checked")
	}
}

func TestReleaseArchiveMembers(t *testing.T) {
	if _, err := ArchiveManifest(zipped(t, tarBytes("relais_0.3.0_linux_arm64", []byte(`{}`))), "relais_0.3.0_linux_arm64"); err != nil {
		t.Fatalf("NOTICE required: published member layout refused: %v", err)
	}
}

func TestReleaseTagPolicy(t *testing.T) {
	r, q := setup(t)
	_, m, _, err := resolve(context.Background(), r, q.From, q.Arch)
	if err != nil {
		t.Fatal(err)
	}
	m.Agents[0].Identity += "-changed"
	name := "relais_0.3.0_linux_arm64.tar.gz"
	archive := zipped(t, tarBytes(strings.TrimSuffix(name, ".tar.gz"), rawJSON(t, m)))
	r.data["v0.3.0/"+name] = archive
	old := q.From.ArchiveSHA256
	q.From.ArchiveSHA256 = digest(archive)
	r.data["v0.3.0/checksums.txt"] = []byte(strings.ReplaceAll(string(r.data["v0.3.0/checksums.txt"]), old, q.From.ArchiveSHA256))
	for i, a := range r.assets["v0.3.0"] {
		if a.Name == name {
			a.Size = int64(len(archive))
			a.Digest = "sha256:" + q.From.ArchiveSHA256
			r.assets["v0.3.0"][i] = a
		}
	}
	_, err = PlanArtifacts(context.Background(), r, q)
	if code(err) != "manifest-policy-drift" {
		t.Fatalf("tag-policy guard: %v", err)
	}
}

func TestReleaseRefusals(t *testing.T) {
	cases := []struct {
		name, want string
		edit       func(*fakeReader, *Request)
	}{
		{"draft", "release-draft", func(r *fakeReader, _ *Request) {
			v := r.releases["v0.3.0"]
			v.Release.Draft = true
			r.releases["v0.3.0"] = v
		}},
		{"prerelease", "release-prerelease", func(r *fakeReader, _ *Request) {
			v := r.releases["v0.3.0"]
			v.Release.Prerelease = true
			r.releases["v0.3.0"] = v
		}},
		{"missing", "release-missing", func(r *fakeReader, _ *Request) {
			r.releases["v0.3.0"] = ReleaseReply{State: "not-found", Tag: "v0.3.0"}
		}},
		{"planned-digest", "planned-digest-mismatch", func(_ *fakeReader, q *Request) { q.From.ArchiveSHA256 = strings.Repeat("f", 64) }},
		{"archive-bytes", "archive-digest-mismatch", func(r *fakeReader, _ *Request) {
			name := "v0.3.0/relais_0.3.0_linux_arm64.tar.gz"
			r.data[name][0] ^= 1
		}},
		{"all-archives", "asset-incomplete", func(r *fakeReader, _ *Request) { r.assets["v0.3.0"][0].State = "open" }},
		{"release-edit", "release-changed", func(r *fakeReader, _ *Request) {
			r.hook = func(tag string) {
				if tag == "v0.3.0" && r.reads[tag] == 2 {
					r.assets[tag][0].ID = 99
				}
			}
		}},
		{"a-edit-during-b", "release-changed", func(r *fakeReader, _ *Request) {
			r.hook = func(tag string) {
				if tag == "v0.3.1" && r.reads[tag] == 1 {
					r.assets["v0.3.0"][0].ID = 99
				}
			}
		}},
		{"tag-policy", "manifest-policy-drift", func(r *fakeReader, _ *Request) {
			key := strings.Repeat("a", 40) + "/scripts/release/compatibility-policy.json"
			var p releasecompat.Policy
			_ = json.Unmarshal(r.files[key], &p)
			p.TestedPeers = []string{"self"}
			r.files[key] = rawJSON(t, p)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, q := setup(t)
			c.edit(r, &q)
			_, err := PlanArtifacts(context.Background(), r, q)
			if code(err) != c.want {
				t.Fatalf("release guard %s: got %v want %s", c.name, err, c.want)
			}
		})
	}
}

func TestAssetPagination(t *testing.T) {
	r, _ := setup(t)
	r.page = func(tag, token string) AssetPage {
		return AssetPage{Tag: tag, Token: token, Assets: r.assets[tag], Complete: false}
	}
	if _, err := assetList(context.Background(), r, "v0.3.0"); code(err) != "pagination-invalid" {
		t.Fatalf("pagination guard: truncated listing accepted: %v", err)
	}
	r.page = func(tag, token string) AssetPage {
		return AssetPage{Tag: tag, Token: token, Assets: r.assets[tag], Complete: false, NextToken: "1"}
	}
	if _, err := assetList(context.Background(), r, "v0.3.0"); code(err) != "pagination-invalid" {
		t.Fatalf("pagination guard: %v", err)
	}
	r.page = func(tag, token string) AssetPage {
		return AssetPage{Tag: tag, Token: token, Complete: true, NextToken: "2"}
	}
	if _, err := assetList(context.Background(), r, "v0.3.0"); code(err) != "pagination-invalid" {
		t.Fatalf("final page guard: %v", err)
	}
}

func TestStrictArchiveGrammar(t *testing.T) {
	base := "relais_0.3.0_linux_arm64"
	valid := tarBytes(base, []byte(`{"manifest_version":2}`))
	cases := []struct {
		name   string
		offset int
		edit   func([]byte)
	}{
		{"symlink", 512, func(h []byte) { h[156] = '2' }},
		{"nul-type", 512, func(h []byte) { h[156] = 0 }},
		{"global-pax", 0, func(h []byte) { h[156] = 'g' }},
		{"posix-magic", 512, func(h []byte) { copy(h[257:], "ustar\x0000") }},
		{"gnu-prefix", 512, func(h []byte) { h[345] = 'x' }},
		{"device", 512, func(h []byte) { h[329] = '1' }},
		{"link-name", 512, func(h []byte) { copy(h[157:], []byte{0xef, 0xbb, 0xbf}) }},
		{"bom-name", 512, func(h []byte) {
			name := bytes.TrimRight(h[:100], "\x00")
			copy(h[:100], append([]byte{0xef, 0xbb, 0xbf}, name...))
		}},
		{"directory-mode", 0, func(h []byte) { copy(h[100:], "badmode\x00") }},
		{"member-mode", 512, func(h []byte) { copy(h[100:], "0000755\x00") }},
		{"uid", 512, func(h []byte) { copy(h[108:], "      0\x00") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := bytes.Clone(valid)
			h := data[c.offset : c.offset+512]
			c.edit(h)
			seal(h)
			if _, err := ArchiveManifest(zipped(t, data), base); code(err) != "archive-invalid" {
				t.Fatalf("archive guard %s: %v", c.name, err)
			}
		})
	}
	if _, err := ArchiveManifest(zipped(t, valid), base); err != nil {
		t.Fatal(err)
	}
}

func TestNativeManifestContracts(t *testing.T) {
	for _, data := range []string{`{"manifest_version":2,"manifest_version":2}`, `{"manifest_version":2.0}`, `{"manifest_version":2e0}`, `{"agents":[{"reads":[-1]}]}`, `{"agents":[{"reads":[4294967296]}]}`, "\ufeff{}"} {
		var m releasecompat.Manifest
		if err := decodeManifest([]byte(data), &m); err == nil {
			t.Fatalf("native manifest contract accepted %s", data)
		}
	}
	for _, data := range []string{`{"version":"\ud800"}`, `{"version":"\udc00"}`} {
		var m releasecompat.Manifest
		if err := decodeManifest([]byte(data), &m); err == nil {
			t.Fatal("allow-lone-surrogate: malformed Unicode accepted")
		}
	}
	var p releasecompat.Policy
	if err := decodeManifest([]byte(`{"_comment":42}`), &p); err == nil {
		t.Fatal("native policy comment type ignored")
	}
	var m releasecompat.Manifest
	if err := decodeManifest([]byte(`{"roles":{"worker":{"legacy":{"formats":{"snapshot":{"reads":[1],"continues":[1]}}}}}}`), &m); err != nil || m.Roles["worker"]["legacy"].Formats["snapshot"].Writes != nil {
		t.Fatalf("omitted nullable writes: %v", err)
	}
}

func TestNativeCompatibilityPolicy(t *testing.T) {
	for _, scenario := range []string{"tested-peer", "rollback-boundary"} {
		t.Run(scenario, func(t *testing.T) {
			r, q := setup(t)
			_, am, _, err := resolve(context.Background(), r, q.From, q.Arch)
			if err != nil {
				t.Fatal(err)
			}
			_, bm, _, err := resolve(context.Background(), r, q.To, q.Arch)
			if err != nil {
				t.Fatal(err)
			}
			target, running, op, want := bm, am, "upgrade", "unsupported_pair"
			if scenario == "tested-peer" {
				target.TestedPeers = []string{"0.3.1"}
			} else {
				target, running, op, want = am, bm, "rollback", "rollback_boundary"
				running.OldestRollback = "0.3.1"
			}
			pool := releasecompat.Pool{Schema: 1, Complete: true, TargetWorkerProfile: "legacy"}
			for _, role := range []string{"worker", "relay", "control", "signal"} {
				profile, agents, defaults := "default", []string{}, false
				if role == "worker" {
					profile, agents, defaults = "legacy", []string{"echo"}, true
				}
				pool.Participants = append(pool.Participants, releasecompat.Participant{Name: role, Role: role, Profile: profile, Version: running.Version, Commit: running.Commit, ArchiveSHA256: q.From.ArchiveSHA256, Agents: agents, DefaultBudgets: defaults, Manifest: running})
			}
			v := releasecompat.Preflight(pool, target, op)
			if v.Allowed || v.Refusal == nil || v.Refusal.Code != want {
				t.Fatalf("native compatibility policy %s: %+v", scenario, v)
			}
		})
	}
}
