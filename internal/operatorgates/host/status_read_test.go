package host

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// runStatus executes a rendered control status script verbatim, with every
// HTTP open refused, and returns the envelope it prints.
func runStatus(t *testing.T, script string) map[string]string {
	t.Helper()
	body, ok := strings.CutPrefix(script, "python3 - <<'PY'\n")
	if !ok {
		t.Fatal("unexpected script prefix")
	}
	if body, ok = strings.CutSuffix(body, "PY\n"); !ok {
		t.Fatal("unexpected script suffix")
	}
	cmd := exec.Command("python3", "-B", "-c", `import sys,urllib.request
from unittest.mock import patch
with patch.object(urllib.request.OpenerDirector, 'open', side_effect=ConnectionRefusedError()):
 exec(compile(sys.stdin.read(), '<generated-status>', 'exec'))`)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status script: %v\n%s", err, out)
	}
	raw, err := Unpack(string(out), 256000)
	if err != nil {
		t.Fatalf("status output: %v\n%s", err, out)
	}
	var envelope map[string]string
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

// Control status is effectful: a reconcile can run before the read fails.
// Without the recovery opt-in its exception stays an unknown result.
func TestControlStatusFailureStaysUnknown(t *testing.T) {
	script, err := Process("status", map[string]string{"control": "http://192.0.2.1:9"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, script); len(got) != 1 || got["kind"] != "unknown" {
		t.Fatalf("MUTATION: control-status-unknown: %v", got)
	}
}

// The failover recovery wait opts in to a definite failed read with a fixed
// code. Only the exception branch differs from the ordinary status script.
func TestRecoveryStatusReportsFailedRead(t *testing.T) {
	parameters := map[string]string{"control": "http://192.0.2.1:9"}
	script, err := RecoveryStatus(parameters)
	if err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, script); len(got) != 2 || got["kind"] != "failed" || got["code"] != "connection-refused-error" {
		t.Fatalf("MUTATION: recovery-status-failed-read: %v", got)
	}
	plain, err := Process("status", parameters, nil)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(script, "except Exception as error:\n    emit({'kind':'failed','code':read_failure(error)})", "except Exception:\n    emit({'kind':'unknown'})", 1)
	if unknown != plain {
		t.Fatal("MUTATION: recovery-status-scope: scripts differ outside the failure branch")
	}
}
