package operatorrehearsal

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/relaishq/relais/internal/operatorgates/artifacts"
)

// This profile is deliberately a fixed published pair. A new release needs a
// reviewed profile and a fresh calibration set; it cannot reuse this evidence.
const (
	ReleaseACommit = "9243cf5736d81d139da5e4bd4bc828df3fb79c50"
	ReleaseBCommit = "24daff7755299b864bdf8082a9e182749394f5d4"
	ReleaseASHA256 = "40551e4f1f795878e73d2aaf87b387f544786fe4c7ca9321679e0c5e80e5e3d5"
	ReleaseBSHA256 = "6adb967415a18255e71bd64b5a184adc0c608ed09a5b507774a98f0f2ef8ad85"
)

func hash(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func fileHash(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type archiveReader struct {
	artifacts.Reader
	archives map[string][]byte
}

func (r *archiveReader) Download(ctx context.Context, tag string, asset artifacts.Asset) (artifacts.Download, error) {
	d, err := r.Reader.Download(ctx, tag, asset)
	if err == nil && asset.Name != "checksums.txt" {
		r.archives[asset.Name] = d.Bytes
	}
	return d, err
}

// PrepareReleases is the only token-consuming stage. Reader scope and redirect
// handling belong to artifacts.PublicReader. No child process receives the token.
func PrepareReleases(ctx context.Context, directory, token string) error {
	r := &archiveReader{Reader: artifacts.NewPublicReader(nil, token), archives: map[string][]byte{}}
	plan, err := artifacts.PlanArtifacts(ctx, r, artifacts.Request{From: artifacts.PinRequest{Version: "0.3.4", ArchiveSHA256: ReleaseASHA256}, To: artifacts.PinRequest{Version: "0.3.5", ArchiveSHA256: ReleaseBSHA256}, Arch: "x86_64"})
	if err != nil {
		return err
	}
	if err = validatePair(plan); err != nil {
		return err
	}
	if err = os.Mkdir(directory, 0700); err != nil {
		return err
	}
	for _, pin := range []artifacts.Pin{plan.From, plan.To} {
		if err = os.WriteFile(filepath.Join(directory, pin.Archive), r.archives[pin.Archive], 0600); err != nil {
			return err
		}
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "plan.json"), data, 0600)
}
func validatePair(p artifacts.Plan) error {
	if p.Version != 1 || p.Arch != "x86_64" || p.From.Version != "0.3.4" || p.To.Version != "0.3.5" || p.From.Commit != ReleaseACommit || p.To.Commit != ReleaseBCommit || p.From.ArchiveSHA256 != ReleaseASHA256 || p.To.ArchiveSHA256 != ReleaseBSHA256 || p.From.Archive != "relais_0.3.4_linux_amd64.tar.gz" || p.To.Archive != "relais_0.3.5_linux_amd64.tar.gz" {
		return &artifacts.Refusal{Code: "release-profile-differs", Detail: "planned release pair differs from the pinned rehearsal pair"}
	}
	return nil
}
func readReleases(directory string) (artifacts.Plan, error) {
	var p artifacts.Plan
	raw, err := os.ReadFile(filepath.Join(directory, "plan.json"))
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if err = validatePair(p); err != nil {
		return p, err
	}
	for _, pin := range []artifacts.Pin{p.From, p.To} {
		raw, e := os.ReadFile(filepath.Join(directory, pin.Archive))
		if e != nil {
			return p, e
		}
		if hash(raw) != pin.ArchiveSHA256 {
			return p, errors.New("release-digest-differs")
		}
		manifest, e := artifacts.ArchiveManifest(raw, "relais_"+pin.Version+"_linux_amd64")
		if e != nil || hash(manifest) != pin.ManifestSHA256 {
			return p, errors.New("release-manifest-differs")
		}
	}
	return p, nil
}

// extractLoad reads only the known member of the already verified A archive.
func extractLoad(archive, output string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer z.Close()
	t := tar.NewReader(z)
	for {
		h, e := t.Next()
		if e != nil {
			return e
		}
		if h.Name != "relais_0.3.4_linux_amd64/relais-load" {
			continue
		}
		out, e := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if e != nil {
			return e
		}
		_, e = io.CopyN(out, t, h.Size)
		return errors.Join(e, out.Close())
	}
}
