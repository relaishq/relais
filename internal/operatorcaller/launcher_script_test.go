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

func TestStopScriptRequiresExactCollectedProof(t *testing.T) {
	i := fixtureManifest().Identity
	u := i.Unit()
	dead := "Id=" + u + "\nActiveState=inactive\nSubState=dead\n"
	missing := "Failed to stop " + u + ": Unit " + u + " not loaded.\n"
	for _, row := range []struct {
		name, state, journal, stop string
		showCode, stopCode         int
		want                       bool
	}{
		{"stopped", "Id=" + u + "\nActiveState=active\nSubState=running\n", "", "", 0, 0, true},
		{"collected-dead", dead, "", missing, 0, 5, true},
		{"collected-absent", dead, "", missing, 1, 5, true},
		{"collected-journal", "", u + ": Deactivated successfully.\n", missing, 1, 5, true},
		{"wrong-unit-state", strings.ReplaceAll(dead, u, "other.service"), "", missing, 0, 5, false},
		{"wrong-unit-journal", "", "other.service: Deactivated successfully.\n", missing, 1, 5, false},
		{"missing-proof", "", "", missing, 1, 5, false},
		{"missing-substate", "Id=" + u + "\nActiveState=inactive\n", "", missing, 0, 5, false},
		{"active", "Id=" + u + "\nActiveState=active\nSubState=running\n", "", missing, 0, 5, false},
		{"observation-timeout", dead, "", missing, 124, 5, false},
		{"stop-timeout", dead, "", missing, 0, 124, false},
		{"stop-killed", dead, "", missing, 0, 137, false},
		{"permission-denied", dead, "", "Access denied\n", 0, 1, false},
		{"wrong-stop-unit", dead, "", "Failed to stop other.service: Unit other.service not loaded.\n", 0, 5, false},
		{"extra-error", dead, "", missing + "another error\n", 0, 5, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"state": row.state, "journal": row.journal, "stop": row.stop,
				"systemctl":  "#!/bin/sh\nprintf '%s\\n' \"$1\" >> " + quote(root+"/order") + "\ncase $1 in\nshow) cat " + quote(root+"/state") + "; exit " + strconv.Itoa(row.showCode) + ";;\nstop) cat " + quote(root+"/stop") + " >&2; exit " + strconv.Itoa(row.stopCode) + ";;\n*) exit 99;;\nesac\n",
				"journalctl": "#!/bin/sh\nprintf 'journal\\n' >> " + quote(root+"/order") + "\nprintf '%s\\n' \"$@\" > " + quote(root+"/journal-args") + "\ncat " + quote(root+"/journal") + "\n",
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0700); err != nil {
					t.Fatal(err)
				}
			}
			script, err := StopScript(i)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-s")
			cmd.Env = []string{"PATH=" + root + ":" + os.Getenv("PATH")}
			cmd.Stdin = bytes.NewReader(script)
			out, err := cmd.CombinedOutput()
			if (err == nil) != row.want {
				t.Fatal("collected-unit stop certainty differs", string(out), err)
			}
			order, _ := os.ReadFile(root + "/order")
			if !strings.HasPrefix(string(order), "show\n") || !strings.HasSuffix(string(order), "stop\n") {
				t.Fatal("proof was not observed before stop", string(order))
			}
			if row.name == "collected-journal" {
				args, _ := os.ReadFile(root + "/journal-args")
				if !strings.Contains(string(args), "UNIT="+u+"\n") || !strings.Contains(string(args), "_PID=1\n") {
					t.Fatal("journal proof was not scoped to the manager and exact unit", string(args))
				}
			}
		})
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
