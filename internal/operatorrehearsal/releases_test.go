package operatorrehearsal

import (
	"errors"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/artifacts"
)

func TestValidatePairRefusalCode(t *testing.T) {
	var refusal *artifacts.Refusal
	if err := validatePair(artifacts.Plan{}); !errors.As(err, &refusal) || refusal.Code != "release-profile-differs" {
		t.Fatal(err)
	}
}

// Every fixed release-read refusal code stays public, so a diagnostic can tell
// a transient read error from a manifest, digest or compatibility refusal.
func TestReleaseRefusalCodesArePublic(t *testing.T) {
	for _, code := range []string{"architecture-invalid", "architecture-mismatch", "archive-digest-mismatch", "archive-invalid", "asset-ambiguous", "asset-digest-mismatch", "asset-incomplete", "asset-missing", "asset-size", "checksums-invalid", "manifest-identity", "manifest-invalid", "manifest-missing", "manifest-policy-drift", "pagination-invalid", "planned-digest-invalid", "planned-digest-mismatch", "read-failed", "read-rate-limited", "release-changed", "release-draft", "release-missing", "release-prerelease", "reply-invalid", "reply-mismatch", "version-invalid", "version-order", "version-same", "version-unstable", "version-unsupported", "upgrade-refused", "rollback-refused", "release-profile-differs"} {
		if got := PublicCode(code); got != code {
			t.Errorf("PublicCode(%q) = %q", code, got)
		}
	}
}
