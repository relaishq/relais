package operatorrehearsal

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

// Actual admission collection -> archived receipt -> the session's independent
// inventory and history reads -> live five-host lease admission and snapshot.
func TestRelayTwoAdmissionBindsSession(t *testing.T) {
	fixtures, pins := idleInventory(t)
	var relay struct {
		ObservedAt   int64                       `json:"observedAt"`
		Participants []releasecompat.Participant `json:"participants"`
	}
	require.NoError(t, json.Unmarshal(fixtures["relay"], &relay))
	relay.Participants = relay.Participants[:1]
	relay.Participants[0].Name = "relay-two"
	fixtures["relay-two"], _ = json.Marshal(relay)
	scenario := operator.Scenario{RelayTwo: true, SchemaVersion: 1, Gates: []operator.Gate{operator.Rollout}, ReleaseA: operator.Identity{SourceCommit: pins[0].Commit, SHA256: pins[0].ArchiveSHA256}, ReleaseB: operator.Identity{SourceCommit: pins[1].Commit, SHA256: pins[1].ArchiveSHA256}}
	var err error
	scenario.Digest, err = operator.PlanIdentity(scenario)
	require.NoError(t, err)
	p := admissionFixture(t, false, nil)
	dir := t.TempDir()
	var mu sync.Mutex
	helpers := map[string]bool{}
	a := ClockAdmission{Scenario: scenario, RunID: "five-host-admission", FrontendSlot: "shared", ControlSlot: "shared", CallerLeapMode: "unsmeared", Policy: p.policy, Output: dir, HelpersUntil: p.now().Add(time.Hour), Now: p.now, Pause: p.pause, Timeout: p.timeout, Addresses: map[string]string{}}
	for i, slot := range operator.HostSlots(true) {
		a.Addresses[slot] = fmt.Sprintf("192.0.2.%d", i+1)
	}
	a.Host = func(ctx context.Context, slot string, detach bool, input []byte, args ...string) (string, error) {
		if args[0] == "sh" {
			return p.read(ctx, slot, string(input))
		}
		if detach {
			mu.Lock()
			helpers[slot] = true
			mu.Unlock()
		}
		return "", nil
	}
	status := AdmitClock(context.Background(), a)
	require.Empty(t, status.Cause)
	require.Len(t, helpers, 5)
	bound, selected, err := BindClockAdmission(dir, status, scenario)
	require.NoError(t, err)
	require.Len(t, selected.Receipt.Hosts, 5)
	var evidence clocks.SelectionEvidence
	raw, err := os.ReadFile(filepath.Join(dir, "clock-admission-evidence.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &evidence))
	wire := bindingtest.Fixture{Input: evidence}
	for i := range wire.Input.Hosts {
		log := &wire.Input.Hosts[i].Tracking
		first, last := clocks.FrozenTrackingRange(log.Rows, evidence.StartUnixSeconds, evidence.EndUnixSeconds)
		var frozen []clocks.TrackingRow
		for _, row := range log.Rows {
			if row.UnixSeconds >= first && row.UnixSeconds <= last {
				frozen = append(frozen, row)
			}
		}
		log.Rows = frozen
		for j := range wire.Input.Hosts[i].Tracking.Rows {
			wire.Input.Hosts[i].Tracking.Rows[j].BeforeRun = true
		}
	}
	clock := &operatorgates.FakeClock{Now: p.now()}
	peer := &idlePeer{respond: func(req operator.Request) operator.Reply {
		out := ""
		if req.Action == "clock" {
			if req.Operation == operator.HostLeaseRead {
				clock.Advance(20 * time.Millisecond)
			}
			out = wire.Wire(string(req.Script), clock.Wall())
		} else {
			out = bindingtest.Pack(json.RawMessage(fixtures[req.Target]))
		}
		payload, _ := json.Marshal(operator.HostOutput{Stdout: out})
		zero := 0
		return operator.Reply{Transport: operator.Succeeded, ExitCode: &zero, Payload: payload}
	}}
	init := operator.Init{Scenario: bound.Scenario, ClockBinding: &bound.ClockBinding, ArmedAtUnixMS: fmt.Sprint(clock.Wall().UnixMilli()), EndUnixMS: fmt.Sprint(clock.Wall().Add(time.Hour).UnixMilli()), Capabilities: []operator.Operation{operator.HostExecute, operator.HostLeaseRead}}
	session, err := operatorgates.NewSession("idle", init, clock)
	require.NoError(t, err)
	config := operatorgates.ResourceConfig{Pins: pins, Bounds: selected.Receipt.Bounds, ClockPolicy: a.Policy, ClockLeapMode: a.CallerLeapMode, Endpoints: operatorcaller.Endpoints{RunID: a.RunID, FrontendControlBoundNS: selected.Receipt.Bounds.FrontendControlBoundNS, FrontendWorkerBoundNS: selected.Receipt.Bounds.FrontendWorkerBoundNS, DriftPPM: 100}}
	require.NoError(t, session.BindClock(context.Background(), peer, config, time.Minute))
	guard, err := session.BoundLeaseGuard(peer, func(_ context.Context, d time.Duration) error { clock.Advance(d); return nil })
	require.NoError(t, err)
	require.NoError(t, guard.Refresh(context.Background(), time.Minute))
	snapshot, err := guard.Snapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.Clocks, 5)
	// A four-host plan can never bind this five-host receipt.
	scenario.RelayTwo = false
	scenario.Digest, err = operator.PlanIdentity(scenario)
	require.NoError(t, err)
	bound, _, err = BindClockAdmission(dir, status, scenario)
	require.NoError(t, err)
	init.Scenario, init.ClockBinding = bound.Scenario, &bound.ClockBinding
	session, err = operatorgates.NewSession("idle", init, clock)
	require.NoError(t, err)
	require.EqualError(t, session.BindClock(context.Background(), peer, config, time.Minute), "clock-binding-roles-differ")
	require.False(t, session.Latched())
}

func TestRelayTwoAdmissionChargesEveryHelper(t *testing.T) {
	_, setup, _ := clockAdmissionTiming(4*time.Second, 8*time.Second, true)
	require.Equal(t, 42*time.Second, setup)
	require.NoError(t, CheckClockAdmissionTiming(12*time.Second, 12*time.Second, false))
	require.EqualError(t, CheckClockAdmissionTiming(12*time.Second, 12*time.Second, true), "clock-admit-budget")
}
