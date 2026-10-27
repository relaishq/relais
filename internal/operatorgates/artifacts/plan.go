package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/relaishq/relais/internal/releasecompat"
)

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type snapshot struct {
	tag     string
	release Release
	assets  map[string]Asset
}

func readError(err error) error {
	var r *Refusal
	if errors.As(err, &r) && r.Code == "read-rate-limited" {
		return err
	}
	return refuse("read-failed", "release read failed; result unknown")
}

func releaseView(ctx context.Context, r Reader, tag string) (Release, error) {
	reply, err := r.Release(ctx, tag)
	if err != nil {
		return Release{}, readError(err)
	}
	if reply.State == "not-found" {
		if reply.Tag != tag {
			return Release{}, refuse("reply-mismatch", "missing reply names another tag")
		}
		return Release{}, refuse("release-missing", "no published release for this tag")
	}
	v := reply.Release
	if reply.State != "found" || !commitPattern.MatchString(v.TagCommit) {
		return v, refuse("reply-invalid", "malformed release metadata")
	}
	if v.Tag != tag {
		return v, refuse("reply-mismatch", "release reply names another tag")
	}
	if v.Draft {
		return v, refuse("release-draft", "draft releases are not published artifacts")
	}
	if v.Prerelease {
		return v, refuse("release-prerelease", "prerelease releases are not stable artifacts")
	}
	return v, nil
}

func assetList(ctx context.Context, r Reader, tag string) (map[string]Asset, error) {
	assets, seen := map[string]Asset{}, map[string]bool{}
	token := ""
	for range MaxAssetPages {
		p, err := r.Assets(ctx, tag, token)
		if err != nil {
			return nil, readError(err)
		}
		if p.Tag != tag || p.Token != token {
			return nil, refuse("reply-mismatch", "asset page names another tag or page")
		}
		for _, a := range p.Assets {
			if a.ID <= 0 || a.ID > 1<<53-1 || a.Size < 0 || a.Size > 1<<53-1 || a.State != "uploaded" && a.State != "open" || a.Digest != "" && (!strings.HasPrefix(a.Digest, "sha256:") || !shaPattern.MatchString(strings.TrimPrefix(a.Digest, "sha256:"))) {
				return nil, refuse("reply-invalid", "malformed asset page")
			}
			if old, ok := assets[a.Name]; ok && old != a {
				return nil, refuse("asset-ambiguous", "two assets claim one name")
			}
			assets[a.Name] = a
		}
		if p.Complete {
			if p.NextToken != "" {
				return nil, refuse("pagination-invalid", "final page names a next page")
			}
			return assets, nil
		}
		if p.NextToken == "" || seen[p.NextToken] {
			return nil, refuse("pagination-invalid", "incomplete listing without a fresh next page")
		}
		seen[p.NextToken], token = true, p.NextToken
	}
	return nil, refuse("pagination-invalid", "asset listing exceeds page limit")
}

func required(assets map[string]Asset, name string, limit int64) (Asset, error) {
	a, ok := assets[name]
	if !ok {
		return a, refuse("asset-missing", "release lacks required asset")
	}
	if a.State != "uploaded" {
		return a, refuse("asset-incomplete", "asset upload is unfinished")
	}
	if a.Size > limit {
		return a, refuse("asset-size", "asset exceeds limit")
	}
	return a, nil
}

func download(ctx context.Context, r Reader, tag string, a Asset) ([]byte, error) {
	reply, err := r.Download(ctx, tag, a)
	if err != nil {
		return nil, readError(err)
	}
	if reply.Tag != tag || reply.AssetID != a.ID {
		return nil, refuse("reply-mismatch", "download answers another asset")
	}
	if int64(len(reply.Bytes)) != a.Size {
		return nil, refuse("asset-size", "download length differs from metadata")
	}
	return reply.Bytes, nil
}

func tagFile(ctx context.Context, r Reader, commit, path string, value any) error {
	f, err := r.File(ctx, commit, path)
	if err != nil {
		return readError(err)
	}
	if f.Commit != commit || f.Path != path {
		return refuse("reply-mismatch", "file answers another commit or path")
	}
	if f.State == "not-found" {
		return refuse("manifest-missing", "release commit lacks compatibility file")
	}
	if f.State != "found" || len(f.Bytes) > MaxManifestBytes {
		return refuse("reply-invalid", "file is not a bounded reply")
	}
	return decodeManifest(f.Bytes, value)
}
func decodeManifest(data []byte, v any) error {
	if !validText(data) {
		return refuse("manifest-invalid", "invalid JSON text")
	}
	if err := releasecompat.Decode(data, v); err != nil {
		r := &Refusal{Code: "manifest-invalid", Detail: "invalid compatibility JSON"}
		var c *releasecompat.Refusal
		if errors.As(err, &c) {
			r.Compat = c.Code
		}
		return r
	}
	return nil
}

// ExpectedManifest reuses the native manifest and policy contracts, stamping
// only identity and reviewed policy fields, as the release producer does.
func ExpectedManifest(template releasecompat.Manifest, policy releasecompat.Policy, version, commit, arch string) (releasecompat.Manifest, error) {
	stamp := func(v, self string) string {
		if v == "self" {
			return self
		}
		return v
	}
	peers := func(self string) []string {
		out := make([]string, len(policy.TestedPeers))
		for i, v := range policy.TestedPeers {
			out[i] = stamp(v, self)
		}
		return out
	}
	if template.Version != "0.0.0-dev" || template.Commit != "" || template.OS != "any" || template.Arch != "any" || template.OldestLivePeer != stamp(policy.OldestLivePeer, "0.0.0-dev") || template.OldestRollback != stamp(policy.OldestRollback, "0.0.0-dev") || !slices.Equal(template.TestedPeers, peers("0.0.0-dev")) {
		return template, refuse("manifest-policy-drift", "checked-in manifest differs from its policy")
	}
	template.Version, template.Commit, template.OS, template.Arch = version, commit, "linux", arch
	template.OldestLivePeer, template.OldestRollback, template.TestedPeers = stamp(policy.OldestLivePeer, version), stamp(policy.OldestRollback, version), peers(version)
	return template, nil
}

func resolve(ctx context.Context, r Reader, request PinRequest, arch string) (pin Pin, manifest releasecompat.Manifest, snap snapshot, err error) {
	tag, goarch := "v"+request.Version, arch
	if goarch == "x86_64" {
		goarch = "amd64"
	}
	archive := "relais_" + request.Version + "_linux_" + goarch + ".tar.gz"
	v, err := releaseView(ctx, r, tag)
	if err != nil {
		return pin, manifest, snap, err
	}
	assets, err := assetList(ctx, r, tag)
	if err != nil {
		return pin, manifest, snap, err
	}
	checks, err := required(assets, "checksums.txt", MaxChecksumsBytes)
	if err != nil {
		return pin, manifest, snap, err
	}
	asset, err := required(assets, archive, MaxArchiveBytes)
	if err != nil {
		return pin, manifest, snap, err
	}
	checkBytes, err := download(ctx, r, tag, checks)
	if err != nil {
		return pin, manifest, snap, err
	}
	if checks.Digest != "" && checks.Digest != "sha256:"+digest(checkBytes) {
		return pin, manifest, snap, refuse("asset-digest-mismatch", "checksum asset digest differs")
	}
	entries, err := Checksums(checkBytes, request.Version)
	if err != nil {
		return pin, manifest, snap, err
	}
	// Sort only unordered checksum entries; never reorder host effects.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a, e := required(assets, name, MaxArchiveBytes)
		if e != nil {
			return pin, manifest, snap, e
		}
		if a.Digest != "" && a.Digest != "sha256:"+entries[name] {
			return pin, manifest, snap, refuse("asset-digest-mismatch", "release digest differs from checksums")
		}
	}
	if entries[archive] != request.ArchiveSHA256 {
		return pin, manifest, snap, refuse("planned-digest-mismatch", "checksums differ from planned archive")
	}
	archiveBytes, err := download(ctx, r, tag, asset)
	if err != nil {
		return pin, manifest, snap, err
	}
	if digest(archiveBytes) != entries[archive] {
		return pin, manifest, snap, refuse("archive-digest-mismatch", "archive differs from checksums and plan")
	}
	manifestBytes, err := ArchiveManifest(archiveBytes, strings.TrimSuffix(archive, ".tar.gz"))
	if err != nil {
		return pin, manifest, snap, err
	}
	if err = decodeManifest(manifestBytes, &manifest); err != nil {
		return pin, manifest, snap, err
	}
	if err = manifest.Validate(); err != nil {
		return pin, manifest, snap, &Refusal{Code: "manifest-invalid", Detail: err.Error()}
	}
	if manifest.OS != "linux" || manifest.Arch != goarch {
		return pin, manifest, snap, refuse("architecture-mismatch", "archive manifest platform differs")
	}
	if manifest.Version != request.Version || manifest.Commit != v.TagCommit {
		return pin, manifest, snap, refuse("manifest-identity", "archive identity differs from tag")
	}
	var template releasecompat.Manifest
	var policy releasecompat.Policy
	if err = tagFile(ctx, r, v.TagCommit, "scripts/release/compatibility.json", &template); err != nil {
		return pin, manifest, snap, err
	}
	if err = tagFile(ctx, r, v.TagCommit, "scripts/release/compatibility-policy.json", &policy); err != nil {
		return pin, manifest, snap, err
	}
	expected, err := ExpectedManifest(template, policy, request.Version, v.TagCommit, goarch)
	if err != nil {
		return pin, manifest, snap, err
	}
	// JSON equality preserves null versus empty slices in the native decoder.
	a, _ := json.Marshal(manifest)
	b, _ := json.Marshal(expected)
	if string(a) != string(b) {
		return pin, manifest, snap, refuse("manifest-policy-drift", "archive is not the stamped tag template")
	}
	return Pin{asset, request.Version, tag, v.TagCommit, arch, archive, entries[archive], digest(checkBytes), digest(manifestBytes)}, manifest, snapshot{tag, v, assets}, nil
}

func PlanArtifacts(ctx context.Context, reader Reader, request Request) (Plan, error) {
	var plan Plan
	for _, pin := range []PinRequest{request.From, request.To} {
		if _, err := releasecompat.Compare(pin.Version, pin.Version); err != nil {
			return plan, refuse("version-invalid", "expected a release version without v prefix")
		}
		if !stableVersion.MatchString(pin.Version) {
			return plan, refuse("version-unstable", "expected a stable release")
		}
		if c, _ := releasecompat.Compare(pin.Version, "0.2.0"); c <= 0 {
			return plan, refuse("version-unsupported", "release must be newer than 0.2.0")
		}
	}
	c, _ := releasecompat.Compare(request.From.Version, request.To.Version)
	if c == 0 {
		return plan, refuse("version-same", "upgrade and rollback require distinct releases")
	}
	if c > 0 {
		return plan, refuse("version-order", "A must be older than B")
	}
	if request.Arch != "arm64" && request.Arch != "x86_64" {
		return plan, refuse("architecture-invalid", "architecture must be arm64 or x86_64")
	}
	for _, pin := range []PinRequest{request.From, request.To} {
		if !shaPattern.MatchString(pin.ArchiveSHA256) {
			return plan, refuse("planned-digest-invalid", "planned archive SHA-256 must be lowercase hex")
		}
	}
	a, am, as, err := resolve(ctx, reader, request.From, request.Arch)
	if err != nil {
		return plan, err
	}
	b, bm, bs, err := resolve(ctx, reader, request.To, request.Arch)
	if err != nil {
		return plan, err
	}
	for _, step := range []struct {
		running, target releasecompat.Manifest
		op              string
		runningSHA      string
	}{{am, bm, "upgrade", a.ArchiveSHA256}, {bm, am, "rollback", b.ArchiveSHA256}} {
		pool := releasecompat.Pool{Schema: 1, Complete: true, TargetWorkerProfile: "legacy"}
		for _, participant := range [][2]string{{"w1", "worker"}, {"w2", "worker"}, {"relay", "relay"}, {"relay-standby", "relay"}, {"control", "control"}, {"signal", "signal"}} {
			name, role := participant[0], participant[1]
			profile, agents, defaults := "default", []string{}, false
			if role == "worker" {
				profile, agents, defaults = "legacy", []string{"echo"}, true
			}
			pool.Participants = append(pool.Participants, releasecompat.Participant{Name: name, Role: role, Profile: profile, Version: step.running.Version, Commit: step.running.Commit, ArchiveSHA256: step.runningSHA, Agents: agents, DefaultBudgets: defaults, Manifest: step.running})
		}
		verdict := releasecompat.Preflight(pool, step.target, step.op)
		if !verdict.Allowed {
			return plan, &Refusal{Code: step.op + "-refused", Detail: verdict.Refusal.Reason, Compat: verdict.Refusal.Code}
		}
	}
	for _, s := range []snapshot{as, bs} {
		v, err := releaseView(ctx, reader, s.tag)
		if err != nil {
			return plan, err
		}
		assets, err := assetList(ctx, reader, s.tag)
		if err != nil {
			return plan, err
		}
		if v != s.release || !reflect.DeepEqual(assets, s.assets) {
			return plan, refuse("release-changed", "release metadata or assets changed while planning")
		}
	}
	plan = Plan{Version: 1, Arch: request.Arch, From: a, To: b}
	for i, pin := range []Pin{a, b, a} {
		plan.Steps = append(plan.Steps, Step{[]string{"install", "upgrade", "rollback"}[i], Parameters{pin.Version, request.Arch, pin.ArchiveSHA256}})
	}
	return plan, nil
}

func VerifyStageDigest(ctx context.Context, r Reader, pin Pin) error {
	data, err := download(ctx, r, pin.Tag, pin.Asset)
	if err != nil {
		return err
	}
	if pin.Asset.Name != pin.Archive || digest(data) != pin.ArchiveSHA256 {
		return refuse("archive-digest-mismatch", "archive changed before stage")
	}
	return nil
}
