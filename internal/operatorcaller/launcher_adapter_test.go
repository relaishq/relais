package operatorcaller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLaunchConfigMailboxModes(t *testing.T) {
	adapter := fixtureLaunchConfig()
	adapter.MailboxPath = ""
	adapter.AdapterPath = "/opt/caller/mailbox-adapter"
	adapter.AdapterSHA256 = strings.Repeat("d", 64)
	for _, row := range []struct {
		name string
		base LaunchConfig
		edit func(*LaunchConfig)
		want string
	}{
		{"directory", fixtureLaunchConfig(), func(*LaunchConfig) {}, ""},
		{"adapter", adapter, func(*LaunchConfig) {}, ""},
		{"adapter-spool", adapter, func(c *LaunchConfig) { c.SpoolPath = "/opt/caller/spool" }, ""},
		{"both", adapter, func(c *LaunchConfig) { c.MailboxPath = "/opt/caller/mailbox" }, "invalid-mailbox-mode"},
		{"neither", fixtureLaunchConfig(), func(c *LaunchConfig) { c.MailboxPath = "" }, "invalid-mailbox-mode"},
		{"orphan-hash", fixtureLaunchConfig(), func(c *LaunchConfig) { c.AdapterSHA256 = adapter.AdapterSHA256 }, "invalid-mailbox-mode"},
		{"orphan-spool", fixtureLaunchConfig(), func(c *LaunchConfig) { c.SpoolPath = "/opt/caller/spool" }, "invalid-mailbox-mode"},
		{"missing-hash", adapter, func(c *LaunchConfig) { c.AdapterSHA256 = "" }, "invalid-launch-hash"},
		{"short-hash", adapter, func(c *LaunchConfig) { c.AdapterSHA256 = "abcd" }, "invalid-launch-hash"},
		{"uppercase-hash", adapter, func(c *LaunchConfig) { c.AdapterSHA256 = strings.Repeat("D", 64) }, "invalid-launch-hash"},
		{"relative-adapter", adapter, func(c *LaunchConfig) { c.AdapterPath = "adapter" }, "invalid-launch-path"},
		{"newline-adapter", adapter, func(c *LaunchConfig) { c.AdapterPath += "\ncommand" }, "invalid-launch-path"},
		{"nul-adapter", adapter, func(c *LaunchConfig) { c.AdapterPath += "\x00" }, "invalid-launch-path"},
		{"relative-spool", adapter, func(c *LaunchConfig) { c.SpoolPath = "spool" }, "invalid-launch-path"},
		{"newline-spool", adapter, func(c *LaunchConfig) { c.SpoolPath = "/spool\r" }, "invalid-launch-path"},
		{"relative-directory", fixtureLaunchConfig(), func(c *LaunchConfig) { c.MailboxPath = "mailbox" }, "invalid-launch-path"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := row.base
			row.edit(&c)
			for _, script := range []func() ([]byte, error){
				func() ([]byte, error) { return ReadyScript(c) },
				func() ([]byte, error) { return StartScript(c, fixtureManifest().Identity, time.Now().Add(time.Minute)) },
			} {
				_, err := script()
				if row.want == "" && err != nil || row.want != "" && (err == nil || err.Error() != row.want) {
					t.Fatal("MUTATION: launcher-mailbox-mode", row.want, err)
				}
			}
		})
	}
}

func TestStartScriptStagesMailboxAdapter(t *testing.T) {
	for _, name := range []string{"adapter", "spool", "hash-mismatch", "copy-change", "copy-change-restored"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			c := fixtureLaunchConfig()
			c.MailboxPath = ""
			c.WorkRoot = root
			c.RuntimeDeadline = time.Now().Add(5 * time.Minute)
			if name == "spool" {
				c.SpoolPath = root + "/spool ' with spaces"
			}
			for n, f := range []struct{ path, digest *string }{{&c.BridgePath, &c.BridgeSHA256}, {&c.LoadPath, &c.LoadSHA256}, {&c.ConfigPath, &c.ConfigSHA256}, {&c.AdapterPath, &c.AdapterSHA256}} {
				*f.path = filepath.Join(root, "input ' "+strconv.Itoa(n))
				data := []byte("staged input " + strconv.Itoa(n))
				if err := os.WriteFile(*f.path, data, 0600); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(data)
				*f.digest = hex.EncodeToString(sum[:])
			}
			if name == "hash-mismatch" {
				if err := os.WriteFile(c.AdapterPath, []byte("changed adapter"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(name, "copy-change") {
				cp, err := exec.LookPath("cp")
				if err != nil {
					t.Fatal(err)
				}
				// Replace the source at the copy boundary. The restored case
				// leaves the source hash valid while the staged bytes are wrong.
				stub := "#!/bin/sh\nset -eu\nif [ \"$1\" = " + quote(c.AdapterPath) + " ]; then\n" +
					quote(cp) + " \"$1\" \"$1.original\"\nprintf 'changed during copy' > \"$1\"\n" +
					quote(cp) + " \"$@\"\n"
				if name == "copy-change-restored" {
					stub += quote(cp) + " \"$1.original\" \"$1\"\n"
				}
				stub += "else\n" + quote(cp) + " \"$@\"\nfi\n"
				if err := os.WriteFile(filepath.Join(root, "cp"), []byte(stub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(root+"/argv") + "\n"
			if err := os.WriteFile(filepath.Join(root, "systemd-run"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			i := fixtureManifest().Identity
			script, err := StartScript(c, i, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-s")
			cmd.Env = []string{"PATH=" + root + ":" + os.Getenv("PATH")}
			cmd.Stdin = bytes.NewReader(script)
			out, err := cmd.CombinedOutput()
			args, readErr := os.ReadFile(root + "/argv")
			if name == "hash-mismatch" || strings.HasPrefix(name, "copy-change") {
				if err == nil || !os.IsNotExist(readErr) || bytes.Contains(out, []byte("caller-started:")) {
					t.Fatal("MUTATION: launcher-adapter-hash", string(out), err, readErr)
				}
				if strings.HasPrefix(name, "copy-change") {
					staged, err := os.ReadFile(filepath.Join(root, i.Unit(), "mailbox-adapter"))
					if err != nil || string(staged) != "changed during copy" {
						t.Fatal("copy control did not stage the changed bytes", string(staged), err)
					}
					source, err := os.ReadFile(c.AdapterPath)
					want := "changed during copy"
					if name == "copy-change-restored" {
						want = "staged input 3"
					}
					if err != nil || string(source) != want {
						t.Fatal("copy control left an unexpected source", string(source), err)
					}
				}
				return
			}
			if err != nil || readErr != nil || string(out) != "caller-started:"+i.Unit()+"\n" {
				t.Fatal("adapter launch failed", string(out), err, readErr)
			}
			staged := filepath.Join(root, i.Unit(), "mailbox-adapter")
			data, err := os.ReadFile(staged)
			info, statErr := os.Stat(staged)
			if err != nil || statErr != nil || info.Mode().Perm() != 0700 || string(data) != "staged input 3" {
				t.Fatal("adapter not staged as an executable", err, statErr)
			}
			argv := "\n" + string(args)
			if !strings.Contains(argv, "\n-adapter\n"+staged+"\n") || strings.Contains(argv, "\n-mailbox\n") {
				t.Fatal("MUTATION: launcher-adapter-flags", string(args))
			}
			if !strings.Contains(argv, "\n-deadline\n"+c.RuntimeDeadline.UTC().Format(time.RFC3339Nano)+"\n") {
				t.Fatal("caller runtime deadline missing", string(args))
			}
			if c.SpoolPath == "" && strings.Contains(argv, "\n-spool\n") || c.SpoolPath != "" && !strings.Contains(argv, "\n-spool\n"+c.SpoolPath+"\n") {
				t.Fatal("spool argument changed", string(args))
			}
		})
	}
}

func TestDirectoryLaunchScriptsStayUnchanged(t *testing.T) {
	c := fixtureLaunchConfig()
	c.RuntimeDeadline = time.Now().Add(10 * time.Minute).Truncate(time.Second)
	ready, err := ReadyScript(c)
	if err != nil {
		t.Fatal(err)
	}
	start, err := StartScript(c, fixtureManifest().Identity, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]byte{"directory-ready.sh": ready, "directory-start.sh": start} {
		want, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		want = bytes.ReplaceAll(want, []byte("{{DEADLINE}}"), []byte(strconv.FormatInt(c.RuntimeDeadline.Unix(), 10)))
		if !bytes.Equal(got, want) {
			t.Fatalf("directory script changed: %s\n%s", name, got)
		}
	}
}
