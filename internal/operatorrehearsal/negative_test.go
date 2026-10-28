package operatorrehearsal

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type credentialControlRunner struct {
	dir            string
	start, restore string
	control        string
	commands       []string
}

func (r *credentialControlRunner) Run(_ context.Context, c Command) CommandResult {
	r.commands = append(r.commands, c.Label)
	if c.Label != "negative-"+r.control {
		return CommandResult{Code: code(0)}
	}
	script := strings.ReplaceAll(string(c.Input), "/run/relais/signal.token", filepath.Join(r.dir, "signal.token"))
	script = strings.ReplaceAll(script, "/etc/systemd/system/relais-worker.service", filepath.Join(r.dir, "worker.service"))
	script = strings.ReplaceAll(script, "time.sleep(0.1)", "pass")
	command := exec.Command("/bin/sh", "-eu")
	command.Env = []string{"PATH=" + r.dir + ":" + os.Getenv("PATH"), "CONTROL_DIR=" + r.dir, "START_MODE=" + r.start, "RESTORE_MODE=" + r.restore}
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
func credentialControlFixture(t *testing.T, mode, restore string) (*Deployment, *credentialControlRunner, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	systemctl := `#!/bin/sh
set -eu
case "$1" in
stop)
 case "$*" in *worker*) if [ "$(cat "$CONTROL_DIR/load")" = not-found ]; then exit 5; fi;; esac
 echo inactive > "$CONTROL_DIR/state";;
start|restart)
 case "$*" in
  *worker*)
   if [ "$(cat "$CONTROL_DIR/load")" = not-found ]; then
    if [ "$START_MODE" = accepts ]; then echo active > "$CONTROL_DIR/state"; exit 0; fi
    echo inactive > "$CONTROL_DIR/state"; exit 5
   fi;;
 esac
 if [ -f "$CONTROL_DIR/signal.token" ]; then
  if [ "$RESTORE_MODE" = broken ]; then echo failed > "$CONTROL_DIR/state"; exit 1; fi
  echo active > "$CONTROL_DIR/state"
 else
  case "$START_MODE" in
   sync) echo failed > "$CONTROL_DIR/state"; exit 1;;
   async) echo failed > "$CONTROL_DIR/state";;
   accepts) echo active > "$CONTROL_DIR/state";;
   unrelated) echo unrelated > "$CONTROL_DIR/state";;
  esac
 fi;;
reset-failed) :;;
daemon-reload) if [ -f "$CONTROL_DIR/worker.service" ]; then echo loaded; else echo not-found; fi > "$CONTROL_DIR/load";;
is-failed) test "$(cat "$CONTROL_DIR/state")" = failed;;
is-active) test "$(cat "$CONTROL_DIR/state")" = active;;
show)
 state=$(cat "$CONTROL_DIR/state")
 load=loaded
 case "$*" in *worker*) load=$(cat "$CONTROL_DIR/load");; esac
 while [ "$#" -gt 0 ]; do
  if [ "$1" = -p ]; then
   shift
   case "$1" in
    LoadState) value=$load;;
    ActiveState) if [ "$state" = unrelated ]; then value=failed; else value=$state; fi;;
    SubState) if [ "$state" = active ]; then value=running; else value=dead; fi;;
    MainPID) if [ "$state" = active ]; then value=42; else value=0; fi;;
    ExecMainStatus) if [ "$state" = failed ]; then value=243; elif [ "$state" = active ]; then value=0; else value=1; fi;;
    ExecMainCode) value=exited;;
    Result) if [ "$state" = active ]; then value=success; else value=exit-code; fi;;
    *) exit 1;;
   esac
   printf '%s=%s\n' "$1" "$value"
  fi
  shift
 done;;
*) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctl), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state"), []byte("active\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "signal.token"), []byte("fixture-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.service"), []byte("fixture-unit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "load"), []byte("loaded\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &credentialControlRunner{dir: dir, start: mode, restore: restore, control: "missing-credential"}
	var diagnostics bytes.Buffer
	d := &Deployment{Topology: fixtureTopology(t), Runner: runner, Diagnostics: &diagnostics, aclNegative: true, containerIDs: fixtureContainerIDs()}
	return d, runner, &diagnostics
}
func TestCredentialControlObservesAsynchronousRefusal(t *testing.T) {
	d, r, diagnostics := credentialControlFixture(t, "async", "")
	if err := d.NegativeControls(context.Background()); err != nil {
		t.Fatal("MUTATION: credential-asynchronous-refusal", err, diagnostics.String())
	}
	data, err := os.ReadFile(filepath.Join(r.dir, "signal.token"))
	if err != nil || string(data) != "fixture-token\n" {
		t.Fatal("credential was not restored", err)
	}
	for _, want := range []string{"relais-signal.service", "signal.token", "expected=", "start_status=0", "ExecMainStatus=243", "restore", "exit_status=0"} {
		if !strings.Contains(diagnostics.String(), want) {
			t.Fatal("MUTATION: credential-control-diagnostics", want, diagnostics.String())
		}
	}
}

func TestCredentialControlRejectsOtherOutcomesAndRestores(t *testing.T) {
	for _, row := range []struct{ name, mode, restore, remove, reason string }{
		{"synchronous", "sync", "", "", ""},
		{"accepted-missing-credential", "accepts", "", "", "negative-control-refusal-failed"},
		{"unrelated-exit", "unrelated", "", "", "negative-control-refusal-failed"},
		{"missing-input", "async", "", "signal.token", "negative-control-setup-failed"},
		{"restore-failed", "async", "broken", "", "negative-control-restore-failed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			d, r, diagnostics := credentialControlFixture(t, row.mode, row.restore)
			if row.remove != "" {
				if err := os.Remove(filepath.Join(r.dir, row.remove)); err != nil {
					t.Fatal(err)
				}
			}
			err := d.NegativeControls(context.Background())
			if row.reason == "" && err != nil || row.reason != "" && (err == nil || err.Error() != row.reason) {
				t.Fatal("MUTATION: credential-refusal-result", err, diagnostics.String())
			}
			for _, want := range []string{"control=missing-credential", "relais-signal.service", "credential=", "expected=", "exit_status=", "ActiveState="} {
				if !strings.Contains(diagnostics.String(), want) {
					t.Fatal("missing diagnostic", want, diagnostics.String())
				}
			}
			if row.remove == "" {
				data, e := os.ReadFile(filepath.Join(r.dir, "signal.token"))
				if e != nil || string(data) != "fixture-token\n" {
					t.Fatal("credential not restored", e)
				}
				if _, e := os.Stat(filepath.Join(r.dir, "signal.token.saved")); !os.IsNotExist(e) {
					t.Fatal("backup left behind", e)
				}
			}
			if row.reason != "" {
				for _, label := range r.commands {
					if label == "negative-missing-unit" {
						t.Fatal("continued after failed control")
					}
				}
			}
			if strings.Contains(diagnostics.String(), "fixture-token") {
				t.Fatal("credential contents were logged")
			}
		})
	}
}

func TestMissingUnitControlRestoresAndVerifies(t *testing.T) {
	for _, row := range []struct{ name, mode, restore, reason string }{
		{"refusal", "async", "", ""},
		{"accepted", "accepts", "", "negative-control-refusal-failed"},
		{"restore-failed", "async", "broken", "negative-control-restore-failed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			d, r, diagnostics := credentialControlFixture(t, row.mode, row.restore)
			r.control = "missing-unit"
			err := d.NegativeControls(context.Background())
			if row.reason == "" && err != nil || row.reason != "" && (err == nil || err.Error() != row.reason) {
				t.Fatal("unit control result", err, diagnostics.String())
			}
			raw, e := os.ReadFile(filepath.Join(r.dir, "worker.service"))
			if e != nil || string(raw) != "fixture-unit\n" {
				t.Fatal("unit not restored", e)
			}
			if _, e := os.Stat(filepath.Join(r.dir, "worker.service.saved")); !os.IsNotExist(e) {
				t.Fatal("unit backup left behind", e)
			}
			for _, want := range []string{"relais-worker.service", "LoadState=not-found", "restore_verified=", "exit_status="} {
				if !strings.Contains(diagnostics.String(), want) {
					t.Fatal("missing unit diagnostic", want, diagnostics.String())
				}
			}
		})
	}
}

type aclControlRunner struct {
	bootRunner
	mode                   string
	negativeSeen, restored bool
}

func (r *aclControlRunner) Run(ctx context.Context, c Command) CommandResult {
	if c.Label == "negative-wrong-acl" {
		r.negativeSeen = true
		if r.mode == "observation-error" {
			return CommandResult{Code: code(1)}
		}
		if r.mode == "unexpected-acceptance" {
			return CommandResult{Code: code(0), Stdout: []byte("OK\n1\n")}
		}
	}
	if c.Label == "store-admin" && r.negativeSeen {
		r.restored = true
		if r.mode == "restore-error" {
			return CommandResult{Code: code(1)}
		}
	}
	return r.bootRunner.Run(ctx, c)
}
func TestACLControlRestoresAfterObservationFailure(t *testing.T) {
	for _, mode := range []string{"observation-error", "unexpected-acceptance", "restore-error", "success"} {
		t.Run(mode, func(t *testing.T) {
			runner := &aclControlRunner{mode: mode}
			var diagnostics bytes.Buffer
			d := &Deployment{Topology: fixtureTopology(t), Runner: runner, Diagnostics: &diagnostics, containerIDs: fixtureContainerIDs()}
			err := d.checkWrongACL(context.Background())
			if (err == nil) != (mode == "success") || !runner.restored || d.aclNegative != (err == nil) {
				t.Fatal("ACL restoration/accounting", err, diagnostics.String())
			}
			for _, want := range []string{"unit=none", "credential=stdin", "expected=NOPERM", "exit_status=", "restore_verified="} {
				if !strings.Contains(diagnostics.String(), want) {
					t.Fatal("missing ACL diagnostic", want, diagnostics.String())
				}
			}
			for _, secret := range d.Topology.Secrets {
				if strings.Contains(diagnostics.String(), secret) {
					t.Fatal("ACL diagnostic leaked a secret")
				}
			}
		})
	}
}

type restoreVerifyRunner struct{ bootRunner }

func (r *restoreVerifyRunner) Run(ctx context.Context, c Command) CommandResult {
	if c.Label == "verify-after-controls-shared" {
		return CommandResult{Code: code(1)}
	}
	return r.bootRunner.Run(ctx, c)
}
func TestNegativeControlsRequireVerifiedHosts(t *testing.T) {
	runner := &restoreVerifyRunner{}
	var diagnostics bytes.Buffer
	d := &Deployment{Topology: fixtureTopology(t), Runner: runner, Diagnostics: &diagnostics, aclNegative: true, containerIDs: fixtureContainerIDs()}
	if err := d.NegativeControls(context.Background()); err == nil || err.Error() != "negative-control-verification-failed" {
		t.Fatal("host restore was assumed", err)
	}
	if !strings.Contains(diagnostics.String(), "verify-after-controls-shared failed exit_status=1") {
		t.Fatal("empty verification failure", diagnostics.String())
	}
}
func TestNegativeControlEmptyCommandFailureHasContext(t *testing.T) {
	for _, result := range []CommandResult{{Code: code(1)}, {}} {
		var diagnostics bytes.Buffer
		d := &Deployment{Topology: fixtureTopology(t), Runner: &fakeRunner{result: result}, Diagnostics: &diagnostics, aclNegative: true, containerIDs: fixtureContainerIDs()}
		if err := d.NegativeControls(context.Background()); err == nil {
			t.Fatal("empty failed command accepted")
		}
		for _, want := range []string{"host=shared", "unit=relais-signal.service", "credential=/run/relais/signal.token", "expected=", "exit_status="} {
			if !strings.Contains(diagnostics.String(), want) {
				t.Fatal("empty negative-control failure", want, diagnostics.String())
			}
		}
	}
}
