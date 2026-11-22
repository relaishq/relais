package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/pkg/operator"
)

func TestAbortedCompletionArtifactKeepsFixedReason(t *testing.T) {
	for _, reason := range []string{"generator-saturation-hard-ceiling", "private free text"} {
		var pool operatorevidence.Pool
		raw := `{"runId":"run-private","phases":[{"callerJsonl":"","events":[],"receipts":[],"callerCompletion":{"runId":"run-private","job":"caller-1","done":false,"stopped":true,"exitCode":1,"sessions":["private-session"],"stopReason":"` + reason + `"}}]}`
		if err := json.Unmarshal([]byte(raw), &pool); err != nil {
			t.Fatal(err)
		}
		out, err := SanitizeMediaEvidence(pool)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"callerCompletion":`) || !strings.Contains(string(out), `"stopped":true`) || !strings.Contains(string(out), `"exitCode":1`) || strings.Contains(string(out), "private-session") || strings.Contains(string(out), "run-private") || strings.Contains(string(out), "private free text") {
			t.Fatalf("completion receipt lost or leaked details: %s", out)
		}
		if reason == "generator-saturation-hard-ceiling" && !strings.Contains(string(out), `"stopReason":"generator-saturation-hard-ceiling"`) {
			t.Fatalf("fixed stop reason was obscured: %s", out)
		}
		if reason == "private free text" && !strings.Contains(string(out), `"stopReason":"unavailable"`) {
			t.Fatalf("unknown stop reason did not use the fixed unavailable value: %s", out)
		}
	}
}

func TestReportCallerStopReasons(t *testing.T) {
	for _, reason := range []string{"generator-saturation-hard-ceiling", "generator-saturation-sustained", "generator-saturation-budget", "invalid-generator-statistics", "caller-stream-failure", "caller-supervision-failure", "private endpoint or token"} {
		r := passingReport()
		r.Uncertainties = []operator.Uncertainty{{Kind: "caller.collect", RequestID: "1", Target: "caller", CallerID: "2", Action: "collect", Rule: "caller-stopped", Code: reason}}
		var out bytes.Buffer
		if err := r.Write(&out); err != nil {
			t.Fatal(err)
		}
		var saved Report
		if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
			t.Fatal(err)
		}
		want := reason
		if reason == "private endpoint or token" {
			want = "unavailable"
		}
		if len(saved.Uncertainties) != 1 || saved.Uncertainties[0].Code != want || saved.Uncertainties[0].CallerID != "2" || saved.Uncertainties[0].Rule != "caller-stopped" || strings.Contains(out.String(), "private endpoint or token") {
			t.Fatalf("caller stop report lost its fixed cause: %s", out.String())
		}
		if saved.Verdict == operator.Passed || !slices.Contains(saved.Failures, "command-result-unknown") {
			t.Fatal("caller stop weakened the report failure policy", saved)
		}
	}
}
