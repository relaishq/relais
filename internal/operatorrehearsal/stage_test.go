package operatorrehearsal

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/inventory"
)

// Model a systemd-mounted /tmp visible to exec but distinct from Docker's
// archive-copy root. Run the unchanged template downloader using file:// only.
type stageFilesystemRunner struct {
	roots    map[string]string
	copied   []string
	dropCopy bool
}

func (r *stageFilesystemRunner) Run(_ context.Context, c Command) CommandResult {
	if c.Label == "stage-copy" {
		id, target, ok := strings.Cut(c.Args[3], ":")
		root := r.roots[id]
		if !ok || root == "" {
			return CommandResult{Code: code(1), Stderr: []byte("unknown container")}
		}
		dest := filepath.Join(root, strings.TrimPrefix(target, "/"))
		r.copied = append(r.copied, dest)
		if r.dropCopy {
			return CommandResult{Code: code(0)}
		}
		raw, err := os.ReadFile(c.Args[2])
		if err == nil {
			err = os.WriteFile(dest, raw, 0600)
		}
		if err != nil {
			return CommandResult{Code: code(1), Stderr: []byte(err.Error())}
		}
		return CommandResult{Code: code(0)}
	}
	// docker exec -i CONTAINER sh -eu
	if len(c.Args) < 4 || r.roots[c.Args[3]] == "" {
		return CommandResult{Code: code(1), Stderr: []byte("unknown exec container")}
	}
	root := r.roots[c.Args[3]]
	script := strings.NewReplacer("/tmp/relais-stage-", root+"/exec-tmp/relais-stage-", "/opt/relais", root+"/opt/relais").Replace(string(c.Input))
	command := exec.Command("/bin/sh", "-eu")
	command.Env = []string{"PATH=" + root + "/tools:" + os.Getenv("PATH"), "TMPDIR=" + root + "/exec-tmp"}
	command.Stdin = strings.NewReader(script)
	var out, stderr bytes.Buffer
	command.Stdout, command.Stderr = &out, &stderr
	status := 0
	if err := command.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			return CommandResult{Err: err}
		}
		status = exit.ExitCode()
	}
	return CommandResult{Code: code(status), Stdout: out.Bytes(), Stderr: stderr.Bytes()}
}
func stageArchive(t *testing.T, version string) []byte {
	t.Helper()
	var data bytes.Buffer
	zipped := gzip.NewWriter(&data)
	archive := tar.NewWriter(zipped)
	base := "relais_" + version + "_linux_amd64"
	for _, name := range []string{"relais-relay", "relais-worker", "relais-control", "relais-signal", "relais-demo", "relais-load", "relais-compat", "compatibility.json", "start.sh", "stop.sh", "LICENSE", "NOTICE", "README.md"} {
		raw := []byte("fixture " + name + " " + version + "\n")
		// Compatibility policy itself is covered by the pinned/product suites.
		// This fixture exercises the template's path, digest and install boundaries.
		if name == "relais-compat" {
			raw = []byte("#!/bin/sh\nexit 0\n")
		}
		if err := archive.WriteHeader(&tar.Header{Name: base + "/" + name, Mode: 0755, Size: int64(len(raw)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func stageFilesystemFixture(t *testing.T) (*rehearsal, *stageFilesystemRunner, *bytes.Buffer) {
	t.Helper()
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("local file downloader requires curl")
	}
	topology := fixtureTopology(t)
	dir := t.TempDir()
	assets := filepath.Join(dir, "verified assets")
	if err = os.Mkdir(assets, 0700); err != nil {
		t.Fatal(err)
	}
	runner := &stageFilesystemRunner{roots: map[string]string{}}
	var diagnostics bytes.Buffer
	d := &Deployment{Topology: topology, Runner: runner, Diagnostics: &diagnostics, containerIDs: fixtureContainerIDs()}
	r := &rehearsal{c: RehearseConfig{Assets: assets}, topology: topology, deployment: d, runner: runner, diagnostics: &diagnostics}
	for _, version := range []string{"0.3.0", "0.3.1"} {
		raw := stageArchive(t, version)
		r.pins = append(r.pins, inventory.Pin{Version: version, ArchiveSHA256: hash(raw)})
		if err = os.WriteFile(filepath.Join(assets, "relais_"+version+"_linux_amd64.tar.gz"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, slot := range Slots {
		root := filepath.Join(dir, slot)
		runner.roots[d.containerIDs[slot]] = root
		for _, path := range []string{"tmp", "exec-tmp", "opt/relais/boot", "tools"} {
			if err = os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
				t.Fatal(err)
			}
		}
		downloader := topology.Common.Files["/opt/relais/boot/download.sh"].Content
		if err = os.WriteFile(filepath.Join(root, "opt/relais/boot/download.sh"), []byte(downloader), 0755); err != nil {
			t.Fatal(err)
		}
		// A regression must never turn this offline test into a release download.
		guardedCurl := "#!/bin/sh\nfor arg do last=$arg; done\ncase $last in file://*) exec " + quote(curl) + " \"$@\";; *) echo 'network refused by fixture' >&2; exit 99;; esac\n"
		if err = os.WriteFile(filepath.Join(root, "tools/curl"), []byte(guardedCurl), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return r, runner, &diagnostics
}
func TestStageReadsArchiveInExecutionFilesystem(t *testing.T) {
	r, runner, diagnostics := stageFilesystemFixture(t)
	if err := r.stage(context.Background()); err != nil {
		t.Fatal("stage archive is invisible to downloader", err, runner.copied, diagnostics.String())
	}
	for _, slot := range Slots {
		root := runner.roots[r.deployment.containerIDs[slot]]
		for _, pin := range r.pins {
			target := filepath.Join(root, "opt/relais/preview", pin.Version)
			receipt, err := os.ReadFile(filepath.Join(target, "archive.sha256"))
			if err != nil || strings.TrimSpace(string(receipt)) != pin.ArchiveSHA256 {
				t.Fatal("staged receipt differs", slot, pin.Version, err)
			}
			binary, err := os.ReadFile(filepath.Join(target, "relais-worker"))
			if err != nil || !strings.Contains(string(binary), pin.Version) {
				t.Fatal("wrong staged release", slot, pin.Version, err)
			}
		}
	}
}
func TestStageRejectsArchiveDigestMismatch(t *testing.T) {
	for _, version := range []string{"0.3.0", "0.3.1"} {
		t.Run(version, func(t *testing.T) {
			r, runner, diagnostics := stageFilesystemFixture(t)
			archive := filepath.Join(r.c.Assets, "relais_"+version+"_linux_amd64.tar.gz")
			if err := os.WriteFile(archive, []byte("changed archive"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := r.stage(context.Background()); err == nil {
				t.Fatal("unverified archive installed")
			}
			if !strings.Contains(diagnostics.String(), "artifact digest mismatch; installation refused") {
				t.Fatal(diagnostics.String())
			}
			root := runner.roots[r.deployment.containerIDs[Slots[0]]]
			if _, err := os.Stat(filepath.Join(root, "opt/relais/preview", version, "archive.sha256")); !os.IsNotExist(err) {
				t.Fatal("digest failure left a successful stage receipt", err)
			}
		})
	}
}
func TestStageFailureNamesCopyAndReadPaths(t *testing.T) {
	r, runner, diagnostics := stageFilesystemFixture(t)
	runner.dropCopy = true
	if err := r.stage(context.Background()); err == nil {
		t.Fatal("missing archive installed")
	}
	for _, want := range []string{"host_source=", "copy_destination=", "download_source=file://", "/opt/relais/relais-stage-0.3.0.tar.gz", r.c.Assets} {
		if !strings.Contains(diagnostics.String(), want) {
			t.Fatalf("missing staging path %q: %s", want, diagnostics.String())
		}
	}
}
