package operatorrehearsal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestImageUsesAL2023CurlProvider(t *testing.T) {
	raw, err := assets.ReadFile("assets/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range strings.Fields(string(raw)) {
		if word == "curl" || word == "--allowerasing" {
			t.Fatal("image must retain the base curl-minimal provider")
		}
	}
}

func TestRehearsalLeavesNftInstallationToSSMRelayBoot(t *testing.T) {
	raw, err := assets.ReadFile("assets/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "RUN ") && strings.Contains(line, "dnf install") && strings.Contains(line, "nftables") {
			t.Fatal("image pre-installs the relay isolation prerequisite")
		}
	}
	if !strings.Contains(string(raw), "RUN ! command -v nft\n") {
		t.Fatal("image must refuse an already installed nft")
	}
	c := fixtureConfig()
	c.RelayTwo = true
	topology, err := Load(os.DirFS("../.."), c)
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{"relay", "relay-two"} {
		var node struct {
			SSM        string   `json:"ssm"`
			Components []string `json:"components"`
		}
		if err := json.Unmarshal([]byte(topology.Hosts[slot].Metadata.Files["/etc/relais/node.json"].Content), &node); err != nil {
			t.Fatal(err)
		}
		if node.SSM != "true" || !slices.Equal(node.Components, []string{"relay"}) {
			t.Fatalf("%s does not select the template's operator relay install: %+v", slot, node)
		}
	}
}

func TestImageBuildFailurePrintsPlainRedactedStreams(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.tar.gz")
	data := []byte("digest-bound image fixture")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	topology := fixtureTopology(t)
	sum := sha256.Sum256(data)
	topology.Config.ArchiveSHA256 = hex.EncodeToString(sum[:])
	layout, err := Layout([]int{0, 1, 2, 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := topology.Secrets["WorkerPassword"]
	// Exercise the real subprocess capture through Boot, without invoking Docker.
	// Each stream exceeds the host-protocol budget, as package transaction logs can.
	script := "#!/bin/sh\nplain=false\nfor arg do\ncase \"$arg\" in --progress=plain) plain=true;; --quiet|-q) exit 2;; esac\ndone\n" +
		"if [ \"$plain\" != true ]; then printf 'plain progress required\\n' >&2; exit 2; fi\n" +
		"printf '%s\\n' " + quote(strings.Repeat("package transaction stdout\n", 2000)) + " " + quote("image stdout "+secret) + "\n" +
		"printf '%s\\n' " + quote(strings.Repeat("package transaction stderr\n", 2000)) + " " + quote("image stderr "+secret) + " " + quote("dnf transaction failed: package conflict") + " >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var log bytes.Buffer
	d, err := NewDeployment(topology, filepath.Join(dir, "work"), archive, layout, LocalRunner{}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Boot(context.Background()); err == nil || err.Error() != "container-step-failed:image" {
		t.Fatal(err)
	}
	for _, expected := range []string{"container step image failed", "stdout:\npackage transaction stdout", "image stdout [secret]", "stderr:\npackage transaction stderr", "image stderr [secret]", "dnf transaction failed: package conflict"} {
		if !strings.Contains(log.String(), expected) {
			t.Fatalf("image failure lost diagnostic %q (captured %d bytes)", expected, log.Len())
		}
	}
	if strings.Contains(log.String(), secret) {
		t.Fatal("image failure disclosed a secret")
	}
}
