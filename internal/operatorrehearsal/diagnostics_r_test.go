package operatorrehearsal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
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
			assertNoDiagnosticAddresses(t, text)
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

func TestPartRDiagnosticAddressNeighbours(t *testing.T) {
	cases := []string{
		"10.42.0.10:24700->10.42.0.11:80",
		"[10.42.0.10]:24700",
		"172.17.0.2:24700]",
		"src=10.42.0.10:24700-",
		"a_10.42.0.10",
		"10.42.0.10_",
		"host-10.42.0.10",
		"udp4 10.42.0.10:24700:",
		"addr 10.42.0.10/24",
		"[2001:db8::1]:24700->[2001:db8::2]:80",
		"2001:db8::1->2001:db8::2",
		"[2001:db8::1]:24700]",
		"2001:db8::1]",
		"src=[2001:db8::1]:24700-",
		"src=2001:db8::1-",
		"a_2001:db8::1",
		"a2001:db8::1_",
		"2001:db8::1:24700]",
		"2001:db8::1_",
		"udp6 [2001:db8::1]:24700:",
		"[::ffff:10.42.0.10]:80",
		"::ffff:10.42.0.10",
		"fe80::1%eth0:24700",
		"[fe80::1%eth0]:24700->[fe80::2%en0]:80",
		"a_fe80::1%enp0s1-",
		"2001:db8:0:1:2:3:4:5->2001:db8:0:1:2:3:4:6",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			private := filepath.Join(t.TempDir(), "capture")
			transcript := "read udp " + input + ": connection refused\n"
			if err := os.WriteFile(private, []byte(transcript), 0600); err != nil {
				t.Fatal(err)
			}
			var public bytes.Buffer
			if err := savePartRDiagnostics(private, &public, nil); err != nil {
				t.Fatal(err)
			}
			assertNoDiagnosticAddresses(t, public.String())
			if !strings.Contains(public.String(), "connection refused") {
				t.Fatal("failure detail lost", public.String())
			}
		})
	}
}

// Scan every substring of each output field with the standard address parser.
// This deliberately does not reuse the production tokenizer: punctuation or
// labels must not hide an address inside a larger, invalid token.
func assertNoDiagnosticAddresses(t *testing.T, text string) {
	t.Helper()
	for _, field := range strings.Fields(text) {
		for start := range len(field) {
			for end := start + 1; end <= len(field); end++ {
				if _, err := netip.ParseAddr(field[start:end]); err == nil {
					t.Errorf("address remains in projected output: %q in %q", field[start:end], text)
					return
				}
			}
		}
	}
}
