package releasecompat

import (
	"os"
	"strings"
	"testing"
)

// These fixtures are the unmodified manifests from the checksummed published
// darwin_arm64 v0.3.0 and v0.3.1 archives, not reconstructed old capabilities.
func publishedManifest(t *testing.T, version string) Manifest {
	t.Helper()
	data, err := os.ReadFile("testdata/release-" + version + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := Decode(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestDialOutKeepsPublishedInboundPairs(t *testing.T) {
	for _, version := range []string{"0.3.0", "0.3.1"} {
		t.Run(version, func(t *testing.T) {
			old := publishedManifest(t, version)
			next, err := Generate("../..", "0.3.2", strings.Repeat("f", 40), old.OS, old.Arch)
			if err != nil {
				t.Fatal(err)
			}
			next.TestedPeers = []string{"0.3.0", "0.3.1", "0.3.2"}
			for _, test := range []struct {
				from, to Manifest
				op       string
			}{{old, next, "upgrade"}, {next, old, "rollback"}} {
				verdict := Preflight(testPool(test.from), test.to, test.op)
				if !verdict.Allowed {
					t.Fatalf("published %s %s refused: %+v", version, test.op, verdict)
				}
			}
		})
	}
}
func TestDialOutRequiresRelayAndControlCapability(t *testing.T) {
	m := testManifest(t, "0.3.1")
	m.OS = "darwin"
	pool := testPool(m)
	pool.Participants[1].Profile = "dial-out"
	if err := checkState(pool.Participants, nil); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"relay", "control"} {
		t.Run(role, func(t *testing.T) {
			pool := testPool(m)
			pool.Participants[1].Profile = "dial-out"
			for i, p := range pool.Participants {
				if p.Role == role {
					pool.Participants[i].Manifest = publishedManifest(t, "0.3.0")
					break
				}
			}
			if err := checkState(pool.Participants, nil); err == nil || !strings.Contains(err.Error(), "dial_out_unsupported") {
				t.Fatalf("missing %s capability permitted: %v", role, err)
			}
		})
	}
	next := testManifest(t, "0.4.0")
	next.OS = m.OS
	verdict := Preflight(pool, next, "upgrade")
	if !verdict.Allowed {
		t.Fatal(verdict.Refusal)
	}
	if verdict.Steps[0].Profile != "dial-out" && verdict.Steps[1].Profile != "dial-out" {
		t.Fatal("upgrade changed a dial-out worker's mode")
	}
	requireRefused(t, Preflight(pool, publishedManifest(t, "0.3.0"), "rollback"), "dial_out_unsupported")
	pool = testPool(m)
	pool.TargetWorkerProfile = "dial-out"
	requireRefused(t, Preflight(pool, next, "upgrade"), "worker_mode_change")
}
