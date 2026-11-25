package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/relayloss"
)

func TestRelayHostLossRunsNetnsAndRequiresConfirmedCompletion(t *testing.T) {
	for _, mode := range []string{"pass", "over-budget", "missing-report", "failed", "unknown", "failed-phase", "private-phase"} {
		t.Run(mode, func(t *testing.T) {
			work := t.TempDir()
			var diagnostics bytes.Buffer
			r := &rehearsal{c: RehearseConfig{RedisExecutable: "/pinned/redis-server"}, work: work, bridge: filepath.Join(work, "relais-gates"), diagnostics: &diagnostics}
			r.runner = commandRunner(func(ctx context.Context, c Command) CommandResult {
				for _, arg := range []string{"-topology=netns", "-relay-loss-netns", "-runs=1", "-after=5s", "-relay-loss-report=" + filepath.Join(work, "relay-loss.json"), "-redis-server=/pinned/redis-server"} {
					if !slices.Contains(c.Args, arg) {
						t.Fatal("missing fault contract", arg)
					}
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 3*time.Minute {
					t.Fatal("unbounded drill")
				}
				if mode != "missing-report" {
					e := relayloss.Report{Completed: true}
					for i := range relayloss.CallCount {
						e.Calls = append(e.Calls, relayloss.Call{Number: i + 1, Gap: time.Second, RecoveryGap: 500 * time.Millisecond, Recovered: true})
					}
					if mode == "failed-phase" {
						e.Completed = false
						e.FailedPhase = "recovery"
					}
					if mode == "private-phase" {
						e.Completed = false
						e.FailedPhase = "private-secret-value"
					}
					if mode == "over-budget" {
						e.Calls[7].Gap = 6 * time.Second
					}
					data, err := json.Marshal(e)
					if err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(filepath.Join(work, "relay-loss.json"), data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "failed" {
					return CommandResult{Code: code(1)}
				}
				if mode == "unknown" {
					return CommandResult{}
				}
				return CommandResult{Code: code(0)}
			})
			report := r.runRelayLoss(t.Context())
			if report.Passed() != (mode == "pass") {
				t.Fatal("wrong host-loss verdict", report)
			}
			if mode == "failed-phase" && (report.FailedPhase != "recovery" || !strings.Contains(diagnostics.String(), "failed_phase=recovery")) {
				t.Fatal("failed phase missing", report, diagnostics.String())
			}
			if mode == "private-phase" && (report.FailedPhase != "unavailable" || strings.Contains(diagnostics.String(), "private-secret-value")) {
				t.Fatal("unsafe phase reached diagnostics", diagnostics.String())
			}
			if mode == "pass" && report.Calls[0].RecoveryGap != 500*time.Millisecond {
				t.Fatal("recovery gap lost", report)
			}
		})
	}
}

func TestPinnedTopologyKeepsSingleRelayComparison(t *testing.T) {
	topology := fixtureTopology(t)
	for _, host := range topology.Hosts {
		settings := host.Metadata.Files["/etc/relais/node.env"].Content
		if !strings.Contains(settings, "RELAY_TWO_PRIVATE=\n") {
			t.Fatal("pinned release was enrolled into a head relay set")
		}
	}
}
