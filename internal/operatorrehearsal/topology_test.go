package operatorrehearsal

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/artifacts"
)

type bootRunner struct{ commands []Command }

func (r *bootRunner) Run(_ context.Context, c Command) CommandResult {
	r.commands = append(r.commands, c)
	out := ""
	if len(c.Args) > 1 && c.Args[1] == "run" {
		return CommandResult{Code: code(0), Stdout: []byte(fixtureContainerIDs()[strings.TrimPrefix(c.Label, "host-")])}
	}
	switch c.Label {
	case "network":
		out = strings.Repeat("b", 64)
	case "store-ready":
		out = "PONG"
	case "store-cluster":
		out = "cluster_state:ok"
	case "store-admin":
		out = "OK"
	case "time-ready":
		out = "Leap status     : Normal"
	case "systemd":
		out = "running"
	case "negative-wrong-acl":
		out = "OK\nNOPERM this user has no permissions"
	case "acl-positive":
		out = "OK\n1"
	case "cleanup-list":
		out = strings.Repeat("c", 64) + "\n"
	}
	return CommandResult{Code: code(0), Stdout: []byte(out)}
}

func TestBootPlanUsesTLSActualTemplateAndServiceAffinity(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.tar.gz")
	data := []byte("digest-bound archive fixture")
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
	// Boot submits the four template scripts concurrently; protect the command
	// recorder in this test without making real subprocess or Docker calls.
	record := &serialBootRunner{runner: runner}
	d, err := NewDeployment(topology, filepath.Join(dir, "work"), archive, layout, record, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, slot := range Slots {
		config, err := os.ReadFile(filepath.Join(dir, "work", "hosts", slot, "chrony.conf"))
		if err != nil {
			t.Fatal(err)
		}
		for _, directive := range []string{"log tracking\n", "logdir /var/log/chrony\n", "maxclockerror 1\n"} {
			if !strings.Contains(string(config), directive) {
				t.Fatal("tracking log configuration missing", slot, directive)
			}
		}
		dropIn, err := os.ReadFile(filepath.Join(dir, "work", "hosts", slot, "chronyd.conf"))
		if err != nil || !strings.Contains(string(dropIn), "chronyd -x ") {
			t.Fatal("rehearsal may adjust the host clock", err)
		}
	}
	booted := map[string]bool{}
	storeSeen := false
	proxySeen := false
	for _, c := range runner.commands {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "docker run") {
			if !strings.Contains(joined, "--cpuset-cpus 8,9") {
				t.Fatal("container overlaps caller")
			}
			if !strings.Contains(joined, containerOwnerKey+"="+d.owner) {
				t.Fatal("resource lacks ownership")
			}
		}
		if c.Label == "store" {
			storeSeen = true
			for _, s := range []string{"--port 0", "--tls-port 16379", "--cluster-enabled yes", "--tls-cluster yes", "--tmpfs /data:rw", "valkey/valkey:9.0.6"} {
				if !strings.Contains(joined, s) {
					t.Fatal("store lacks " + s)
				}
			}
			if strings.Contains(joined, "--tls-port 6379") {
				t.Fatal("unsafe store port")
			}
		}
		if c.Label == "signal-front" {
			proxySeen = strings.Contains(joined, "openssl-listen:443") && strings.Contains(joined, "openssl:10.42.10.20:24743")
		}
		if c.Label == "time" {
			// Execute the generated reference-config write without chronyd. This
			// catches shell escaping that would leave literal backslash-n bytes.
			conf := filepath.Join(dir, "chrony-reference.conf")
			script := c.Args[len(c.Args)-1]
			script, _, _ = strings.Cut(script, " && exec chronyd")
			script = strings.ReplaceAll(script, "/etc/chrony-reference.conf", quote(conf))
			if output, x := exec.Command("/bin/sh", "-eu", "-c", script).CombinedOutput(); x != nil {
				t.Fatalf("reference config: %v %s", x, output)
			}
			b, x := os.ReadFile(conf)
			if x != nil || string(b) != "local stratum 1\nallow "+topology.Subnet+"\n" {
				t.Fatal("reference config has wrong newlines")
			}
		}
		if strings.HasPrefix(c.Label, "boot-") {
			booted[strings.TrimPrefix(c.Label, "boot-")] = true
			if !strings.Contains(joined, "bash /rehearsal/user-data.sh") || !strings.Contains(joined, "RELAIS_TEST_ARCHIVE_URL=file:///release.tar.gz") {
				t.Fatal("template boot replaced")
			}
			if strings.Contains(joined, "RELAIS_TEST_READY") {
				t.Fatal("readiness bypassed")
			}
		}
	}
	if len(booted) != 4 || !storeSeen || !proxySeen {
		t.Fatal("incomplete boot plan")
	}
	if err = d.NegativeControls(context.Background(), func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	controls := map[string]string{}
	for _, c := range runner.commands {
		if strings.HasPrefix(c.Label, "negative-") {
			controls[c.Label] = string(c.Input)
		}
	}
	if len(controls) != 4 || !strings.Contains(controls["negative-missing-credential"], "failed-with-EXIT_CREDENTIALS-243-and-no-process") || !strings.Contains(controls["negative-missing-unit"], "nonzero-start-and-LoadState-not-found") || !strings.Contains(controls["negative-wrong-time-source"], "192.0.2.123") {
		t.Fatal("negative controls did not demand refusal")
	}
	if err = d.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range runner.commands {
		if c.Label == "cleanup-list" && !strings.Contains(strings.Join(c.Args, " "), containerOwnerKey+"="+d.owner) {
			t.Fatal("unscoped cleanup")
		}
	}
}

// Setup/verification can be concurrent; the fake does no I/O.
type serialBootRunner struct {
	mu     sync.Mutex
	runner *bootRunner
}

func (r *serialBootRunner) Run(ctx context.Context, c Command) CommandResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runner.Run(ctx, c)
}

// bootCommands boots the real template against the recording fake runner.
func bootCommands(t *testing.T) []Command {
	t.Helper()
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.tar.gz")
	data := []byte("digest-bound archive fixture")
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
	return runner.commands
}

// Per host: the rehearsal clock is set up, the template user data boots (and
// installs its own chrony.conf), that file is checked, the rehearsal reference
// is restored and synchronized, then the verify script (ending in ClockCheck) runs.
func TestBootOrdersClockStepsPerHost(t *testing.T) {
	resync := []string{"install -m 0644 /rehearsal/chrony.conf /etc/chrony.conf\n", "systemctl restart chronyd\n", "chronyc -n waitsync 240 0 20 1\n"}
	if ClockResyncScript != "set -eu\n"+strings.Join(resync, "") {
		t.Fatal("resync must restore the reference, restart, then wait a bounded time for sync")
	}
	commands := bootCommands(t)
	for _, slot := range Slots {
		var labels []string
		for _, c := range commands {
			if strings.HasSuffix(c.Label, "-"+slot) && (strings.HasPrefix(c.Label, "setup-") || strings.HasPrefix(c.Label, "boot-") || strings.HasPrefix(c.Label, "template-chrony-") || strings.HasPrefix(c.Label, "clock-resync-") || strings.HasPrefix(c.Label, "verify-")) {
				labels = append(labels, strings.TrimSuffix(c.Label, "-"+slot))
				if strings.HasPrefix(c.Label, "clock-resync-") && string(c.Input) != ClockResyncScript {
					t.Fatal("resync script replaced", slot)
				}
				if strings.HasPrefix(c.Label, "verify-") && !strings.HasSuffix(string(c.Input), ClockCheck()) {
					t.Fatal("verify does not end with the clock check", slot)
				}
			}
		}
		if want := []string{"setup", "boot", "template-chrony", "clock-resync", "verify"}; !reflect.DeepEqual(labels, want) {
			t.Fatalf("%s clock step order %v, want %v", slot, labels, want)
		}
	}
}

// The check accepts exactly the file install.sh writes and refuses any other.
func TestTemplateChronyScriptMatchesInstallBlock(t *testing.T) {
	topology := fixtureTopology(t)
	script, err := topology.TemplateChronyScript()
	if err != nil {
		t.Fatal(err)
	}
	install := topology.Common.Files["/opt/relais/boot/install.sh"].Content
	root := t.TempDir()
	snippet := install[strings.Index(install, "cat > "):strings.Index(install, "chmod 0644")]
	if output, x := exec.Command("bash", "-euc", "root="+quote(root)+"\nmkdir -p \"$root/etc\"\n"+snippet).CombinedOutput(); x != nil {
		t.Fatalf("install block: %v %s", x, output)
	}
	written, err := os.ReadFile(filepath.Join(root, "etc/chrony.conf"))
	if err != nil || !strings.Contains(string(written), "minpoll 3 maxpoll 3\n") || !strings.HasSuffix(string(written), "\nlog tracking\n") {
		t.Fatalf("install block wrote %q %v", written, err)
	}
	conf := filepath.Join(root, "etc/chrony.conf")
	local := strings.ReplaceAll(script, "'/etc/chrony.conf'", strconv.Quote(conf))
	for _, row := range []struct {
		name, content string
		pass          bool
	}{
		{"template", string(written), true},
		{"rehearsal-reference", "server 10.42.0.5 prefer iburst minpoll 0 maxpoll 0\n", false},
		{"slower-poll", strings.Replace(string(written), "minpoll 3 maxpoll 3", "minpoll 4 maxpoll 4", 1), false},
	} {
		if err = os.WriteFile(conf, []byte(row.content), 0644); err != nil {
			t.Fatal(err)
		}
		output, x := exec.Command("/bin/sh", "-eu", "-c", local).CombinedOutput()
		if (x == nil) != row.pass || !row.pass && !strings.Contains(string(output), "template-chrony-mismatch") {
			t.Fatalf("MUTATION: %s pass=%v err=%v output=%s", row.name, row.pass, x, output)
		}
	}
	missing := *topology
	missing.Common.Files = map[string]File{}
	if _, err = missing.TemplateChronyScript(); err == nil || err.Error() != "template-chrony-missing" {
		t.Fatal("missing install block accepted", err)
	}
}

func TestBootRejectsWrongArchiveBeforeAnyCommand(t *testing.T) {
	topology := fixtureTopology(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, "wrong.tar.gz")
	if err := os.WriteFile(archive, []byte("wrong"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, _ := Layout([]int{2, 3, 8, 9}, nil)
	runner := &fakeRunner{}
	d, err := NewDeployment(topology, filepath.Join(dir, "work"), archive, layout, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Boot(context.Background()); err == nil || len(runner.commands) != 0 {
		t.Fatal("unverified archive submitted")
	}
}

func TestCertificatesBindLocalTLSNames(t *testing.T) {
	dir := t.TempDir()
	ca, err := certificates(dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	for _, row := range []struct{ file, name string }{{"store", StoreEndpoint}, {"store", StoreNode}, {"signal", SignalName}, {"signal", SignalIP}} {
		b, x := os.ReadFile(filepath.Join(dir, row.file, "server.crt"))
		if x != nil {
			t.Fatal(x)
		}
		block, _ := pem.Decode(b)
		cert, x := x509.ParseCertificate(block.Bytes)
		if x != nil {
			t.Fatal(x)
		}
		if _, x = cert.Verify(x509.VerifyOptions{DNSName: row.name, Roots: pool}); x != nil {
			t.Fatal(x)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestP2ReleaseReaderScopesJobToken(t *testing.T) {
	var redirected bool
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		header := http.Header{}
		body := ""
		if r.URL.Host == "api.github.com" {
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				t.Fatal("metadata lacked job token")
			}
			if strings.Contains(r.URL.Path, "/releases/") {
				header.Set("Location", "https://assets.rehearsal.test/metadata")
				return &http.Response{StatusCode: 302, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			}
			body = `{"sha":"` + strings.Repeat("a", 40) + `"}`
		} else {
			redirected = true
			if r.Header.Get("Authorization") != "" {
				t.Fatal("job token leaked to asset host")
			}
			body = `{"id":1,"tag_name":"v0.3.0","draft":false,"prerelease":false}`
		}
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	reader := artifacts.NewPublicReader(client, "fixture-token")
	reply, err := reader.Release(context.Background(), "v0.3.0")
	if err != nil || reply.State != "found" || reply.Release.Tag != "v0.3.0" || !redirected {
		t.Fatalf("P2 release read: %v", err)
	}
	if _, err = reader.Release(context.Background(), "../secrets"); err == nil {
		t.Fatal("accepted arbitrary API path")
	}
}

// This opt-in test is the manual runner seam used by P6. It is intentionally
// skipped on this Mac: offline command tests do not prove systemd or containers.
func TestContainerTemplateBoot(t *testing.T) {
	archive := os.Getenv("RELAIS_REHEARSAL_ARCHIVE")
	if testing.Short() || os.Getenv("RELAIS_REHEARSAL_CONTAINERS") != "1" || archive == "" {
		t.Skip("runner only: use make test-operator-rehearsal-container with pinned release settings")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("container rehearsal requires a Linux runner")
	}
	layout, err := DetectLayout(nil)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Name: fmt.Sprintf("relais-template-%d", os.Getpid()), Version: os.Getenv("RELAIS_REHEARSAL_VERSION"), Architecture: os.Getenv("RELAIS_REHEARSAL_ARCH"), ArchiveSHA256: os.Getenv("RELAIS_REHEARSAL_SHA256"), StorePort: 16379}
	topology, err := Load(os.DirFS("../.."), config)
	if err != nil {
		t.Fatal(err)
	}
	archive, err = filepath.Abs(archive)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDeployment(topology, filepath.Join(t.TempDir(), "topology"), archive, layout, LocalRunner{}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if x := d.Close(ctx); x != nil {
			t.Error(x)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	if err = d.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if err = d.NegativeControls(ctx, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Log("VERIFIED: actual template units booted; missing credential, wrong ACL, missing unit and wrong time source refused")
}
