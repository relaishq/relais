package mediaworker

import (
	"strings"

	"github.com/relaishq/relais/internal/relaytrace"
)

// Reason codes stay inside the opt-in, in-process trace. The public refusal
// and persisted identity are unchanged. Never trace a raw offer or parse error:
// either can contain ICE passwords, including on malformed input.
func restartMismatch(id, reason, offer string) error {
	traceRestartOffer("worker-restart-refused/"+reason, id, offer)
	return ErrRestartMismatch
}

func traceRestartOffer(event, id, offer string) {
	if !relaytrace.Enabled() {
		return
	}
	var normalized []byte
	if offer != "" {
		// Invalid offers have no normalized evidence. The reason still tells
		// the observer which check refused the request.
		normalized, _ = normalizeRestartOffer(offer)
		// Diagnostic redaction also covers noncanonical attribute spelling;
		// it must not broaden the persisted identity's accepted cases.
		lines := strings.Split(string(normalized), "\r\n")
		kept := lines[:0]
		for _, line := range lines {
			key, _, _ := strings.Cut(line, ":")
			if !strings.EqualFold(key, "a=ice-pwd") {
				kept = append(kept, line)
			}
		}
		normalized = []byte(strings.Join(kept, "\r\n"))
	}
	relaytrace.Emit(event, id, normalized)
}
