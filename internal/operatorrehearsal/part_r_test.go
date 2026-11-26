package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorevidence/relayjudge"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/artifacts"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func relayLossPin() artifacts.Pin {
	return artifacts.Pin{Version: ReleaseRVersion, Tag: "v0.4.0", Arch: "x86_64", Commit: ReleaseRCommit, Archive: "relais_0.4.0_linux_amd64.tar.gz", ArchiveSHA256: ReleaseRSHA256, ManifestSHA256: strings.Repeat("a", 64)}
}
func TestPartRReleasePin(t *testing.T) {
	pin := relayLossPin()
	require.NoError(t, validateRelayLossPin(pin))
	for _, mutate := range []func(*artifacts.Pin){func(p *artifacts.Pin) { p.Version = "0.3.5" }, func(p *artifacts.Pin) { p.Commit = ReleaseACommit }, func(p *artifacts.Pin) { p.Arch = "arm64" }, func(p *artifacts.Pin) { p.Tag = "v0.3.5" }, func(p *artifacts.Pin) { p.ArchiveSHA256 = ReleaseBSHA256 }, func(p *artifacts.Pin) { p.Archive = "../archive" }, func(p *artifacts.Pin) { p.ManifestSHA256 = "" }} {
		changed := pin
		mutate(&changed)
		require.Error(t, validateRelayLossPin(changed))
	}
	dir := t.TempDir()
	raw, _ := json.Marshal(pin)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "release-r.json"), raw, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, pin.Archive), []byte("corrupted archive fixture"), 0600))
	_, err := readRelayLossRelease(dir)
	require.EqualError(t, err, "release-digest-differs")
	// A pair plan cannot be consumed as a single pin, or vice versa.
	raw, _ = json.Marshal(comparisonPlan())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "release-r.json"), raw, 0600))
	_, err = readRelayLossRelease(dir)
	require.Error(t, err)
}

func partRTopology(t *testing.T) *Topology {
	t.Helper()
	c := fixtureConfig()
	c.RelayTwo = true
	c.ArchiveSHA256 = ReleaseRSHA256
	topology, err := Load(os.DirFS("../.."), c)
	require.NoError(t, err)
	return topology
}
func TestPartRTopologyAndStaging(t *testing.T) {
	topology := partRTopology(t)
	require.Len(t, topology.Hosts, 5)
	require.Equal(t, []string{"relay", "w1", "w2", "shared", "relay-two"}, topology.Slots())
	require.Equal(t, "RelayTwoHost", topology.Hosts["relay-two"].Resource)
	for _, slot := range topology.Slots() {
		require.Contains(t, topology.Hosts[slot].Metadata.Files["/etc/relais/node.env"].Content, "RELAY_TWO_PRIVATE=10.42.1.10\n")
		_, err := topology.Bundle(slot)
		require.NoError(t, err)
	}
	require.Contains(t, topology.Hosts["relay-two"].Metadata.Files["/etc/relais/node.env"].Content, "RELAY_PUBLIC=10.42.1.10\n")
	require.Contains(t, topology.Hosts["relay-two"].Metadata.Files["/etc/relais/node.env"].Content, "RELAY_SLOT=preview-two\n")
	run := &assemblyRunner{}
	containers := map[string]string{}
	for _, slot := range topology.Slots() {
		containers[slot] = strings.Repeat("a", 64)
	}
	d := &Deployment{Topology: topology, Runner: run, containerIDs: containers}
	r := &rehearsal{c: RehearseConfig{PartR: true, Assets: "/verified"}, topology: topology, deployment: d, runner: run, diagnostics: io.Discard, pins: []inventory.Pin{{Version: ReleaseRVersion, ArchiveSHA256: ReleaseRSHA256}}}
	require.Equal(t, "http://10.42.1.10:24700", r.parameters().RelayTwo)
	require.NoError(t, r.stage(context.Background()))
	require.Len(t, run.commands, 10)
	for _, c := range run.commands {
		require.NotContains(t, strings.Join(c.Args, " ")+string(c.Input), "0.3.")
	}
	// The fifth host uses the same privileged systemd image and namespace as the
	// other relay. This is a command proof; actual boot remains runner-only.
	d.Layout = CPULayout{Services: []int{2, 3}}
	d.networkID = strings.Repeat("b", 64)
	args := d.runArgs("relay-two", topology.Hosts["relay-two"].IP, "image", true)
	require.Contains(t, args, "image")
	script, err := topology.VerifyScript("relay-two")
	require.NoError(t, err)
	require.Contains(t, script, "systemctl is-active --quiet 'relais-relay.service'")
}
func TestPartRScenarioBindsHeadToolsAndSingleRelease(t *testing.T) {
	head := strings.Repeat("c", 40)
	sha := strings.Repeat("d", 64)
	r := &rehearsal{c: RehearseConfig{Commit: head}, report: &Report{TemplateSHA256: strings.Repeat("e", 64), CallerSHA256: sha}, relayJudge: operatorevidence.OpenRelayLossJudge(head, sha, "/judge")}
	scenario, err := r.partRScenario()
	require.NoError(t, err)
	require.True(t, scenario.RelayTwo)
	require.Equal(t, []operator.Gate{operator.RelayHostLoss}, scenario.Gates)
	require.Equal(t, scenario.ReleaseA, scenario.ReleaseB)
	require.Equal(t, operator.Identity{SourceCommit: ReleaseRCommit, SHA256: ReleaseRSHA256}, scenario.ReleaseA)
	require.Equal(t, head, scenario.Caller.SourceCommit)
	require.Equal(t, head, scenario.Judge.SourceCommit)
	require.Equal(t, sha, scenario.Judge.SHA256)
}
func passingPartRReport() Report {
	r := Report{PartR: true, SchemaVersion: 1, Event: "workflow_dispatch", ScenarioSHA256: strings.Repeat("a", 64), Cleanup: true}
	result := &relayjudge.Result{RunID: "private-run", Outcome: "pass", Causes: []string{}}
	for i := range 4 {
		result.Calls = append(result.Calls, relayjudge.CallResult{Ordinal: i, Session: "private-session-10.0.0.1", Role: "affected", Outcome: "pass", Causes: []string{}, Gap: time.Second})
	}
	r.Gates = []GateReport{{Gate: operator.RelayHostLoss, ProcedureCompleted: true, CallerStopped: true, ProcedureResult: operatorgates.ProcedureResult{Completed: true, Status: operator.Passed, Process: operatorgates.ProcessEvidence{CompletedSteps: 7, RelayLoss: result}, Media: &operatorgates.MediaSummary{Status: operator.Passed}}}}
	return r
}
func TestPartRReportVerdictsAndPrivacy(t *testing.T) {
	r := passingPartRReport()
	r.Evaluate()
	require.Equal(t, operator.Passed, r.Verdict)
	for _, mode := range []string{"slow", "missing", "inconclusive", "restore", "stop", "unknown", "cleanup", "wrong-gate", "cause", "partial"} {
		r := passingPartRReport()
		g := &r.Gates[0]
		switch mode {
		case "slow":
			g.Process.RelayLoss.Calls[0].Gap = 5*time.Second + 1
		case "missing":
			g.Process.RelayLoss = nil
		case "inconclusive":
			g.Status = operator.Inconclusive
		case "restore":
			g.Process.RelayLoss.Outcome = "fail"
		case "stop":
			g.CallerStopped = false
		case "unknown":
			r.Latched = true
		case "cleanup":
			r.Cleanup = false
		case "wrong-gate":
			g.Gate = operator.Rollout
		case "cause":
			g.Process.RelayLoss.Causes = []string{"relay-loss-missing"}
		case "partial":
			g.ProcedureCompleted = false
		}
		r.Evaluate()
		require.Equal(t, operator.Failure, r.Verdict, mode)
	}
	r = passingPartRReport()
	r.Gates[0].Process.RelayLoss.Calls[1].Causes = []string{"secret-host", "relay-loss-slow"}
	var out bytes.Buffer
	require.NoError(t, r.Write(&out))
	require.NotContains(t, out.String(), "private-")
	require.NotContains(t, out.String(), "10.0.0.1")
	require.NotContains(t, out.String(), "secret-host")
	require.Contains(t, out.String(), "relay-loss-slow")
	require.Contains(t, out.String(), "unclassified")
	var saved Report
	require.NoError(t, json.Unmarshal(out.Bytes(), &saved))
	require.Equal(t, operator.Failure, saved.Verdict)
	require.Len(t, saved.Gates[0].Process.RelayLoss.Calls, 4)
	require.True(t, slices.Contains(saved.Gates[0].Process.RelayLoss.Calls[1].Causes, "relay-loss-slow"))
}

func TestPartRBootIncludesFifthSystemdHost(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "release.tar.gz")
	data := []byte("verified archive command fixture")
	require.NoError(t, os.WriteFile(archive, data, 0600))
	c := fixtureConfig()
	c.RelayTwo = true
	c.ArchiveSHA256 = hash(data)
	topology, err := Load(os.DirFS("../.."), c)
	require.NoError(t, err)
	base := &bootRunner{}
	record := &serialBootRunner{runner: base}
	runner := commandRunner(func(ctx context.Context, cmd Command) CommandResult {
		if cmd.Label == "host-relay-two" { // Capture creation under the existing recorder lock.
			record.mu.Lock()
			base.commands = append(base.commands, cmd)
			record.mu.Unlock()
			return CommandResult{Code: code(0), Stdout: []byte(strings.Repeat("8", 64))}
		}
		return record.Run(ctx, cmd)
	})
	layout, err := Layout([]int{0, 1, 2, 3}, nil)
	require.NoError(t, err)
	d, err := NewDeployment(topology, filepath.Join(dir, "topology"), archive, layout, runner, io.Discard)
	require.NoError(t, err)
	require.NoError(t, d.Boot(t.Context()))
	require.Len(t, d.ContainerIDs(), 8)
	booted := false
	for _, cmd := range base.commands {
		if cmd.Label == "host-relay-two" {
			for _, arg := range []string{"--privileged", "--cgroupns=private", "--tmpfs"} {
				require.Contains(t, cmd.Args, arg)
			}
		}
		if cmd.Label == "boot-relay-two" {
			booted = true
			require.Contains(t, cmd.Args, "/rehearsal/user-data.sh")
		}
	}
	require.True(t, booted)
}
