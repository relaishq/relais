package operatorgates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

func TestAdmissionCountsStopAndIdleGap(t *testing.T) {
	for _, mode := range []string{"fits", "gap-exhausts-window", "known-failure"} {
		t.Run(mode, func(t *testing.T) {
			_, clock := fixtureSession(t)
			boundary := false
			status := operator.Inconclusive
			if mode == "known-failure" {
				status = operator.Failure
			}
			result := RunAdmission(context.Background(), AdmissionPorts{
				Clock: clock, BoundaryMinimum: 4*time.Minute + time.Second,
				Survivor: func(context.Context, time.Duration) MeasureResult {
					clock.Advance(4 * time.Minute)
					return MeasureResult{Status: status}
				},
				StopSurvivor: func(context.Context, time.Duration) bool { clock.Advance(time.Minute); return true },
				Idle: func(context.Context, time.Duration) error {
					if mode == "fits" {
						clock.Advance(30 * time.Second)
					} else {
						clock.Advance(2 * time.Minute)
					}
					return nil
				},
				Boundary: func(context.Context, time.Duration) MeasureResult {
					boundary = true
					return MeasureResult{Status: operator.Inconclusive}
				},
			}, 10*time.Minute)
			if mode == "fits" {
				if !boundary || result.Reason != "" {
					t.Fatal("admitted boundary was refused", result.Reason)
				}
			} else if boundary || result.Reason != "gate-deadline" {
				t.Fatal("MUTATION: admission-gap-budget", result.Reason)
			}
			if mode == "known-failure" && result.Status != operator.Failure {
				t.Fatal("boundary deadline hid a known failure")
			}
		})
	}
}

type budgetJudge struct{}

func (budgetJudge) Judge(context.Context, operatorevidence.Pool) (operatorevidence.Summary, error) {
	return operatorevidence.Summary{Status: operator.Inconclusive}, nil
}

type budgetCallerStore struct {
	read   func(string) operatorcaller.ReadResult
	create func() operatorcaller.CreateState
}

func (b budgetCallerStore) Read(_ context.Context, key string) (operatorcaller.ReadResult, error) {
	return b.read(key), nil
}
func (b budgetCallerStore) Create(context.Context, string, []byte) operatorcaller.CreateState {
	return b.create()
}

func TestResourceAdmissionCountsActualPhaseGap(t *testing.T) {
	runtime, peer, _ := runtimeFixture(t)
	s := runtime.Session
	s.capabilities[operator.CallerStop] = true
	clock := s.clock.(*FakeClock)
	id := operatorcaller.Identity{RunID: "run-load-budget", Gate: operator.Load, Phase: operatorcaller.Survivor, CallerID: "1", Attempt: "1", PublicSHA: runtime.Pins[0].Commit}
	m := operatorcaller.Manifest{Version: 1, Identity: id, ArmedAt: clock.Wall().UnixMilli(), CreatedAt: clock.Wall().UnixMilli(), Deadline: clock.Wall().Add(12 * time.Minute).UnixMilli(), Plan: operatorcaller.NewPlan(id.Phase, 240, id.PublicSHA), Endpoints: operatorcaller.Endpoints{RunID: id.RunID, Signaling: "https://signal.test/calls", Relay: runtime.Config.Parameters.Workers["w1"].Media.String(), DriftPPM: 1}}
	boundaryStarted := false
	store := budgetCallerStore{read: func(key string) operatorcaller.ReadResult {
		var data []byte
		if strings.HasSuffix(key, "/finished.json") {
			data = oracletest.Marshal(t, map[string]any{"runId": id.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": []string{"original"}})
		} else {
			data = []byte("incomplete evidence")
		}
		return operatorcaller.ReadResult{Found: true, Data: data}
	}, create: func() operatorcaller.CreateState { boundaryStarted = true; return operatorcaller.CreateUnknown }}
	broker, err := operatorcaller.NewBroker(m, operatorcaller.NewMailbox(store), resourceCallerClock{clock, resourcePause}, s)
	if err != nil {
		t.Fatal(err)
	}
	broker.Registration = &operatorcaller.Registration{Near: true, Sessions: []string{"original"}}
	caller := NewCallerRuntime(broker, clock, 11*time.Minute, &EvidenceRuntime{RunID: id.RunID, Judge: budgetJudge{}}, operatorevidence.Phase{})
	boundary := *broker
	boundary.Manifest.Phase = operatorcaller.AdmissionBoundary
	boundary.Manifest.CallerID = "2"
	boundary.Manifest.Plan = operatorcaller.NewPlan(operatorcaller.AdmissionBoundary, 240, id.PublicSHA)
	runtime.commandPorts = &ProcedurePorts{Clock: clock, Broker: caller, Sleep: func(context.Context, time.Duration) error { <-caller.done; return nil }, Step: runtime.Step}
	stopped, idle := false, false
	peer.respond = func(q operator.Request) operator.Reply {
		if q.Operation == operator.CallerStop {
			stopped = true
			clock.Advance(time.Minute)
			return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Payload: oracletest.Marshal(t, CallerProof{Inactive: true})}
		}
		var data any
		switch q.Action {
		case "status":
			status := controlplane.Status{Ready: true}
			status.Pool.Cap = 2
			for _, w := range []string{"w1", "w2"} {
				status.Workers = append(status.Workers, controlplane.WorkerStatus{Name: w, Address: runtime.Config.Parameters.Workers[w].Media})
			}
			data = status
		case "worker-status":
			data = mediaworker.WorkerStatus{InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}, Address: runtime.Config.Parameters.Workers["w1"].Media}
		case "store-status":
			idle = true
			clock.Advance(2 * time.Minute)
			data = map[string]any{"owners": map[string]int{"w1": 0, "w2": 0}, "transfers": 0}
		default:
			return operator.Reply{Transport: operator.Failed, ExitCode: new(int), NotSubmitted: true, RefusalCode: "unsupported-operation"}
		}
		return hostReply(t, oracletest.Marshal(t, map[string]any{"kind": "ok", "started": clock.Wall(), "ended": clock.Wall(), "data": data}))
	}
	launchers := resourceCallers{&ResourcePorts{Session: s, Peer: peer, End: 12 * time.Minute, Manifest: m, start: &operatorcaller.StartResult{State: operatorcaller.Started, Certainty: operator.Succeeded}}}
	// Four minutes of survivor media fit with a four-minute boundary if the
	// intervening one-minute stop and two-minute idle wait are incorrectly omitted.
	clock.Advance(4 * time.Minute)
	out := runResourceAdmission(context.Background(), s, runtime, caller, &boundary, launchers, 10*time.Minute, strings.Repeat("x", 64), &ResourceResult{})
	if !stopped || !idle || boundaryStarted || out.Reason != "gate-deadline" {
		for _, u := range s.Uncertainties() {
			t.Log("uncertainty", u.Kind, u.Action, u.Rule)
		}
		t.Fatal("MUTATION: resource-admission-gap", out.Reason, stopped, idle, boundaryStarted)
	}
}
