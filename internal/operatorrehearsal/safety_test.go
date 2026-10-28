package operatorrehearsal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func bootedFixture(t *testing.T) (*Deployment, *bootRunner) {
	t.Helper()
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.tar.gz")
	data := []byte("review archive fixture")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	config := fixtureConfig()
	sum := sha256.Sum256(data)
	config.ArchiveSHA256 = hex.EncodeToString(sum[:])
	topology, err := Load(os.DirFS("../.."), config)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := Layout([]int{2, 3, 8, 9}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &bootRunner{}
	d, err := NewDeployment(topology, filepath.Join(dir, "work"), archive, layout, &serialBootRunner{runner: runner}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, runner
}

func TestCleanupOwnerCannotBeInheritedFromImage(t *testing.T) {
	d, runner := bootedFixture(t)
	other, err := NewDeployment(d.Topology, filepath.Join(t.TempDir(), "work"), d.Archive, d.Layout, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.owner == other.owner {
		t.Fatal("deployment ownership ID reused")
	}
	for _, command := range runner.commands {
		if command.Label == "image" || command.Label == "network" {
			for _, arg := range command.Args {
				if strings.Contains(arg, d.owner) {
					t.Fatal("container ownership ID escapes onto another resource")
				}
			}
		}
	}
	if err = d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, command := range runner.commands {
		if strings.HasPrefix(command.Label, "cleanup-") {
			joined := strings.Join(command.Args, " ")
			if strings.Contains(joined, "docker image") || strings.Contains(joined, "docker volume") || strings.Contains(joined, " -v ") {
				t.Fatal("cleanup can delete an unowned image or volume")
			}
			if strings.Contains(joined, "docker network ls") {
				t.Fatal("network cleanup rediscovered ownership rather than retaining the created ID")
			}
		}
	}
}

func TestEveryGeneratedArgvExcludesSecrets(t *testing.T) {
	d, runner := bootedFixture(t)
	if err := d.NegativeControls(context.Background()); err != nil {
		t.Fatal(err)
	}
	adapter := &Adapter{Topology: d.Topology, Containers: d.ContainerIDs(), Runner: &serialBootRunner{runner: runner}}
	req := request()
	req.Script = []byte("printf " + d.Topology.Secrets["SignalToken"])
	_ = adapter.Execute(context.Background(), req)
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, command := range runner.commands {
		for _, arg := range command.Args {
			for _, secret := range d.Topology.Secrets {
				if strings.Contains(arg, secret) {
					t.Fatalf("generated argv exposes a secret in %s", command.Label)
				}
			}
		}
	}
}

func TestTruncatedDiagnosticsSuppressSecretPrefixes(t *testing.T) {
	topology := fixtureTopology(t)
	secret := topology.Secrets["WorkerPassword"]
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			var output boundedOutput
			_, _ = output.Write(append(bytes.Repeat([]byte("x"), 23999-32), []byte(secret)...))
			result := CommandResult{Code: code(3), Truncated: output.truncated}
			if stream == "stdout" {
				result.Stdout = output.data
			} else {
				result.Stderr = output.data
			}
			// A custom runner can still return a captured prefix; both diagnostic sinks
			// must suppress it independently of LocalRunner's capture implementation.
			prefix := []byte(secret[:32])
			if stream == "stdout" {
				result.Stdout = prefix
			} else {
				result.Stderr = prefix
			}
			runner := &fakeRunner{result: result}
			var diagnostic bytes.Buffer
			adapter := &Adapter{Topology: topology, Containers: fixtureContainerIDs(), Runner: runner, Diagnostics: &diagnostic}
			_ = adapter.Execute(context.Background(), request())
			d := &Deployment{Topology: topology, Runner: runner, Diagnostics: &diagnostic}
			_, _ = d.command(context.Background(), "fixture", []string{"unused"}, nil)
			if bytes.Contains(diagnostic.Bytes(), prefix) {
				t.Fatal("truncated diagnostic exposed a secret prefix")
			}
			if !strings.Contains(diagnostic.String(), "suppressed") {
				t.Fatal("incomplete diagnostic was not explicitly suppressed")
			}
		})
	}
}

func TestStoreACLUsersFollowAttachedMembership(t *testing.T) {
	files := copiedTemplate(t)
	entry := files["deploy/aws/preview.yaml"]
	old := []byte("      - Ref: WorkerStoreUser\n")
	if bytes.Count(entry.Data, old) != 1 {
		t.Fatal("membership mutation did not match exactly once")
	}
	entry.Data = bytes.Replace(entry.Data, old, nil, 1)
	topology, err := Load(files, fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range topology.Users {
		if user.Name == "worker" {
			t.Fatal("detached worker ACL still installed")
		}
	}
	if len(topology.Users) != 2 {
		t.Fatal("attached ACL membership differs")
	}
	d := &Deployment{Topology: topology, Runner: &fakeRunner{}}
	if err = d.checkWrongACL(context.Background()); err == nil {
		t.Fatal("missing worker membership can qualify setup")
	}
}

type clockControlRunner struct {
	dir              string
	removeComparison bool
}

func (r *clockControlRunner) Run(_ context.Context, c Command) CommandResult {
	if c.Label != "negative-wrong-time-source" {
		return CommandResult{Code: code(0)}
	}
	script := strings.ReplaceAll(string(c.Input), "/etc/chrony.conf", filepath.Join(r.dir, "chrony.conf"))
	script = strings.ReplaceAll(script, "'/proc/'", quote(r.dir+"/"))
	script = strings.ReplaceAll(script, "time.sleep(0.1)", "pass")
	if r.removeComparison {
		script = strings.ReplaceAll(script, "rows[0][1] != '"+TimeIP+"'", "False")
	}
	command := exec.Command("/bin/sh", "-eu")
	command.Env = []string{"PATH=" + r.dir + ":" + os.Getenv("PATH")}
	command.Stdin = strings.NewReader(script)
	var out, stderr bytes.Buffer
	command.Stdout = &out
	command.Stderr = &stderr
	err := command.Run()
	status := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			return CommandResult{Err: err}
		}
		status = exit.ExitCode()
	}
	return CommandResult{Code: code(status), Stdout: out.Bytes(), Stderr: stderr.Bytes()}
}
func TestWrongTimeSourceRequiresAddressRejection(t *testing.T) {
	for _, row := range []struct {
		name, sources                                                                  string
		missingPID, missingSocket, trackingFailure, removeComparison, badRestore, pass bool
	}{
		{name: "wrong-address", sources: "^? 192.0.2.123 1", pass: true},
		{name: "restore-source-still-wrong", sources: "^? 192.0.2.123 1", badRestore: true},
		{name: "wrong-address-table", sources: "MS Name/IP address Stratum Poll Reach LastRx Last sample\n===============================================================================\n^? 192.0.2.123 1", pass: true},
		{name: "missing-process", sources: "^? 192.0.2.123 1", missingPID: true},
		{name: "missing-socket", sources: "^? 192.0.2.123 1", missingSocket: true},
		{name: "unselected-reference", sources: "^? " + TimeIP + " 1"},
		{name: "tracking-unavailable", sources: "^? 192.0.2.123 1", trackingFailure: true},
		{name: "address-comparison-removed", sources: "^? 192.0.2.123 1", removeComparison: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			_ = python(t, "--version")
			dir := t.TempDir()
			sourceCommand := "printf '%s\\n' " + quote(row.sources)
			if row.missingSocket {
				sourceCommand = "exit 1"
			}
			trackingCommand := "printf 'Leap status     : Not synchronised\\n'"
			if row.trackingFailure {
				trackingCommand = "exit 1"
			}
			// The fake daemon follows its restored configuration, rather than
			// reporting the intentionally wrong source forever after restoration.
			systemctl := `#!/bin/sh
if [ "$1" = show ]; then
 if [ "$3" = MainPID ] && [ "$4" = --value ]; then printf 42; exit; fi
 printf 'LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=42\nResult=success\nExecMainCode=0\nExecMainStatus=0\n'
fi
`
			chronyc := "#!/bin/sh\nif ! grep -q 192.0.2.123 " + quote(filepath.Join(dir, "chrony.conf")) + "; then\ncase $2 in sources) printf '%s\\n' " + quote("^* "+TimeIP+" 1") + ";; tracking) printf 'Leap status     : Normal\\n';; waitsync) exit 0;; esac\nexit 0\nfi\ncase $2 in sources) " + sourceCommand + ";; tracking) " + trackingCommand + ";; waitsync) exit 0;; *) exit 1;; esac\n"
			if row.badRestore {
				chronyc = strings.Replace(chronyc, "if ! grep -q 192.0.2.123 "+quote(filepath.Join(dir, "chrony.conf"))+"; then", "if false; then", 1)
			}
			files := map[string][]byte{"chrony.conf": []byte("original\n"), "systemctl": []byte(systemctl), "chronyc": []byte(chronyc)}
			if !row.missingPID {
				files["42/cmdline"] = []byte("chronyd\x00-x\x00")
			}
			if err := writeFiles(dir, files); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"systemctl", "chronyc"} {
				if err := os.Chmod(filepath.Join(dir, name), 0755); err != nil {
					t.Fatal(err)
				}
			}
			runner := &clockControlRunner{dir: dir, removeComparison: row.removeComparison}
			d := &Deployment{Topology: fixtureTopology(t), Runner: runner, Diagnostics: io.Discard, aclNegative: true, containerIDs: fixtureContainerIDs()}
			err := d.NegativeControls(context.Background())
			if (err == nil) != row.pass {
				t.Fatalf("wrong-source control accepted unrelated failure: success=%v expected=%v", err == nil, row.pass)
			}
			if row.badRestore && (err == nil || err.Error() != "negative-control-restore-failed") {
				t.Fatal("clock restore was assumed", err)
			}
			raw, e := os.ReadFile(filepath.Join(dir, "chrony.conf"))
			if e != nil || string(raw) != "original\n" {
				t.Fatal("time configuration was not restored", e)
			}
			if _, e := os.Stat(filepath.Join(dir, "chrony.conf.saved")); !os.IsNotExist(e) {
				t.Fatal("time configuration backup remains", e)
			}
		})
	}
}

// This models Docker's image-label inheritance at the real command boundary.
// The independent container uses the built image with no explicit owner label.
type ownershipRunner struct {
	containers map[string]map[string]string
	removed    map[string]bool
	networkID  string
}

func (r *ownershipRunner) Run(_ context.Context, c Command) CommandResult {
	if c.Label == "cleanup-list" {
		filter := strings.TrimPrefix(c.Args[len(c.Args)-1], "label=")
		key, value, _ := strings.Cut(filter, "=")
		var ids []string
		for id, labels := range r.containers {
			if labels[key] == value {
				ids = append(ids, id)
			}
		}
		return CommandResult{Code: code(0), Stdout: []byte(strings.Join(ids, "\n"))}
	}
	if c.Label == "cleanup-container" {
		r.removed[c.Args[len(c.Args)-1]] = true
	}
	if c.Label == "cleanup-network" {
		r.networkID = c.Args[len(c.Args)-1]
	}
	return CommandResult{Code: code(0)}
}
func commandLabels(args []string) map[string]string {
	labels := map[string]string{}
	for i, arg := range args {
		if arg == "--label" && i+1 < len(args) {
			key, value, _ := strings.Cut(args[i+1], "=")
			labels[key] = value
		}
	}
	return labels
}
func TestCleanupPreservesIndependentContainerUsingSameImage(t *testing.T) {
	d, commands := bootedFixture(t)
	imageLabels := map[string]string{}
	for _, c := range commands.commands {
		if c.Label == "image" {
			imageLabels = commandLabels(c.Args)
		}
	}
	owned := map[string]string{}
	foreign := map[string]string{}
	for key, value := range imageLabels {
		owned[key] = value
		foreign[key] = value
	}
	for key, value := range commandLabels(d.runArgs("w1", d.Topology.Hosts["w1"].IP, d.Topology.Config.Name+"-host", true)) {
		owned[key] = value
	}
	ownedID, foreignID := strings.Repeat("c", 64), strings.Repeat("d", 64)
	runner := &ownershipRunner{containers: map[string]map[string]string{ownedID: owned, foreignID: foreign}, removed: map[string]bool{}}
	d.Runner = runner
	if err := d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runner.removed[ownedID] || runner.removed[foreignID] {
		t.Fatal("cleanup deleted the independent container or missed its own container")
	}
	if runner.networkID != d.networkID || runner.networkID == d.Topology.Config.Name {
		t.Fatal("cleanup did not use the exact network creation ID")
	}
}
