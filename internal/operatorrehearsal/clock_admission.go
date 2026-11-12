package operatorrehearsal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// ClockHostCommand runs one fixed admission command on a deployed host and
// returns its stdout. A detached command must keep running after the call returns.
type ClockHostCommand func(ctx context.Context, slot string, detach bool, input []byte, args ...string) (string, error)

// ClockAdmission is the launcher's admission input. The rehearsal and
// `relais-gates clock-admit` both run it through collectClockSelection.
type ClockAdmission struct {
	RunID                     string
	FrontendSlot, ControlSlot string
	CallerLeapMode            string
	Policy                    clocks.SelectionPolicy
	// Addresses holds each slot's private IPv4 address for the probe helpers.
	Addresses map[string]string
	// HelpersUntil is the session deadline; helpers outlive admission until then.
	HelpersUntil time.Time
	Output       string
	Host         ClockHostCommand
	// HostReadLatency is the p99 latency of one host command, the L of
	// clockTiming.hostReadLatencyMs. Zero keeps the fixed setup budget.
	HostReadLatency time.Duration
	// Now and Pause are the admission clock; nil selects the real clock.
	Now   func() time.Time
	Pause func(context.Context, time.Duration) error
}

// ClockAdmissionBinding is what Init needs after an admitted selection.
type ClockAdmissionBinding struct {
	operator.RuntimeBinding
	ClockBinding operator.ClockBindingArtifacts `json:"clockBinding"`
}

// Collection always freezes this trailing window, so no other policy window can be admitted.
const clockAdmissionWindowSeconds = 120

// Helper setup gets at least the original fixed budget, and a margin for
// the frontend helper to start probing after its command returns.
const clockSetupMinimum = 10 * time.Second
const clockSetupMargin = 2 * time.Second

// clockSetupBudget covers the helper commands, which run one after another:
// each slot writes its helper, then starts it.
func clockSetupBudget(latency time.Duration) time.Duration {
	return max(clockSetupMinimum, 2*time.Duration(len(Slots))*latency+clockSetupMargin)
}

// CheckClockAdmissionLatency refuses a host latency whose helper setup leaves
// no room in the 150 s admission. Probes start only after setup, so the 120 s
// window, the second the trailing row lags, the 2 s retry pause and two
// trailing reads (an attempt that just misses the row and the one that reads
// it) all follow the setup budget. Zero is the fixed budget and always fits.
func CheckClockAdmissionLatency(latency time.Duration) error {
	if clockSetupBudget(latency)+(clockAdmissionWindowSeconds+3)*time.Second+2*latency > 150*time.Second {
		return errors.New("clock-admit-budget")
	}
	return nil
}

// CheckClockAdmissionPolicy needs no host: the selector's own policy rule plus
// the one window that collection supports.
func CheckClockAdmissionPolicy(policy clocks.SelectionPolicy) error {
	if _, err := clocks.CheckPolicy(policy); err != nil {
		return err
	}
	if policy.WindowSeconds != clockAdmissionWindowSeconds {
		return errors.New("clock-select-policy")
	}
	return nil
}

// AdmitClock collects admission evidence, selects bounds and archives
// clock-selection-status.json. A non-empty Cause is the fixed refusal.
func AdmitClock(ctx context.Context, a ClockAdmission) *ClockSelectionReport {
	result := collectClockSelection(ctx, a.RunID, a.FrontendSlot, a.ControlSlot, a.ports())
	if saveClockJSON(filepath.Join(a.Output, "clock-selection-status.json"), result) != nil {
		return &ClockSelectionReport{Cause: "clock-selection-artifact"}
	}
	return result
}

func (a ClockAdmission) ports() clockAdmissionPorts {
	if a.Now == nil {
		a.Now, a.Pause = time.Now, pause
	}
	return clockAdmissionPorts{
		policy: a.Policy, leapMode: a.CallerLeapMode, selectBounds: a.selectBounds, pause: a.Pause, now: a.Now, latency: a.HostReadLatency,
		read: func(ctx context.Context, slot, script string) (string, error) {
			return a.Host(ctx, slot, false, []byte(script), "sh", "-eu")
		},
		start: func(ctx context.Context, _ time.Time) error {
			if a.FrontendSlot == "" || a.ControlSlot == "" || a.HelpersUntil.IsZero() {
				return errors.New("clock-selection-helper")
			}
			return a.startHelpers(ctx)
		},
	}
}

func (a ClockAdmission) startHelpers(ctx context.Context) error {
	source, err := host.Assets.ReadFile("clock_probe.py")
	if err != nil {
		return err
	}
	// Start every endpoint before starting the frontend's three probe loops.
	order := []string{}
	for _, slot := range Slots {
		if slot != a.FrontendSlot {
			order = append(order, slot)
		}
	}
	order = append(order, a.FrontendSlot)
	for _, slot := range order {
		_, err = a.Host(ctx, slot, false, source, "python3", "-c", "import sys; open('/run/relais-clock-helper.py','x').write(sys.stdin.read())")
		if err != nil {
			return err
		}
		seconds := int(a.HelpersUntil.Sub(a.Now()) / time.Second)
		if seconds < 1 {
			return errors.New("clock-selection-helper")
		}
		args := []string{"python3", "/run/relais-clock-helper.py", "--listen", a.Addresses[slot] + ":24891", "--seconds", strconv.Itoa(seconds), "--until-unix-ns", strconv.FormatInt(a.HelpersUntil.UnixNano(), 10)}
		if slot == a.FrontendSlot {
			args = append(args, "--output", clockProbePath)
			for _, target := range []string{"control", "w1", "w2"} {
				peer := target
				if peer == "control" {
					peer = a.ControlSlot
				}
				args = append(args, "--peer", target+"=http://"+a.Addresses[peer]+":24891")
			}
		}
		if _, err = a.Host(ctx, slot, true, nil, args...); err != nil {
			return err
		}
	}
	return nil
}

func saveClockSelectionInput(dir string, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (evidence, config string, err error) {
	sort.Slice(e.Hosts, func(i, j int) bool { return e.Hosts[i].Slot < e.Hosts[j].Slot })
	evidence, config = filepath.Join(dir, "clock-admission-evidence.json"), filepath.Join(dir, "clock-selection-policy.json")
	return evidence, config, errors.Join(saveClockJSON(evidence, e), saveClockJSON(config, policy))
}

func (a ClockAdmission) selectBounds(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
	if _, _, err := saveClockSelectionInput(a.Output, e, policy); err != nil {
		return clocks.ClockSelection{}, "clock-selection-artifact"
	}
	selected, err := clocks.Select(e, policy)
	if err != nil {
		return clocks.ClockSelection{}, err.Error()
	}
	if saveClockJSON(filepath.Join(a.Output, "clock-selection.json"), selected) != nil {
		return selected, "clock-selection-artifact"
	}
	return selected, ""
}

// BindClockAdmission binds the archived selection in dir to the plan scenario.
// It refuses with clock-binding-missing unless status records an admitted run.
func BindClockAdmission(dir string, status *ClockSelectionReport, scenario operator.Scenario) (ClockAdmissionBinding, clocks.ClockSelection, error) {
	var selection clocks.ClockSelection
	fail := func(err error) (ClockAdmissionBinding, clocks.ClockSelection, error) {
		return ClockAdmissionBinding{}, clocks.ClockSelection{}, err
	}
	if status == nil || status.Cause != "" || status.Bounds == nil {
		return fail(errors.New("clock-binding-missing"))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "clock-selection.json"))
	if err != nil {
		return fail(err)
	}
	if observe.DecodeClosedLimit(raw, &selection, nil, operator.ClockArtifactLimit) != nil {
		return fail(errors.New("clock-binding-invalid"))
	}
	evidence, err := os.ReadFile(filepath.Join(dir, "clock-admission-evidence.json"))
	if err != nil {
		return fail(err)
	}
	receipt, err := json.Marshal(selection.Receipt)
	if err != nil {
		return fail(err)
	}
	artifacts, err := operator.PackClockBindingArtifacts(receipt, evidence)
	if err != nil {
		return fail(err)
	}
	runtime, err := operator.BindRuntime(scenario, selection.SHA256)
	if err != nil {
		return fail(err)
	}
	return ClockAdmissionBinding{RuntimeBinding: runtime, ClockBinding: artifacts}, selection, nil
}
