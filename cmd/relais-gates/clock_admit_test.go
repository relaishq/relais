package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorrehearsal"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

// The fake host command logs every call, answers lease reads from the fake
// chrony reader at the test's virtual time, and accepts helper commands.
const clockAdmitFakeHost = `#!/bin/sh
dir=%q
slot=$1 mode=$2
shift 2
echo "$slot $mode $*" >> "$dir/calls"
[ -e "$dir/fail" ] && exit 1
if [ "$mode" != run ] || [ "$1" != sh ]; then cat > /dev/null; exit 0; fi
now=$(cat "$dir/now")
out=$(python3 -B %q "$dir" "$slot" "$now" false) || exit 1
if [ -e "$dir/expire" ] && [ "$slot" = w2 ] && [ "$now" -ge 1792886525 ]; then
  printf %%s "$out" | python3 -c '
import base64, gzip, re, sys
# Edit the text in place: re-serializing could change row bytes or exceed the wire limit.
doc = gzip.decompress(base64.b64decode(sys.stdin.read())).decode()
measured = int(re.search(r"\"measuredUnixNs\":\s*\"(\d+)\"", doc.split("\"lease\"", 1)[1]).group(1))
head, lease = doc.split("\"lease\"", 1)
lease = re.sub(r"(\"referenceAgeNs\":\s*)\d+", r"\g<1>5200000000", lease, count=1)
lease = re.sub(r"(\"referenceUnixNs\":\s*\")\d+", r"\g<1>" + str(measured - 5200000000), lease, count=1)
print(base64.b64encode(gzip.compress((head + "\"lease\"" + lease).encode())).decode(), end="")'
else
  printf %%s "$out"
fi
`

type clockAdmitFixture struct {
	dir, output string
	args        []string
	plan        clockAdmitPlan
	policy      clocks.SelectionPolicy
	clock       clockAdmitClock
}

func newClockAdmitFixture(t *testing.T) *clockAdmitFixture {
	t.Helper()
	reader, err := filepath.Abs("../../internal/operatorgates/host/clock_admission_test.py")
	require.NoError(t, err)
	f := &clockAdmitFixture{dir: t.TempDir(), output: t.TempDir()}
	program := filepath.Join(f.dir, "host-command")
	require.NoError(t, os.WriteFile(program, fmt.Appendf(nil, clockAdmitFakeHost, f.dir, reader), 0700))
	now := time.Unix(1792886400, 0)
	var mu sync.Mutex
	publish := func() {
		next := filepath.Join(f.dir, "now.next")
		require.NoError(t, os.WriteFile(next, fmt.Append(nil, now.Unix()), 0600))
		require.NoError(t, os.Rename(next, filepath.Join(f.dir, "now")))
	}
	publish()
	f.clock = clockAdmitClock{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, pause: func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
		publish()
		return nil
	}}
	scenario := operator.Scenario{SchemaVersion: 1, Gates: []operator.Gate{operator.Rollout}, Template: operator.Identity{SourceCommit: "source", SHA256: "template"}}
	scenario.Digest, err = operator.PlanIdentity(scenario)
	require.NoError(t, err)
	f.plan = clockAdmitPlan{RunID: "admit-test", FrontendSlot: "shared", ControlSlot: "shared", CallerLeapMode: "unsmeared", HelpersUntilUnixMS: fmt.Sprint(now.Add(time.Hour).UnixMilli()), Addresses: map[string]string{"relay": "192.0.2.1", "shared": "192.0.2.2", "w1": "192.0.2.3", "w2": "192.0.2.4"}, Scenario: scenario}
	// Not the rehearsal policy: the receipt must show the file's values.
	f.policy = clocks.SelectionPolicy{WindowSeconds: 120, FloorNS: 50000, ScaleNumerator: 2, ScaleDenominator: 1, AllowanceNS: 10000, SignalReserveNS: 50000, ControlCeilingNS: 250000, WorkerCeilingNS: 350000, MaxDispersionGrowthPPM: "4"}
	f.args = []string{"-plan", filepath.Join(f.dir, "plan.json"), "-policy", filepath.Join(f.dir, "policy.json"), "-host-command", program, "-output", f.output}
	return f
}

// oneAttempt makes every pause after the first admitting read step past the
// admission deadline. A refusal then costs one attempt, not a retry every two
// seconds and a host read per lease refresh poll; those loops have their own tests.
func (f *clockAdmitFixture) oneAttempt() {
	pause := f.clock.pause
	f.clock.pause = func(ctx context.Context, d time.Duration) error {
		if f.clock.now().Unix() >= 1792886525 {
			d = max(d, 30*time.Second)
		}
		return pause(ctx, d)
	}
}

func (f *clockAdmitFixture) run(t *testing.T) (int, string) {
	t.Helper()
	for name, value := range map[string]any{"plan.json": f.plan, "policy.json": f.policy} {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(f.dir, name), raw, 0600))
	}
	var diagnostics bytes.Buffer
	code := clockAdmit(f.args, &diagnostics, f.clock)
	return code, strings.TrimSpace(diagnostics.String())
}

func (f *clockAdmitFixture) read(t *testing.T, name string, value any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.output, name))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, value))
	return raw
}

func TestClockAdmitWritesReceiptAndBinding(t *testing.T) {
	f := newClockAdmitFixture(t)
	code, cause := f.run(t)
	require.Equal(t, 0, code, cause)
	require.Empty(t, cause)

	var selection clocks.ClockSelection
	var evidence clocks.SelectionEvidence
	var status operatorrehearsal.ClockSelectionReport
	var binding operatorrehearsal.ClockAdmissionBinding
	f.read(t, "clock-selection.json", &selection)
	evidenceRaw := f.read(t, "clock-admission-evidence.json", &evidence)
	statusRaw := f.read(t, "clock-selection-status.json", &status)
	f.read(t, "clock-binding.json", &binding)

	// The policy file is the only source of the policy in the receipt.
	want, err := clocks.Select(evidence, f.policy)
	require.NoError(t, err)
	if selection.SHA256 != want.SHA256 || selection.Receipt.Policy != f.policy || selection.Receipt.RunID != "admit-test" {
		t.Fatal("MUTATION: clock-admit-policy-input", selection.Receipt.Policy)
	}
	require.Equal(t, int64(120000), selection.Receipt.Bounds.FrontendControlBoundNS)
	require.Equal(t, int64(170000), selection.Receipt.Bounds.FrontendWorkerBoundNS)
	require.Equal(t, fmt.Sprintf(`{"cause":"","selectedBounds":{"frontendControlBoundNs":120000,"frontendWorkerBoundNs":170000,"driftPpm":100},"updates":{"relay":%d,"shared":%d,"w1":%d,"w2":%d},"receiptSha256":%q,"evidenceSha256":%q}`, status.Updates["relay"], status.Updates["shared"], status.Updates["w1"], status.Updates["w2"], selection.SHA256, fmt.Sprintf("%x", sha256.Sum256(evidenceRaw))), string(statusRaw))

	if binding.PlanIdentity != f.plan.Scenario.Digest || binding.Scenario.ClockReceiptSHA256 != selection.SHA256 || binding.Scenario.Digest == binding.PlanIdentity {
		t.Fatal("MUTATION: clock-admit-binding", binding.RuntimeBinding)
	}
	runtime, err := operator.BindRuntime(f.plan.Scenario, selection.SHA256)
	require.NoError(t, err)
	require.Equal(t, runtime, binding.RuntimeBinding)
	receipt, packed, err := binding.ClockBinding.Unpack()
	require.NoError(t, err)
	require.Equal(t, selection.SHA256, fmt.Sprintf("%x", sha256.Sum256(receipt)))
	require.Equal(t, evidenceRaw, packed)

	// Helpers start on every endpoint before the frontend's probe loops.
	calls, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	require.NoError(t, err)
	var detached []string
	for line := range strings.Lines(string(calls)) {
		if fields := strings.Fields(line); fields[1] == "detach" {
			detached = append(detached, line)
		}
	}
	require.Len(t, detached, 4)
	require.Contains(t, detached[3], "shared detach python3 /run/relais-clock-helper.py --listen 192.0.2.2:24891 --seconds 3598 ")
	require.Contains(t, detached[3], "--peer control=http://192.0.2.2:24891 --peer w1=http://192.0.2.3:24891 --peer w2=http://192.0.2.4:24891")
	require.NotContains(t, string(evidenceRaw), "192.0.2.")
}

func TestClockAdmitRefusesWithNamedCause(t *testing.T) {
	for cause, change := range map[string]func(*testing.T, *clockAdmitFixture){
		"clock-select-ceiling":   func(_ *testing.T, f *clockAdmitFixture) { f.policy.ControlCeilingNS = 1 },
		"clock-select-growth":    func(_ *testing.T, f *clockAdmitFixture) { f.policy.MaxDispersionGrowthPPM = "0" },
		"clock-selection-helper": func(_ *testing.T, f *clockAdmitFixture) { f.plan.HelpersUntilUnixMS = "1792886400000" },
		"clock-selection-read": func(t *testing.T, f *clockAdmitFixture) {
			require.NoError(t, os.WriteFile(filepath.Join(f.dir, "fail"), nil, 0600))
		},
		"clock-lease-expired": func(t *testing.T, f *clockAdmitFixture) {
			require.NoError(t, os.WriteFile(filepath.Join(f.dir, "expire"), nil, 0600))
		},
	} {
		t.Run(cause, func(t *testing.T) {
			if testing.Short() && cause == "clock-selection-read" {
				t.Skip("spends the real ten-second seed budget on failing reads")
			}
			f := newClockAdmitFixture(t)
			change(t, f)
			f.oneAttempt()
			code, got := f.run(t)
			if code != 1 || got != cause {
				t.Fatal("MUTATION: clock-admit-refusal", code, got)
			}
			var status operatorrehearsal.ClockSelectionReport
			f.read(t, "clock-selection-status.json", &status)
			require.Equal(t, cause, status.Cause)
			require.Nil(t, status.Bounds)
			require.NoFileExists(t, filepath.Join(f.output, "clock-binding.json"))
			if cause == "clock-lease-expired" {
				// #439: the refusing slot, its aged lease and the 5-poll budget stay reported.
				require.NotNil(t, status.LeaseRefusal)
				require.Equal(t, "w2", status.LeaseRefusal.Slot)
				require.Equal(t, int64(5000000000), status.LeaseRefusal.BudgetNS)
				require.Greater(t, status.LeaseRefusal.AgedLeaseAgeNS, status.LeaseRefusal.BudgetNS)
			} else {
				require.Nil(t, status.LeaseRefusal)
			}
		})
	}
}

func TestClockAdmitRefusesBeforeHostCommands(t *testing.T) {
	for name, c := range map[string]struct {
		cause  string
		change func(*clockAdmitFixture)
	}{
		"digest-not-plan": {"clock-binding-digest-differ", func(f *clockAdmitFixture) { f.plan.Scenario.Digest = strings.Repeat("a", 64) }},
		"already-bound": {"clock-binding-digest-differ", func(f *clockAdmitFixture) {
			bound, _ := operator.BindRuntime(f.plan.Scenario, strings.Repeat("a", 64))
			f.plan.Scenario = bound.Scenario
		}},
		"receipt-claimed":  {"clock-binding-digest-differ", func(f *clockAdmitFixture) { f.plan.Scenario.ClockReceiptSHA256 = strings.Repeat("a", 64) }},
		"plan-changed":     {"clock-binding-digest-differ", func(f *clockAdmitFixture) { f.plan.Scenario.Judge.SHA256 = "other" }},
		"run":              {"clock-admit-input", func(f *clockAdmitFixture) { f.plan.RunID = "bad run" }},
		"role":             {"clock-admit-input", func(f *clockAdmitFixture) { f.plan.ControlSlot = "control" }},
		"leap":             {"clock-admit-input", func(f *clockAdmitFixture) { f.plan.CallerLeapMode = "" }},
		"deadline":         {"clock-admit-input", func(f *clockAdmitFixture) { f.plan.HelpersUntilUnixMS = "soon" }},
		"address":          {"clock-admit-input", func(f *clockAdmitFixture) { f.plan.Addresses["w2"] = "w2.internal" }},
		"extra-address":    {"clock-admit-input", func(f *clockAdmitFixture) { f.plan.Addresses["w3"] = "192.0.2.5" }},
		"output-missing":   {"clock-admit-input", func(f *clockAdmitFixture) { f.args[7] = filepath.Join(f.dir, "absent") }},
		"policy":           {"clock-select-policy", func(f *clockAdmitFixture) { f.policy.WindowSeconds = 60 }},
		"policy-window":    {"clock-select-policy", func(f *clockAdmitFixture) { f.policy.WindowSeconds = 121 }},
		"policy-growth":    {"clock-select-policy", func(f *clockAdmitFixture) { f.policy.MaxDispersionGrowthPPM = "fast" }},
		"relative-command": {"usage: relais-gates clock-admit -plan FILE -policy FILE -host-command PROGRAM -output DIR", func(f *clockAdmitFixture) { f.args[5] = "host-command" }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newClockAdmitFixture(t)
			c.change(f)
			code, got := f.run(t)
			if code != 2 || got != c.cause {
				t.Fatal("MUTATION: clock-admit-pre-spend", code, got)
			}
			require.NoFileExists(t, filepath.Join(f.dir, "calls"))
			entries, err := os.ReadDir(f.output)
			require.NoError(t, err)
			require.Empty(t, entries)
			require.NoDirExists(t, filepath.Join(f.dir, "absent"))
		})
	}
	f := newClockAdmitFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "plan.json"), []byte(`{"runId":"a","unknown":1}`), 0600))
	var diagnostics bytes.Buffer
	args := append([]string{}, f.args...)
	args[1] = filepath.Join(f.dir, "plan.json")
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, "policy.json"), []byte(`{}`), 0600))
	require.Equal(t, 2, clockAdmit(args, &diagnostics, f.clock))
	require.Equal(t, "clock-admit-input\n", diagnostics.String())
}

// The receipt is bound after admission from the archived files. A receipt that
// is gone or unreadable by then refuses with the binding cause and no binding file.
func TestClockAdmitRefusesUnboundReceipt(t *testing.T) {
	for cause, damage := range map[string]func(string) error{
		"clock-binding-missing": os.Remove,
		"clock-binding-invalid": func(path string) error {
			return os.WriteFile(path, []byte(`{"receipt":{},"sha256":"","extra":1}`), 0600)
		},
	} {
		t.Run(cause, func(t *testing.T) {
			f := newClockAdmitFixture(t)
			receipt := filepath.Join(f.output, "clock-selection.json")
			pause, damaged := f.clock.pause, false
			// Admission's closing wait is the first pause after the receipt is archived.
			f.clock.pause = func(ctx context.Context, d time.Duration) error {
				if _, err := os.Stat(receipt); err == nil && !damaged {
					damaged = true
					require.NoError(t, damage(receipt))
				}
				return pause(ctx, d)
			}
			code, got := f.run(t)
			if !damaged || code != 1 || got != cause {
				t.Fatal("MUTATION: clock-admit-binding-refusal", damaged, code, got)
			}
			var status operatorrehearsal.ClockSelectionReport
			f.read(t, "clock-selection-status.json", &status)
			require.Empty(t, status.Cause)
			require.NoFileExists(t, filepath.Join(f.output, "clock-binding.json"))
		})
	}
}

// A host command whose child keeps stdout open must not hold a read past the
// caller's deadline, nor past its own exit.
func TestClockAdmitHostReadEndsWithDeadline(t *testing.T) {
	program := filepath.Join(t.TempDir(), "host-command")
	for name, script := range map[string]string{
		// The wrapper stays alive: the deadline kills it, the child keeps the pipe.
		"deadline": "#!/bin/sh\nsleep 20 &\necho x\nwait\n",
		// The wrapper exits at once and leaves the pipe to its child.
		"orphan": "#!/bin/sh\nsleep 20 &\necho x\n",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(program, []byte(script), 0700))
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			began, done := time.Now(), make(chan error, 1)
			go func() {
				_, err := clockAdmitHost(program)(ctx, "w1", false, []byte("script"), "sh", "-eu")
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || err.Error() != "clock-selection-read" || time.Since(began) > 300*time.Millisecond+clockAdmitHostWaitDelay+2*time.Second {
					t.Fatal("MUTATION: clock-admit-host-deadline", err, time.Since(began))
				}
			case <-time.After(10 * time.Second):
				t.Fatal("MUTATION: clock-admit-host-deadline", "read still blocked after 10 s")
			}
		})
	}
}
