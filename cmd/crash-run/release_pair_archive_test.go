package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type pairMember struct {
	name     string
	typeflag byte
	link     string
}

func writePairArchive(t *testing.T, dir, base string, members []pairMember) string {
	t.Helper()
	name := filepath.Join(dir, base+".tar.gz")
	f, err := os.Create(name)
	require.NoError(t, err)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: base + "/", Typeflag: tar.TypeDir, Mode: 0755}))
	for _, m := range members {
		h := &tar.Header{Name: m.name, Typeflag: m.typeflag, Mode: 0755, Linkname: m.link}
		body := "x"
		if m.typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		require.NoError(t, tw.WriteHeader(h))
		if m.typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, f.Close())
	return name
}

func completePairMembers(base string) []pairMember {
	var out []pairMember
	for file := range pairArchiveFiles {
		out = append(out, pairMember{name: base + "/" + file, typeflag: tar.TypeReg})
	}
	return out
}

func TestExtractPairArchiveAcceptsOnlyTheReleaseLayout(t *testing.T) {
	base := "relais_0.3.1_linux_amd64"
	dir := t.TempDir()
	good := writePairArchive(t, dir, base, completePairMembers(base))
	require.NoError(t, extractPairArchive(good, base, filepath.Join(dir, "out")))
	info, err := os.Stat(filepath.Join(dir, "out", "relais-worker"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())

	for name, extra := range map[string]pairMember{
		"traversal": {name: base + "/../relais-worker", typeflag: tar.TypeReg},
		"nested":    {name: base + "/bin/relais-worker", typeflag: tar.TypeReg},
		"symlink":   {name: base + "/relais-extra", typeflag: tar.TypeSymlink, link: "/etc/passwd"},
		"unknown":   {name: base + "/payload", typeflag: tar.TypeReg},
		"duplicate": {name: base + "/relais-worker", typeflag: tar.TypeReg},
		"other top": {name: "elsewhere/relais-worker", typeflag: tar.TypeReg},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			archive := writePairArchive(t, dir, base, append(completePairMembers(base), extra))
			require.ErrorContains(t, extractPairArchive(archive, base, filepath.Join(dir, "out")), "unexpected archive member")
		})
	}
	incomplete := completePairMembers(base)
	for i, m := range incomplete {
		if strings.HasSuffix(m.name, "/relais-compat") {
			incomplete = append(incomplete[:i], incomplete[i+1:]...)
			break
		}
	}
	dir = t.TempDir()
	require.ErrorContains(t, extractPairArchive(writePairArchive(t, dir, base, incomplete), base, filepath.Join(dir, "out")), "lacks relais-compat")
}

func TestVerifyPairDigestNeedsAnIndependentMatch(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "relais_0.3.1_linux_amd64.tar.gz")
	require.NoError(t, os.WriteFile(archive, []byte("archive"), 0600))
	digest, err := fileSHA256(archive)
	require.NoError(t, err)
	require.ErrorContains(t, verifyPairDigest(archive, digest, ""), "no checksums.txt")
	require.NoError(t, verifyPairDigest(archive, digest, strings.ToUpper(digest)))
	require.ErrorContains(t, verifyPairDigest(archive, digest, strings.Repeat("0", 64)), "does not match expected")
	list := filepath.Join(dir, "checksums.txt")
	require.NoError(t, os.WriteFile(list, []byte(strings.Repeat("0", 64)+"  other.tar.gz\n"), 0600))
	require.ErrorContains(t, verifyPairDigest(archive, digest, digest), "no entry")
	require.NoError(t, os.WriteFile(list, []byte(strings.Repeat("0", 64)+"  relais_0.3.1_linux_amd64.tar.gz\n"), 0600))
	require.ErrorContains(t, verifyPairDigest(archive, digest, ""), "does not match checksums.txt")
	require.NoError(t, os.WriteFile(list, []byte(digest+"  relais_0.3.1_linux_amd64.tar.gz\n"), 0600))
	require.NoError(t, verifyPairDigest(archive, digest, ""))
	require.NoError(t, os.WriteFile(list, []byte(digest+"  relais_0.3.1_linux_amd64.tar.gz\n"+digest+"  relais_0.3.1_linux_amd64.tar.gz\n"), 0600))
	require.ErrorContains(t, verifyPairDigest(archive, digest, ""), "duplicate")
}

func TestFindPairArchiveSelectsTheHostArchive(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"relais_0.3.1_linux_amd64.tar.gz", "relais_0.3.1_darwin_arm64.tar.gz", "checksums.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0600))
	}
	found, err := findPairArchive(dir, "linux", "amd64")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "relais_0.3.1_linux_amd64.tar.gz"), found)
	_, err = findPairArchive(dir, "linux", "arm64")
	require.ErrorContains(t, err, "found 0")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "relais_0.3.2_linux_amd64.tar.gz"), nil, 0600))
	_, err = findPairArchive(dir, "linux", "amd64")
	require.ErrorContains(t, err, "found 2")
}

// The uploaded evidence keeps the verdict, inventories and logs, and drops
// only extracted archive binaries. Patterns are matched per path segment, as
// the artifact action does for single-star globs.
func TestReleasePairWorkflowKeepsTheVerdictAndDropsBinaries(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/release-pair.yml")
	require.NoError(t, err)
	var excludes []string
	for _, line := range strings.Split(string(data), "\n") {
		if pattern, ok := strings.CutPrefix(strings.TrimSpace(line), "!bin/"); ok {
			excludes = append(excludes, strings.TrimSuffix(pattern, "/**"))
		}
	}
	require.NotEmpty(t, excludes)
	excluded := func(name string) bool {
		for _, pattern := range excludes {
			if ok, err := path.Match(pattern, name); err == nil && ok {
				return true
			}
		}
		return false
	}
	for _, kept := range []string{"release-pair-123/release-pair-verdict.json", "release-pair-123/caller-reports.json", "release-pair-123/preflight-01-upgrade-0.3.1-verdict.json", "release-pair-123/03-worker-0-0.3.0.log"} {
		require.False(t, excluded(kept), kept)
	}
	for _, label := range []string{"A", "B"} {
		require.True(t, excluded("release-pair-123/"+pairArchiveDir(label, "0.3.1")), label)
	}
}
