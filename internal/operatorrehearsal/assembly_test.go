package operatorrehearsal

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/operator"
)

type assemblyRunner struct {
	commands []Command
	fail     bool
}

func (r *assemblyRunner) Run(_ context.Context, c Command) CommandResult {
	r.commands = append(r.commands, c)
	code := 0
	if r.fail {
		code = 1
	}
	return CommandResult{Code: &code, Stdout: []byte("stdout secret-value"), Stderr: []byte("stderr secret-value")}
}
func TestStageUsesVerifiedFilesAndTemplateDownloader(t *testing.T) {
	run := &assemblyRunner{}
	topology := &Topology{Secrets: map[string]string{}}
	d := &Deployment{Topology: topology, Runner: run, containerIDs: map[string]string{}}
	for _, slot := range Slots {
		d.containerIDs[slot] = strings.Repeat("a", 64)
	}
	r := &rehearsal{c: RehearseConfig{Assets: "/verified"}, deployment: d, topology: topology, runner: run, diagnostics: &bytes.Buffer{}, pins: []inventory.Pin{{Version: "0.3.0", ArchiveSHA256: ReleaseASHA256}, {Version: "0.3.1", ArchiveSHA256: ReleaseBSHA256}}}
	if e := r.stage(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(run.commands) != 16 {
		t.Fatal(len(run.commands))
	}
	for i, c := range run.commands {
		if i%2 == 0 {
			if c.Args[0] != "docker" || c.Args[1] != "cp" || !strings.Contains(c.Args[2], "linux_amd64.tar.gz") || !strings.HasPrefix(c.Args[3], strings.Repeat("a", 64)+":") {
				t.Fatal(c)
			}
		} else {
			script := string(c.Input)
			if !strings.Contains(script, "/opt/relais/boot/download.sh") || !strings.Contains(script, "RELAIS_TEST_ARCHIVE_URL=file://") || !strings.Contains(script, "x86_64") || !strings.Contains(script, "archive.sha256") || strings.Contains(script, "https://") {
				t.Fatal(script)
			}
		}
	}
}
func TestCallerFailurePrintsBothRedactedStreams(t *testing.T) {
	run := &assemblyRunner{fail: true}
	var out bytes.Buffer
	r := &rehearsal{runner: run, diagnostics: &out, topology: &Topology{Secrets: map[string]string{"token": "secret-value"}}}
	result := (callerExecutor{r}).Execute(context.Background(), []byte("exit 1"), time.Now().Add(time.Minute))
	if result.Certainty != operator.Failed || !strings.Contains(out.String(), "stdout:") || !strings.Contains(out.String(), "stderr:") || strings.Contains(out.String(), "secret-value") {
		t.Fatal(result, out.String())
	}
}
func TestAMD64AssemblyUsesDownloadArchitectureAndIsolatedCaller(t *testing.T) {
	stage := &host.Stage{Version: "0.3.0", Arch: "amd64", ArchiveSHA256: ReleaseASHA256, ManifestSHA256: strings.Repeat("b", 64), Pool: "e30=", Operation: "upgrade"}
	script, e := host.Process("freeze", struct{}{}, stage)
	if e != nil || !strings.Contains(script, "'x86_64'") {
		t.Fatal(script, e)
	}
	end := time.Now().Add(10 * time.Minute)
	id := operatorcaller.Identity{RunID: "run-assembly", Gate: operator.Rollout, Phase: operatorcaller.Survivor, CallerID: "1", Attempt: "1", PublicSHA: ReleaseACommit}
	cfg := operatorcaller.LaunchConfig{BridgePath: "/bridge", LoadPath: "/load", ConfigPath: "/config", MailboxPath: "/mailbox", WorkRoot: "/work", BridgeSHA256: ReleaseASHA256, LoadSHA256: ReleaseASHA256, ConfigSHA256: ReleaseASHA256, Architecture: "x86_64", RuntimeDeadline: end, CPUAffinity: "4 6"}
	raw, e := operatorcaller.StartScript(cfg, id, end)
	if e != nil || !strings.Contains(string(raw), "--property='CPUAffinity=4 6'") || !strings.Contains(string(raw), "--setenv=GOMAXPROCS=2") {
		t.Fatal(string(raw), e)
	}
	cfg.CPUAffinity = "4; print-secret"
	if _, e = operatorcaller.StartScript(cfg, id, end); e == nil {
		t.Fatal("untrusted affinity accepted")
	}
}

func TestRehearseWritesSetupFailureBeforeResourceCreation(t *testing.T) {
	directory := t.TempDir()
	var diagnostics bytes.Buffer
	report, e := Rehearse(context.Background(), RehearseConfig{Assets: directory + "/absent", Output: directory + "/report"}, &diagnostics)
	if e == nil || report.Verdict != operator.Failure || !report.Cleanup {
		t.Fatal(report, e)
	}
	raw, e := os.ReadFile(directory + "/report/report.json")
	if e != nil || !bytes.Contains(raw, []byte(`"setup-failed"`)) {
		t.Fatal(string(raw), e)
	}
	if !strings.Contains(diagnostics.String(), "local-file-unavailable") {
		t.Fatal("setup cause missing", diagnostics.String())
	}
}

func TestRehearsalLeafCertificateCanBeTrustedDirectly(t *testing.T) {
	dir := t.TempDir()
	if _, e := certificates(dir); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(dir, "signal/server.crt"))
	if e != nil {
		t.Fatal(e)
	}
	block, _ := pem.Decode(raw)
	leaf, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if _, e = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: SignalIP}); e != nil {
		t.Fatal("bridge cannot trust the same leaf certificate pinned by relais-load", e)
	}
	if leaf.IsCA {
		t.Fatal("caller must pin the server leaf, not its CA")
	}
}
