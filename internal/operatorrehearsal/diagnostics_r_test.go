package operatorrehearsal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
)

func TestPartRHostDiagnosticsRedactsUploadedFile(t *testing.T) {
	secret := "fixture-private-diagnostic-token"
	addresses := []string{"10.42.0.10", "192.0.2.91", "[2001:db8::1]:24700", "fe80::1%eth0", "store.rehearsal.test", "node.example.test"}
	for _, partR := range []bool{false, true} {
		name := "comparison"
		if partR {
			name = "part-r"
		}
		t.Run(name, func(t *testing.T) {
			output := t.TempDir()
			_, err := rehearse(context.Background(), RehearseConfig{PartR: partR, Output: output}, io.Discard, func(r *rehearsal, _ context.Context) error {
				r.report.ScenarioSHA256 = strings.Repeat("a", 64)
				r.topology = &Topology{Secrets: map[string]string{"signal": secret}}
				// Host stdout/stderr already pass through the existing redaction. Also
				// cover direct driver writes and a secret split across log writes.
				r.failure("fixture", CommandResult{Code: code(0), Stdout: []byte("connection refused " + strings.Join(addresses, " ") + " " + secret), Stderr: []byte("stderr retained Id=relais-relay.service ActiveState=failed")})
				_, _ = fmt.Fprintln(r.diagnostics, "caller-bind=10.42.0.10:24780 peer=https://node.example.test:443/status")
				_, _ = fmt.Fprintln(r.diagnostics, bindingtest.Pack(map[string]any{"kind": "failed", "code": "connection-refused-error", "endpoint": "http://192.0.2.91:24700", "token": secret}))
				_, _ = fmt.Fprintln(r.diagnostics, "Reference ID : 0A2A003C (10.42.0.10)")
				_, _ = io.WriteString(r.diagnostics, secret[:10])
				_, _ = io.WriteString(r.diagnostics, secret[10:]+"\n")
				if partR {
					raw, err := os.ReadFile(filepath.Join(output, "host-diagnostics.log"))
					if err != nil || len(raw) != 0 {
						t.Fatal("raw diagnostics entered the upload path before projection", err)
					}
				}
				return errors.New("fixture failure")
			})
			if err == nil {
				t.Fatal("fixture must fail before boot")
			}
			raw, err := os.ReadFile(filepath.Join(output, "host-diagnostics.log"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			if !partR {
				if !strings.Contains(text, addresses[0]) {
					t.Fatal("comparison diagnostic bytes changed")
				}
				return
			}
			for _, private := range append(addresses, secret, "0A2A003C", "H4sI") {
				if strings.Contains(text, private) {
					t.Errorf("uploaded diagnostic retained %q", private)
				}
			}
			for _, diagnostic := range []string{"step fixture failed", "connection refused", "stderr retained", "Id=relais-relay.service", "ActiveState=failed", "host observation kind=failed code=connection-refused-error", "[secret]", "[address]"} {
				if !strings.Contains(text, diagnostic) {
					t.Errorf("lost diagnostic %q", diagnostic)
				}
			}
		})
	}
}
