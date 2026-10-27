package host

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPythonHostContracts(t *testing.T) {
	cmd := exec.Command("python3", "-B", "-m", "unittest", "discover", "-p", "*_test.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host contracts: %v\n%s", err, out)
	}
}

// The shared manifest selects each original Python test through a named Go
// subtest. Errors and syntax failures deliberately omit the assertion marker.
func TestPythonGuards(t *testing.T) {
	data, err := os.ReadFile("../../../scripts/ci/gate-mutation/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ ID, PythonTest string }
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.PythonTest == "" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			cmd := exec.Command("python3", "-B", "-m", "unittest", c.PythonTest)
			out, err := cmd.CombinedOutput()
			if err == nil {
				return
			}
			parts := strings.Split(c.PythonTest, ".")
			method := parts[len(parts)-1]
			text := string(out)
			if strings.Contains(text, "FAIL: "+method+" (") && strings.Contains(text, "AssertionError:") && strings.Contains(text, "FAILED (failures=") && !strings.Contains(text, "ERROR:") {
				t.Fatalf("MUTATION: python-guard %s\n%s", c.ID, out)
			}
			t.Fatalf("INVALID Python guard %s: %v\n%s", c.ID, err, out)
		})
	}
}
