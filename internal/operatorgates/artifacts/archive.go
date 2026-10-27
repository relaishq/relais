package artifacts

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var members = map[string]bool{
	"relais-relay": true, "relais-worker": true, "relais-control": true, "relais-signal": true,
	"relais-demo": true, "relais-load": true, "relais-compat": true, "start.sh": true, "stop.sh": true,
	"compatibility.json": false, "LICENSE": false, "NOTICE": false, "README.md": false,
}
var ownerField = regexp.MustCompile(`^[A-Za-z0-9._-]{1,31}\x00+$`)
var checksumLine = regexp.MustCompile(`^([0-9a-f]{64})  (\S+)$`)

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func zeros(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}
func octal(data []byte) (int64, error) {
	for _, b := range data {
		if b < '0' || b > '7' {
			return 0, fmt.Errorf("invalid octal")
		}
	}
	return strconv.ParseInt(string(data), 8, 64)
}

// ArchiveManifest checks the release producer's exact GNU header layout. Go's
// archive/tar intentionally accepts PAX, links and other layouts this contract
// refuses. No extraction or executable invocation is needed by this planner.
func ArchiveManifest(archive []byte, base string) ([]byte, error) {
	fail := func() ([]byte, error) {
		return nil, refuse("archive-invalid", "archive is not the release producer's exact member layout")
	}
	z, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fail()
	}
	defer z.Close()
	r := &io.LimitedReader{R: z, N: (1 << 30) + 1}
	seen := map[string]bool{}
	var manifest []byte
	for entry := 0; ; entry++ {
		var h [512]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return fail()
		}
		if zeros(h[:]) {
			var count int64
			buffer := make([]byte, 32<<10)
			for {
				n, e := r.Read(buffer)
				count += int64(n)
				if !zeros(buffer[:n]) {
					return fail()
				}
				if e == io.EOF {
					break
				}
				if e != nil {
					return fail()
				}
			}
			if count < 512 || r.N == 0 || len(seen) != len(members) {
				return fail()
			}
			return manifest, nil
		}
		var sum int64
		for i, b := range h {
			if i >= 148 && i < 156 {
				sum += 32
			} else {
				sum += int64(b)
			}
		}
		checksum, err := octal(h[148:154])
		if err != nil || h[154] != 0 || h[155] != ' ' || checksum != sum || !bytes.Equal(h[257:265], []byte{'u', 's', 't', 'a', 'r', ' ', ' ', 0}) || !zeros(h[157:257]) || !zeros(h[329:]) || !ownerField.Match(h[265:297]) || !ownerField.Match(h[297:329]) {
			return fail()
		}
		for _, field := range [][2]int{{108, 116}, {116, 124}, {124, 136}, {136, 148}} {
			if _, err := octal(h[field[0] : field[1]-1]); err != nil || h[field[1]-1] != 0 {
				return fail()
			}
		}
		size, _ := octal(h[124:135])
		name := strings.TrimRight(string(h[:100]), "\x00")
		mode := string(h[100:108])
		if entry == 0 {
			if h[156] != '5' || name != base+"/" || mode != "0000755\x00" || size != 0 {
				return fail()
			}
			continue
		}
		member := strings.TrimPrefix(name, base+"/")
		executable, ok := members[member]
		expectedMode := "0000644\x00"
		if executable {
			expectedMode = "0000755\x00"
		}
		if h[156] != '0' || !strings.HasPrefix(name, base+"/") || !ok || seen[member] || mode != expectedMode {
			return fail()
		}
		seen[member] = true
		if member == "compatibility.json" {
			if size > MaxManifestBytes {
				return fail()
			}
			manifest = make([]byte, size)
			if _, err := io.ReadFull(r, manifest); err != nil {
				return fail()
			}
		} else if _, err := io.CopyN(io.Discard, r, size); err != nil {
			return fail()
		}
		if _, err := io.CopyN(io.Discard, r, (512-size%512)%512); err != nil {
			return fail()
		}
	}
}

func Checksums(data []byte, version string) (map[string]string, error) {
	expected := map[string]bool{}
	for _, os := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			expected["relais_"+version+"_"+os+"_"+arch+".tar.gz"] = true
		}
	}
	fail := func() (map[string]string, error) {
		return nil, refuse("checksums-invalid", "checksums.txt must list every published archive exactly once")
	}
	if !utf8.Valid(data) {
		return fail()
	}
	rows := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	entries := map[string]string{}
	for _, line := range rows {
		m := checksumLine.FindStringSubmatch(line)
		if m == nil || !expected[m[2]] || entries[m[2]] != "" {
			return fail()
		}
		entries[m[2]] = m[1]
	}
	if len(entries) != 4 {
		return fail()
	}
	return entries, nil
}
