package host

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// runStatus executes a rendered control status script verbatim, with every
// HTTP open refused, and returns the envelope it prints.
func runStatus(t *testing.T, script string) map[string]any {
	t.Helper()
	return runProcess(t, script, `import sys,urllib.request
from unittest.mock import patch
with patch.object(urllib.request.OpenerDirector, 'open', side_effect=ConnectionRefusedError()):
 exec(compile(sys.stdin.read(), '<generated-status>', 'exec'))`)
}

func runProcess(t *testing.T, script, mock string) map[string]any {
	t.Helper()
	body, ok := strings.CutPrefix(script, "python3 - <<'PY'\n")
	if !ok {
		t.Fatal("unexpected script prefix")
	}
	if body, ok = strings.CutSuffix(body, "PY\n"); !ok {
		t.Fatal("unexpected script suffix")
	}
	cmd := exec.Command("python3", "-B", "-c", mock)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status script: %v\n%s", err, out)
	}
	raw, err := Unpack(string(out), 256000)
	if err != nil {
		t.Fatalf("status output: %v\n%s", err, out)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestUnknownMoveWrapperEvidence(t *testing.T) {
	for _, phase := range []string{"status", "post"} {
		t.Run(phase, func(t *testing.T) {
			// Use the shared wrapper without deployment preflight, then run the
			// real move against a fake opener. No socket or systemd is touched.
			script, err := Process("status", map[string]any{"control": "http://192.0.2.1:9", "worker": "w2", "session": "one", "owner": "w1", "epoch": 7}, nil)
			if err != nil {
				t.Fatal(err)
			}
			script = strings.Replace(script, `data=execute("status",`, `data=execute("move",`, 1)
			mock := `import sys,urllib.request,io,json,time
from unittest.mock import patch
def opened(request, **kwargs):
 if '` + phase + `' == 'status' or request.method == 'POST':
  raise TimeoutError('private address and credential text')
 return io.BytesIO(json.dumps({'calls':[{'id':'one','owner':'w1','lease_epoch':7}]}).encode())
with patch.object(urllib.request.OpenerDirector, 'open', side_effect=opened), patch.object(time, 'monotonic', side_effect=[100,200,205.15]):
 exec(compile(sys.stdin.read(), '<generated-move>', 'exec'))`
			got := runProcess(t, script, mock)
			if len(got) != 4 || got["kind"] != "unknown" || got["code"] != "timeout-error" || got["elapsedMs"] != float64(5150) || got["phase"] != phase {
				t.Fatalf("move wrapper lost bounded evidence: %v", got)
			}
		})
	}
}

func TestUnknownExceptionIncludesEvidence(t *testing.T) {
	script, err := Process("status", map[string]string{"control": "http://192.0.2.1:9"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := runStatus(t, script)
	if len(got) != 3 || got["kind"] != "unknown" || got["code"] != "connection-refused-error" {
		t.Fatalf("unknown exception must carry only kind, code and elapsedMs: %v", got)
	}
	if elapsed, ok := got["elapsedMs"].(float64); !ok || elapsed < 0 {
		t.Fatalf("unknown exception lost elapsed time: %v", got)
	}
}

// Control status is effectful: a reconcile can run before the read fails.
// Without the recovery opt-in its exception stays an unknown result.
func TestControlStatusFailureStaysUnknown(t *testing.T) {
	script, err := Process("status", map[string]string{"control": "http://192.0.2.1:9"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, script); got["kind"] != "unknown" {
		t.Fatalf("MUTATION: control-status-unknown: %v", got)
	}
}

func TestPureReadExceptionStaysFailed(t *testing.T) {
	script, err := Process("relay-status", map[string]string{"relay": "http://192.0.2.1:9"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := runStatus(t, script); len(got) != 2 || got["kind"] != "failed" || got["code"] != "connection-refused-error" {
		t.Fatalf("pure read exception changed certainty: %v", got)
	}
}

func TestServiceJournalsWrapperIsPureRead(t *testing.T) {
	script, err := Process("service-journals", struct{}{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := runProcess(t, script, `import sys,subprocess
from unittest.mock import patch
with patch.object(subprocess, 'Popen', side_effect=PermissionError('private journal error')):
 exec(compile(sys.stdin.read(), '<generated-journals>', 'exec'))`)
	if !IsPureRead("service-journals") || got["kind"] != "failed" || got["code"] != "permission-error" || len(got) != 2 {
		t.Fatalf("journal failure changed certainty or leaked text: %v", got)
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
	unknown := strings.Replace(script, "except Exception as error:\n    emit({'kind':'failed','code':read_failure(error)})", "except Exception as error:\n    emit(dict(kind='unknown',code=read_failure(error),elapsedMs=int((time.monotonic()-execution_started)*1000),**evidence))", 1)
	if unknown != plain {
		t.Fatal("MUTATION: recovery-status-scope: scripts differ outside the failure branch")
	}
}
