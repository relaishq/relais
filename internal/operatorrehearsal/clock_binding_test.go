package operatorrehearsal

import (
	"context"
	"encoding/json"
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
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestBindAdmission(t *testing.T) {
	for _, mode := range []string{"valid", "refused", "latched", "uncertain", "late-uncertain"} {
		t.Run(mode, func(t *testing.T) {
			fixtures, pins := idleInventory(t)
			clock := operatorgates.NewRealClock()
			now := clock.Wall()
			f, err := bindingtest.New("admission", now)
			require.NoError(t, err)
			scenario := operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}, ReleaseA: operator.Identity{SourceCommit: pins[0].Commit, SHA256: pins[0].ArchiveSHA256}, ReleaseB: operator.Identity{SourceCommit: pins[1].Commit, SHA256: pins[1].ArchiveSHA256}}
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
			err = r.bindAdmission(context.Background(), scenario)
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
