package operatorrehearsal

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/relaishq/relais/internal/operatorgates/artifacts"
	"github.com/relaishq/relais/pkg/operator"
)

func comparisonPlan() artifacts.Plan {
	p := artifacts.Plan{Version: 1, Arch: "x86_64", From: artifacts.Pin{Version: "0.3.4", Commit: ReleaseACommit, Archive: "relais_0.3.4_linux_amd64.tar.gz", ArchiveSHA256: ReleaseASHA256}, To: artifacts.Pin{Version: "0.3.5", Commit: ReleaseBCommit, Archive: "relais_0.3.5_linux_amd64.tar.gz", ArchiveSHA256: ReleaseBSHA256}}
	for i, pin := range []artifacts.Pin{p.From, p.To, p.From} {
		p.Steps = append(p.Steps, artifacts.Step{Operation: []string{"install", "upgrade", "rollback"}[i], Parameters: artifacts.Parameters{ReleaseVersion: pin.Version, Architecture: p.Arch, ArchiveSHA256: pin.ArchiveSHA256}})
	}
	return p
}

func TestABMCharacterization(t *testing.T) {
	for _, pin := range []artifacts.Pin{comparisonPlan().From, comparisonPlan().To} {
		topology, err := Load(os.DirFS("../.."), Config{Name: "abm-fixture", Version: pin.Version, Architecture: "x86_64", ArchiveSHA256: pin.ArchiveSHA256})
		if err != nil {
			t.Fatal(err)
		}
		if len(topology.Hosts) != 4 {
			t.Fatal(topology.Hosts)
		}
		for _, h := range topology.Hosts {
			if !strings.Contains(h.Metadata.Files["/etc/relais/node.env"].Content, "RELAY_TWO_PRIVATE=\n") {
				t.Fatal("second relay was retained")
			}
		}
	}
	plan := comparisonPlan()
	if err := validatePair(plan); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*artifacts.Plan){func(p *artifacts.Plan) { p.From = p.To }, func(p *artifacts.Plan) { p.To.Commit = ReleaseACommit }, func(p *artifacts.Plan) { p.Arch = "arm64" }, func(p *artifacts.Plan) { p.To.ArchiveSHA256 = ReleaseASHA256 }} {
		changed := plan
		mutate(&changed)
		if validatePair(changed) == nil {
			t.Fatal("pair validation weakened")
		}
	}
	raw, _ := json.MarshalIndent(plan, "", "  ")
	checkABMGolden(t, "abm-plan.json", append(raw, '\n'))
	r := passingReport()
	r.SchemaVersion = 1
	r.Event = "workflow_dispatch"
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	checkABMGolden(t, "abm-report.json", out.Bytes())
	if !reflect.DeepEqual(CoveredGates, []operator.Gate{operator.Rollout, operator.ControlRestart, operator.UpgradeRollback, operator.RelayDeploy}) {
		t.Fatal(CoveredGates)
	}
}

func checkABMGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := "testdata/" + name
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("A/B/M fixture changed: %s", name)
	}
}
