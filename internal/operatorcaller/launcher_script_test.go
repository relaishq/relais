package operatorcaller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadyScriptNamesFailedCheck(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "uname"), []byte("#!/bin/sh\necho unsupported\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script, err := ReadyScript(fixtureLaunchConfig())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Env = []string{"PATH=" + root + ":" + os.Getenv("PATH")}
	cmd.Stdin = bytes.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err == nil || string(out) != "caller-ready-architecture-failed\n" {
		t.Fatal("caller readiness hid the failing check", string(out), err)
	}
}

func TestStartScriptPreservesArgvAndObservesExecFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "exec-failed"}[fail], func(t *testing.T) {
			root := t.TempDir()
			c := fixtureLaunchConfig()
			c.WorkRoot = root
			c.MailboxPath = root + "/mailbox with spaces"
			c.CPUAffinity = "4 6"
			c.RuntimeDeadline = time.Now().Add(10 * time.Minute)
			for i, f := range []struct{ path, digest *string }{{&c.BridgePath, &c.BridgeSHA256}, {&c.LoadPath, &c.LoadSHA256}, {&c.ConfigPath, &c.ConfigSHA256}} {
				*f.path = filepath.Join(root, "input-"+strconv.Itoa(i))
				data := []byte(*f.path)
				if err := os.WriteFile(*f.path, data, 0700); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(data)
				*f.digest = hex.EncodeToString(sum[:])
			}
			// Model systemd's default early acknowledgement versus Type=exec's
			// confirmed exec failure. This runs the real staging/launch shell.
			stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(root+"/argv") + "\n"
			if fail {
				stub += "for arg do if [ \"$arg\" = --service-type=exec ]; then echo 'bridge exec failed' >&2; exit 203; fi; done\n"
			}
			if err := os.WriteFile(filepath.Join(root, "systemd-run"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			identity := fixtureManifest().Identity
			script, err := StartScript(c, identity, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-s")
			cmd.Env = []string{"PATH=" + root + ":" + os.Getenv("PATH")}
			cmd.Stdin = bytes.NewReader(script)
			out, err := cmd.CombinedOutput()
			if fail {
				if err == nil || bytes.Contains(out, []byte("caller-started:")) || !bytes.Contains(out, []byte("bridge exec failed")) {
					t.Fatal("caller launch hid exec failure", string(out), err)
				}
				return
			}
			if err != nil || string(out) != "caller-started:"+identity.Unit()+"\n" {
				t.Fatal("caller launch failed", string(out), err)
			}
			args, err := os.ReadFile(root + "/argv")
			if err != nil {
				t.Fatal(err)
			}
			rawIdentity, _ := json.Marshal(identity)
			for _, arg := range []string{"--service-type=exec", "--property=CPUAffinity=4 6", "--setenv=GOMAXPROCS=2", c.MailboxPath, string(rawIdentity)} {
				if !strings.Contains("\n"+string(args), "\n"+arg+"\n") {
					t.Fatalf("launcher split or changed argument %q: %s", arg, args)
				}
			}
		})
	}
}
