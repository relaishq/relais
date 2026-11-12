package operatorrehearsal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestBindAdmission(t *testing.T) {
	for _, mode := range []string{"valid", "refused", "latched", "uncertain", "late-uncertain", "plan-changed", "already-bound"} {
		t.Run(mode, func(t *testing.T) {
			fixtures, pins := idleInventory(t)
			clock := operatorgates.NewRealClock()
			now := clock.Wall()
			f, err := bindingtest.New("admission", now)
			require.NoError(t, err)
			scenario := operator.Scenario{SchemaVersion: 1, Gates: []operator.Gate{operator.Rollout}, ReleaseA: operator.Identity{SourceCommit: pins[0].Commit, SHA256: pins[0].ArchiveSHA256}, ReleaseB: operator.Identity{SourceCommit: pins[1].Commit, SHA256: pins[1].ArchiveSHA256}}
			scenario.Digest, err = operator.PlanIdentity(scenario)
			require.NoError(t, err)
			init := operator.Init{Scenario: scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10)}
			old, err := operatorgates.NewSession("admission", init, clock)
			require.NoError(t, err)
			if mode == "latched" {
				old.Halt("command-result-unknown")
			}
			if mode == "uncertain" {
				old.RecordUncertainty(operator.Uncertainty{Kind: "host.execute", Rule: "lost-reply"})
			}
			dir := t.TempDir()
			raw, _ := json.Marshal(f.Selection)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "clock-selection.json"), raw, 0600))
			raw, _ = json.Marshal(f.Input)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "clock-admission-evidence.json"), raw, 0600))
			topo := fixtureTopology(t)
			containers := fixtureContainerIDs()
			calls := 0
			runner := commandRunner(func(_ context.Context, c Command) CommandResult {
				calls++
				if mode == "late-uncertain" && calls == 1 {
					old.RecordUncertainty(operator.Uncertainty{Kind: "host.execute", Rule: "lost-reply"})
				}
				out := f.Wire(string(c.Input), time.Now())
				if !strings.Contains(string(c.Input), "read_clock(") && !strings.Contains(string(c.Input), "historical_tracking(") {
					slot := ""
					for name, id := range containers {
						if len(c.Args) > 3 && c.Args[3] == id {
							slot = name
						}
					}
					require.NotEmpty(t, slot)
					out = bindingtest.Pack(fixtures[slot])
				}
				return CommandResult{Code: code(0), Stdout: []byte(out)}
			})
			adapter := &Adapter{Topology: topo, Containers: containers, Runner: runner, Diagnostics: io.Discard}
			peer := NewLocalPeer(adapter, "admission", init)
			defer peer.Close()
			bounds := f.Selection.Receipt.Bounds
			r := &rehearsal{run: "admission", clock: clock, boundInit: init, session: old, peer: peer, runner: runner, topology: topo, pins: pins, diagnostics: io.Discard, c: RehearseConfig{Output: dir}, report: &Report{ClockSelection: &ClockSelectionReport{Bounds: &bounds}}, judge: &recordingJudge{pinned: operatorevidence.PinnedJudge{Binary: "/judge"}}}
			r.deployment = &Deployment{Topology: topo, containerIDs: containers}
			r.process = &operatorgates.ProcessConfig{Parameters: r.parameters(), ArmedAt: now}
			r.runtime = &operatorgates.ProcessRuntime{Session: old, Peer: peer, Config: r.process}
			if mode == "refused" {
				r.report.ClockSelection.Cause = "clock-lease-expired"
			}
			// The session was armed with the plan; binding gets a scenario that is not it.
			if mode == "plan-changed" {
				scenario.Judge.SHA256 = "other"
			}
			if mode == "already-bound" {
				scenario.ClockReceiptSHA256 = strings.Repeat("a", 64)
			}
			err = r.bindAdmission(context.Background(), scenario)
			if (mode == "plan-changed" || mode == "already-bound") && (!errors.Is(err, operator.ErrPlanIdentity) || r.boundInit.ClockBinding != nil || r.boundInit.Scenario.ClockReceiptSHA256 != "") {
				t.Fatal("MUTATION: admission-plan-identity", mode, err)
			}
			if mode != "valid" {
				if err == nil || r.session != old || mode != "late-uncertain" && calls != 0 {
					t.Fatal("MUTATION: admission-replacement", mode, err, calls)
				}
				return
			}
			if err != nil || r.session == old || r.runtime.Session != r.session || r.runtime.Leases == nil || r.boundInit.ClockBinding == nil || r.boundInit.Scenario.ClockReceiptSHA256 != f.Selection.SHA256 || r.boundInit.ArmedAtUnixMS != init.ArmedAtUnixMS || r.boundInit.EndUnixMS != init.EndUnixMS || r.report.Bounds != bounds || r.report.ScenarioSHA256 != r.boundInit.Scenario.Digest {
				t.Fatal("MUTATION: bind-admission", err, r.report)
			}
			if !reflect.DeepEqual(r.peer.adapter.LeaseProbeFiles, r.leaseProbeFiles()) || len(r.peer.adapter.LeaseProbeFiles) != 1 {
				t.Fatal("MUTATION: admission-probe-files", r.peer.adapter.LeaseProbeFiles)
			}
			_ = r.peer.Close()
		})
	}
}

// Binding refuses a refused or unreadable selection before it builds an identity.
func TestBindClockAdmissionRefusals(t *testing.T) {
	bounds := clocks.Bounds{FrontendControlBoundNS: 120000, FrontendWorkerBoundNS: 170000, DriftPPM: 100}
	scenario := operator.Scenario{SchemaVersion: 1, Gates: []operator.Gate{operator.Rollout}}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "clock-selection.json"), []byte(`{"receipt":{},"sha256":"","extra":1}`), 0600))
	for cause, status := range map[string]*ClockSelectionReport{
		"clock-binding-missing": {Cause: "clock-select-ceiling", Bounds: &bounds},
		"clock-binding-invalid": {Bounds: &bounds},
	} {
		binding, _, err := BindClockAdmission(dir, status, scenario)
		if err == nil || err.Error() != cause || binding.PlanIdentity != "" || len(binding.ClockBinding.ReceiptGZIP) != 0 {
			t.Fatal("MUTATION: clock-admission-binding-refusal", cause, err)
		}
	}
	if _, _, err := BindClockAdmission(dir, nil, scenario); err == nil || err.Error() != "clock-binding-missing" {
		t.Fatal("MUTATION: clock-admission-binding-refusal", err)
	}
}
