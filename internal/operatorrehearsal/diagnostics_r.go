package operatorrehearsal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/pkg/operator"
)

var diagnosticEnvelope = regexp.MustCompile(`(?m)^H4sI[A-Za-z0-9+/]*={0,2}\r?$`)

var diagnosticURL = regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
var diagnosticHost = regexp.MustCompile(`\b[a-zA-Z0-9](?:[a-zA-Z0-9-]*\.)+[a-zA-Z]{2,63}\b`)
var diagnosticIPv4 = regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?::[0-9]+)?`)
var diagnosticIPv6 = regexp.MustCompile(`[0-9a-fA-F:.%]+`)
var diagnosticReference = regexp.MustCompile(`(?m)(Reference ID\s*:\s*)\S+`)

// Keep existing secret redaction and incomplete-command suppression. The R
// upload projection additionally removes URLs, dotted host names, numeric
// addresses and chrony's encoded reference address. Whole-file projection also
// redacts a secret or address split across consecutive driver writes.
func savePartRDiagnostics(private string, public io.Writer, topology *Topology) error {
	raw, err := os.ReadFile(private)
	if err != nil {
		return err
	}
	if topology == nil {
		topology = &Topology{}
	}
	text := diagnosticEnvelope.ReplaceAllStringFunc(string(raw), publicHostDiagnostic)
	text = topology.Redact(text)
	text = diagnosticURL.ReplaceAllString(text, "[address]")
	text = diagnosticHost.ReplaceAllStringFunc(text, func(name string) string {
		// These fixed public service IDs are useful boot evidence, not host names.
		switch name {
		case "relais-relay.service", "relais-relay-standby.service", "relais-worker.service", "relais-control.service", "relais-signal.service", "relais-prepare.service", "chronyd.service":
			return name
		}
		return "[address]"
	})
	text = diagnosticReference.ReplaceAllString(text, "${1}[address]")
	// Match IPv6 before dotted quads so mapped IPv4-in-IPv6 literals are
	// removed as a whole. Brackets, arrows and label separators split candidates.
	text = diagnosticIPv6.ReplaceAllStringFunc(text, func(token string) string {
		if strings.Count(token, ":") < 2 {
			return token
		}
		// Check within the candidate too: a hex label, zone or extra separator
		// can make the whole token invalid without concealing its address.
		// An unzoned IPv6 literal is at most 45 bytes, including a dotted tail.
		for start := range len(token) {
			for end := min(len(token), start+45); end >= start+2; end-- {
				if address, err := netip.ParseAddr(token[start:end]); err == nil && address.Is6() {
					return "[address]"
				}
			}
		}
		return token
	})
	// No word boundary or surrounding-token validation: labels and punctuation
	// must not prevent a dotted quad from being removed.
	text = diagnosticIPv4.ReplaceAllString(text, "[address]")
	_, err = io.WriteString(public, text)
	return err
}

// Packed host replies can contain private endpoints even when their outer
// base64 text has no visible address. Retain only fixed envelope labels.
func publicHostDiagnostic(line string) string {
	raw, err := host.Unpack(line, operator.MaxHostOutputBytes)
	var envelope struct {
		Kind string `json:"kind"`
		Code string `json:"code"`
	}
	if err != nil || json.Unmarshal(raw, &envelope) != nil {
		return "[encoded host output suppressed]"
	}
	switch envelope.Kind {
	case "ok", "failed", "refused", "unknown":
	default:
		envelope.Kind = "unclassified"
	}
	return fmt.Sprintf("host observation kind=%s code=%s", envelope.Kind, PublicCode(envelope.Code))
}
