package operatorcaller

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// FailureCode emits fixed codes for public caller evidence, never error text
// containing an endpoint, response body, credential or adapter-local path.
func FailureCode(err error) string {
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var path *os.PathError
	var exit *exec.ExitError
	switch {
	case errors.As(err, &authority):
		return "caller-signaling-untrusted-certificate"
	case errors.As(err, &hostname):
		return "caller-signaling-certificate-name"
	case errors.As(err, &invalid):
		return "caller-signaling-certificate-invalid"
	case errors.Is(err, context.DeadlineExceeded):
		return "caller-deadline"
	case errors.Is(err, context.Canceled):
		return "caller-interrupted"
	case errors.As(err, &path):
		return "caller-file-unavailable"
	case errors.As(err, &exit):
		return fmt.Sprintf("caller-load-exit-%d", exit.ExitCode())
	}
	for _, code := range strings.Split(err.Error(), "\n") {
		switch code {
		case "invalid-load-hash", "load-hash-differs", "invalid-caller-ca", "invalid-caller-token", "host-publication-unknown", "setup-publication-unknown", "begin-deadline", "invalid-begin", "feed-before-begin", "conductor-stopped", "caller-stream-failed", "truncated-caller-stdout", "caller-readiness-count", "ready-publication-unknown", "stats-publication-unknown", "caller-stats-exhausted", "invalid-caller-feed", "ack-publication-unknown", "caller-sequence-exhausted", "caller-report-size", "report-publication-unknown", "finished-publication-unknown", "caller-evidence-incomplete", "caller-signaling-request-invalid", "caller-signaling-body-unavailable":
			return code
		}
		if regexp.MustCompile(`^caller-signaling-http-[1-5][0-9]{2}$`).MatchString(code) {
			return code
		}
		if strings.HasPrefix(code, "caller-signaling-probe: ") {
			return "caller-signaling-unavailable"
		}
	}
	return "caller-operation-failed"
}
