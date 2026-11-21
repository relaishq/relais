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
	ReleaseACommit = "ae66240481edfd96b099ddb9aca787a59a5dbcac"
	ReleaseBCommit = "9332472e781483cf37c2b51b111dd09ae9b6a92d"
	ReleaseASHA256 = "0bb1f48afdb32d8aa5552d9da3706be8099a1ecf16e06b8a2c1913fed60bb866"
	ReleaseBSHA256 = "7222731226752ae1761f1b6134cbebf5035a144de9f1e818866511b8105b7a01"
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
	plan, err := artifacts.PlanArtifacts(ctx, r, artifacts.Request{From: artifacts.PinRequest{Version: "0.3.2", ArchiveSHA256: ReleaseASHA256}, To: artifacts.PinRequest{Version: "0.3.3", ArchiveSHA256: ReleaseBSHA256}, Arch: "x86_64"})
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
	if p.Version != 1 || p.Arch != "x86_64" || p.From.Version != "0.3.2" || p.To.Version != "0.3.3" || p.From.Commit != ReleaseACommit || p.To.Commit != ReleaseBCommit || p.From.ArchiveSHA256 != ReleaseASHA256 || p.To.ArchiveSHA256 != ReleaseBSHA256 || p.From.Archive != "relais_0.3.2_linux_amd64.tar.gz" || p.To.Archive != "relais_0.3.3_linux_amd64.tar.gz" {
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
		if h.Name != "relais_0.3.2_linux_amd64/relais-load" {
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
