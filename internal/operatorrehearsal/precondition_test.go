package operatorrehearsal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

type idlePeer struct {
	request operator.Request
	respond func(operator.Request) operator.Reply
}

func (p *idlePeer) Send(_ context.Context, f operator.Frame) error {
	p.request = *f.Request
	return nil
}
func (p *idlePeer) Receive(context.Context) (operator.Frame, error) {
	r := p.respond(p.request)
	r.ID = p.request.ID
	return operator.Frame{Type: "result", Session: "idle", Result: &r}, nil
}
func (*idlePeer) Poll() (operator.Frame, error, bool)                      { return operator.Frame{}, nil, false }
func (*idlePeer) Close() error                                             { return nil }
func (*idlePeer) Finish(context.Context, func(error) operator.Frame) error { return nil }

func TestCalibrationCleanupChecksNativeDeploymentState(t *testing.T) {
	fixtures := map[string]json.RawMessage{}
	var pins []inventory.Pin
	for _, c := range oracletest.Cases(t, "inventory.parseInventory") {
		if c.Result.Completion != "returned" {
			continue
		}
		slot := oracletest.JSON[string](t, c.Request.Inputs[0])
		if _, ok := fixtures[slot]; ok {
			continue
		}
		var encoded struct {
			Value struct {
				ObservedAt   int64                       `json:"observedAt"`
				Participants []releasecompat.Participant `json:"participants"`
			} `json:"value"`
		}
		if err := json.Unmarshal([]byte(oracletest.JSON[string](t, c.Request.Inputs[1])), &encoded); err != nil {
			t.Fatal(err)
		}
		for i := range encoded.Value.Participants {
			encoded.Value.Participants[i].Manifest.OS = "linux"
			encoded.Value.Participants[i].Manifest.Arch = "amd64"
		}
		fixtures[slot] = oracletest.Marshal(t, encoded.Value)
		pins = oracletest.JSON[[]inventory.Pin](t, c.Request.Inputs[2])
	}
	for i := range pins {
		pins[i].Architecture = "x86_64"
		pins[i].BootSHA256 = strings.Repeat("d", 64)
	}
	for _, mode := range []string{"idle", "calls", "reservations", "worker-calls", "dead", "draining", "local-sessions", "inventory-unknown", "inventory-architecture", "status-shape"} {
		t.Run(mode, func(t *testing.T) {
			clock := operatorgates.NewRealClock()
			now := clock.Wall()
			scenario := operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates}
			scenario.ReleaseA = operator.Identity{SourceCommit: pins[0].Commit, SHA256: pins[0].ArchiveSHA256}
			scenario.ReleaseB = operator.Identity{SourceCommit: pins[1].Commit, SHA256: pins[1].ArchiveSHA256}
			init := operator.Init{Scenario: scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute}}
			session, err := operatorgates.NewSession("idle", init, clock)
			if err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			r := &rehearsal{clock: clock, session: session, topology: fixtureTopology(t), diagnostics: &log}
			r.process = &operatorgates.ProcessConfig{Parameters: r.parameters(), ArmedAt: now}
			s := controlplane.Status{Ready: true}
			for _, name := range []string{"w1", "w2"} {
				s.Workers = append(s.Workers, controlplane.WorkerStatus{Name: name, Address: r.process.Parameters.Workers[name].Media})
			}
			switch mode {
			case "calls":
				s.Calls = []controlplane.CallStatus{{ID: "calibration", Owner: "w1", Epoch: 1}}
			case "reservations":
				s.Pool.Reservations = 1
			case "worker-calls":
				s.Workers[0].Calls = 1
			case "dead":
				s.Workers[0].Dead = true
			case "draining":
				s.Workers[0].Draining = true
			}
			var requests []string
			peer := &idlePeer{respond: func(req operator.Request) operator.Reply {
				requests = append(requests, req.Target+":"+req.Action)
				var data []byte
				if req.Action == "inventory" {
					data = fixtures[req.Target]
					if mode == "inventory-architecture" {
						data = bytes.ReplaceAll(data, []byte(`"arch":"amd64"`), []byte(`"arch":"arm64"`))
					}
				} else {
					var product any = s
					if req.Action == "worker-status" {
						local := mediaworker.WorkerStatus{InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}}
						if mode == "local-sessions" {
							local.Sessions = 1
						}
						if mode == "inventory-unknown" {
							local.InventoryKnown = false
						}
						product = local
					}
					if mode == "status-shape" {
						product = struct{}{}
					}
					data = oracletest.Marshal(t, map[string]any{"kind": "ok", "started": now, "ended": now, "data": product})
				}
				var compressed bytes.Buffer
				z := gzip.NewWriter(&compressed)
				_, _ = z.Write(data)
				_ = z.Close()
				return operator.Reply{Transport: operator.Succeeded, ExitCode: code(0), Payload: oracletest.Marshal(t, operator.HostOutput{Stdout: base64.StdEncoding.EncodeToString(compressed.Bytes())})}
			}}
			r.runtime = &operatorgates.ProcessRuntime{Session: session, Peer: peer, Config: r.process, Pins: pins, Architecture: "x86_64"}
			session.SetObservationFailureSink(r.preconditionDiagnostic)
			err = r.verifyIdle(context.Background(), time.Minute)
			if (err == nil) != (mode == "idle") {
				t.Fatal(mode, err, log.String())
			}
			if mode == "idle" {
				if len(requests) != 7 || !strings.Contains(log.String(), "calibration-idle-verified") {
					t.Fatal(requests, log.String())
				}
			} else if !strings.Contains(log.String(), "precondition-check host=") || !strings.Contains(log.String(), "observation=") || strings.Contains(log.String(), "unclassified") {
				t.Fatal("missing scoped failure", log.String())
			}
			if mode == "inventory-architecture" && !strings.Contains(log.String(), "host=relay observation=inventory category=precondition-failed:relay-manifest-identity") {
				t.Fatal(log.String())
			}
		})
	}
}

func TestSequenceReportDoesNotInventUnconfirmedStops(t *testing.T) {
	for _, stop := range []bool{true, false} {
		r := &rehearsal{report: new(Report)}
		if !r.stop(context.Background(), time.Minute) {
			t.Fatal("absent caller required a stop")
		}
		sequence := operatorgates.SequenceResult{Gates: []operator.GateResult{{Gate: operator.Rollout, Status: operator.Inconclusive, Reason: "precondition-failed"}}, Procedures: []operatorgates.ProcedureResult{{Status: operator.Inconclusive, Reason: "precondition-failed", Category: "relay-manifest-identity"}}}
		r.recordSequence(sequence, map[operator.Gate]bool{operator.Rollout: stop})
		r.report.Evaluate()
		if r.report.Gates[0].CallerStopped != stop || strings.Contains(fmt.Sprint(r.report.Failures), "rollout:caller-stop-unconfirmed") == stop {
			t.Fatal(r.report.Gates, r.report.Failures)
		}
	}
}
