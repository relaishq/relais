package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// pairRelease is one verified, extracted release archive for the host platform.
type pairRelease struct {
	Label, Version, Commit, OS, Arch string
	Archive, SHA256, Dir             string
	// Manifest is the archive's compatibility.json, embedded unchanged in each
	// inventory so a newer target CLI sees the exact installed manifest.
	Manifest json.RawMessage
}

func (r *pairRelease) String() string { return r.Label + "=" + r.Version }
func (r *pairRelease) binary(name string) string {
	return filepath.Join(r.Dir, name)
}

var pairArchiveName = regexp.MustCompile(`^relais_([0-9][0-9A-Za-z.+-]*)_(linux|darwin)_(amd64|arm64)\.tar\.gz$`)

// The archive members build-archives.sh writes. Anything else is refused.
var pairArchiveFiles = map[string]bool{
	"relais-relay": true, "relais-worker": true, "relais-control": true, "relais-signal": true,
	"relais-demo": true, "relais-load": true, "relais-compat": true, "compatibility.json": true,
	"LICENSE": true, "NOTICE": true, "README.md": true, "start.sh": true, "stop.sh": true,
}

const pairArchiveLimit = 1 << 30

// pairArchiveDir names an extracted archive inside the run directory. The
// workflow excludes these binaries from uploaded evidence by this prefix.
func pairArchiveDir(label, version string) string { return "archive-" + label + "-" + version }

// findPairArchive accepts an archive path, or a directory that holds exactly
// one archive for this host platform.
func findPairArchive(input, goos, goarch string) (string, error) {
	info, err := os.Stat(input)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return input, nil
	}
	matches, err := filepath.Glob(filepath.Join(input, "relais_*_"+goos+"_"+goarch+".tar.gz"))
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%s: expected exactly one relais_<version>_%s_%s.tar.gz, found %d", input, goos, goarch, len(matches))
	}
	return matches[0], nil
}

func fileSHA256(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyPairDigest binds the archive to at least one independent digest: the
// operator's expected value, the checksums.txt shipped beside it, or both.
func verifyPairDigest(archive, digest, expected string) error {
	if expected != "" && !strings.EqualFold(expected, digest) {
		return fmt.Errorf("%s: SHA-256 %s does not match expected %s", archive, digest, expected)
	}
	list := filepath.Join(filepath.Dir(archive), "checksums.txt")
	f, err := os.Open(list)
	if errors.Is(err, os.ErrNotExist) {
		if expected == "" {
			return fmt.Errorf("%s: no checksums.txt beside the archive and no expected SHA-256", archive)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	found := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != filepath.Base(archive) {
			continue
		}
		if found {
			return fmt.Errorf("%s: duplicate checksums.txt entry", archive)
		}
		found = true
		if !strings.EqualFold(fields[0], digest) {
			return fmt.Errorf("%s: SHA-256 %s does not match checksums.txt %s", archive, digest, fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s: checksums.txt has no entry for the archive", archive)
	}
	return nil
}

// extractPairArchive writes only the expected regular files of one top-level
// directory. Links, devices, nested paths and duplicates are refused.
func extractPairArchive(archive, base, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	reader := tar.NewReader(gz)
	seen := map[string]bool{}
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(header.Name)
		if header.Typeflag == tar.TypeDir && name == base {
			continue
		}
		dir, file := path.Split(name)
		if header.Typeflag != tar.TypeReg || path.Clean(dir) != base || !pairArchiveFiles[file] || seen[file] {
			return fmt.Errorf("%s: unexpected archive member %q", archive, header.Name)
		}
		seen[file] = true
		total += header.Size
		if header.Size < 0 || total > pairArchiveLimit {
			return fmt.Errorf("%s: archive exceeds %d bytes", archive, pairArchiveLimit)
		}
		mode := os.FileMode(0600)
		if header.FileInfo().Mode()&0100 != 0 {
			mode = 0700
		}
		out, err := os.OpenFile(filepath.Join(dest, file), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, reader, header.Size)
		if err := errors.Join(copyErr, out.Close()); err != nil {
			return err
		}
	}
	for file := range pairArchiveFiles {
		if !seen[file] {
			return fmt.Errorf("%s: archive lacks %s", archive, file)
		}
	}
	return nil
}

// openPairRelease verifies one side of the pair before any process starts:
// digest, layout, manifest identity and platform, then the archive CLI's own
// artifact check of every binary's linked version and commit.
func openPairRelease(ctx context.Context, label, input, expected, work string) (*pairRelease, error) {
	archive, err := findPairArchive(input, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", label, err)
	}
	match := pairArchiveName.FindStringSubmatch(filepath.Base(archive))
	if match == nil {
		return nil, fmt.Errorf("release %s: %s is not a relais_<version>_<os>_<arch>.tar.gz archive", label, archive)
	}
	if match[2] != runtime.GOOS || match[3] != runtime.GOARCH {
		return nil, fmt.Errorf("release %s: archive platform %s/%s differs from host %s/%s", label, match[2], match[3], runtime.GOOS, runtime.GOARCH)
	}
	digest, err := fileSHA256(archive)
	if err != nil {
		return nil, err
	}
	if err := verifyPairDigest(archive, digest, expected); err != nil {
		return nil, fmt.Errorf("release %s: %w", label, err)
	}
	base := strings.TrimSuffix(filepath.Base(archive), ".tar.gz")
	dir := filepath.Join(work, pairArchiveDir(label, match[1]))
	if err := extractPairArchive(archive, base, dir); err != nil {
		return nil, fmt.Errorf("release %s: %w", label, err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "compatibility.json"))
	if err != nil {
		return nil, err
	}
	var identity struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
	}
	if err := json.Unmarshal(manifest, &identity); err != nil {
		return nil, fmt.Errorf("release %s: manifest: %w", label, err)
	}
	if identity.Version != match[1] || identity.OS != match[2] || identity.Arch != match[3] || identity.Commit == "" {
		return nil, fmt.Errorf("release %s: manifest identity %s %s/%s differs from archive name %s", label, identity.Version, identity.OS, identity.Arch, filepath.Base(archive))
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(check, filepath.Join(dir, "relais-compat"), "artifact", "--manifest", filepath.Join(dir, "compatibility.json"), "--binary-dir", dir, "--version", identity.Version, "--os", identity.OS, "--arch", identity.Arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("release %s: archive artifact check: %w: %s", label, err, strings.TrimSpace(string(out)))
	}
	return &pairRelease{Label: label, Version: identity.Version, Commit: identity.Commit, OS: identity.OS, Arch: identity.Arch, Archive: archive, SHA256: digest, Dir: dir, Manifest: manifest}, nil
}
