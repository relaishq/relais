package operatorrehearsal

import (
	"bytes"
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
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/operator"
)

// The head-caller phase reuses the resource-session seam with its own sinks.
// Its gate rows, latch and caller binary never reach the pinned report fields.
func TestHeadCallerSequenceKeepsPinnedReportSeparate(t *testing.T) {
	raw, err := os.ReadFile("../operatorgates/testdata/resource-session.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Scenario operator.Scenario
		Config   operatorgates.ResourceConfig
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	binary := filepath.Join(work, "judge")
	if err = os.WriteFile(binary, []byte("judge-stub"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := commandRunner(func(_ context.Context, c Command) CommandResult {
		calls++
		return CommandResult{Err: context.DeadlineExceeded}
	})
	topology := fixtureTopology(t)
	r := &rehearsal{work: work, run: "run-resources", runner: runner, diagnostics: io.Discard, topology: topology, pins: fixture.Config.Pins, report: &Report{Bounds: clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000, DriftPPM: 100}}, judge: &recordingJudge{pinned: operatorevidence.PinnedJudge{Binary: binary, SHA256: fixture.Scenario.Judge.SHA256}}}
	r.deployment = &Deployment{Topology: topology, containerIDs: fixtureContainerIDs()}
	now := time.Now()
	init := operator.Init{Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err = r.runResourceSequence(ctx, init, r.pinnedSequence()); err != nil {
		t.Fatal(err)
	}
	pinnedSession, pinnedGates, pinnedErrors := r.session, append([]GateReport{}, r.report.Gates...), strings.Join(r.report.Errors, ",")
	if pinnedSession == nil || len(pinnedGates) != 1 {
		t.Fatal("pinned sequence did not record", pinnedGates)
	}
	head := &HeadCallerReport{}
	judge := &recordingJudge{pinned: r.judge.pinned}
	if err = r.runResourceSequence(ctx, init, headSequence(head, judge, callerBinary{"/head/relais-load", strings.Repeat("d", 64)})); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || r.session != pinnedSession || len(r.report.Gates) != 1 || !reflect.DeepEqual(r.report.Gates, pinnedGates) || strings.Join(r.report.Errors, ",") != pinnedErrors || len(r.report.CPU.CallerAffinities) != 0 {
		t.Fatalf("head phase changed pinned report: calls=%d gates=%+v errors=%v", calls, r.report.Gates, r.report.Errors)
	}
	if len(head.Gates) != 1 || head.Gates[0].Gate != operator.Rollout || head.Gates[0].Reason != "command-result-unknown" || !head.Latched || len(head.Uncertainties) == 0 || judge.note == nil {
		t.Fatalf("head phase not recorded in its own section: %+v", head)
	}
	if _, err = os.Stat(filepath.Join(work, "head-session-spool")); err != nil {
		t.Fatal("head phase must use its own spool", err)
	}
}

func TestHeadCallerSkipsWithoutEffects(t *testing.T) {
	latched := newTestSession(t)
	latched.Halt("command-result-unknown")
	for name, session := range map[string]*operatorgates.Session{
		"no pinned session":      nil,
		"pinned session latched": latched,
		"head caller missing":    newTestSession(t),
	} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			var log bytes.Buffer
			r := &rehearsal{work: work, bridge: filepath.Join(t.TempDir(), "relais-gates"), diagnostics: &log, c: RehearseConfig{Commit: strings.Repeat("a", 40)}, runner: commandRunner(func(context.Context, Command) CommandResult {
				t.Fatal("skipped head phase ran a command")
				return CommandResult{}
			})}
			r.session = session
			head := r.runHeadCaller(context.Background(), operator.Scenario{})
			want := "prior-gate-stopped"
			if name == "head caller missing" {
				want = "local-file-unavailable"
			}
			if len(head.Errors) != 1 || head.Errors[0] != want || len(head.Gates) != 0 || head.CallerSHA256 != "" {
				t.Fatalf("%+v", head)
			}
			if !strings.Contains(log.String(), "head-caller-phase failed: "+want) {
				t.Fatal(log.String())
			}
		})
	}
}

func newTestSession(t *testing.T) *operatorgates.Session {
	now := time.Now()
	init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10)}
	s, err := operatorgates.NewSession("run-head", init, operatorgates.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHeadCallerSectionDoesNotChangePinnedVerdict(t *testing.T) {
	failingHead := &HeadCallerReport{Errors: []string{"sequence-failed"}, Latched: true, Gates: []GateReport{{Gate: operator.Rollout, ProcedureResult: operatorgates.ProcedureResult{Status: operator.Failure}}}, CallerTiming: []CallerTiming{{Gate: operator.Rollout, CapacityInvalidated: true}}}
	passingHead := &HeadCallerReport{Gates: []GateReport{{Gate: operator.Rollout, ProcedureCompleted: true, CallerStopped: true, ProcedureResult: operatorgates.ProcedureResult{Status: operator.Passed}}}}
	for _, pinnedFails := range []bool{false, true} {
		base := passingReport()
		if pinnedFails {
			base.Gates[0].Status = operator.Failure
		}
		base.Evaluate()
		for _, head := range []*HeadCallerReport{failingHead, passingHead} {
			r := passingReport()
			if pinnedFails {
				r.Gates[0].Status = operator.Failure
			}
			r.HeadCaller = head
			r.CallerTiming = head.CallerTiming
			r.Evaluate()
			if r.Verdict != base.Verdict || strings.Join(r.Failures, ",") != strings.Join(base.Failures, ",") {
				t.Fatalf("head-caller section changed the pinned verdict: %s %v, want %s %v", r.Verdict, r.Failures, base.Verdict, base.Failures)
			}
		}
	}
}

func TestHeadCallerReportIsSanitized(t *testing.T) {
	secret := "private-secret"
	r := passingReport()
	r.HeadCaller = &HeadCallerReport{Commit: secret, CallerSHA256: secret, ScenarioSHA256: secret, CallerAffinities: []string{secret}, Errors: []string{secret}, Gates: []GateReport{{Gate: operator.Gate(secret), ProcedureResult: operatorgates.ProcedureResult{Status: operator.Verdict(secret), Reason: secret}}}, CallerTiming: []CallerTiming{{Gate: operator.Gate(secret), Events: []EventTiming{{Kind: secret}}}}}
	r.CallerTiming = []CallerTiming{{Gate: operator.Gate(secret), Events: []EventTiming{{Kind: secret}}}}
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), `"headCaller"`) {
		t.Fatal(out.String())
	}
}

func TestCallerTimingReportsBurstsGapsAndBindingProbes(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	probe := func(d time.Duration) map[string]any { return map[string]any{"caller_send": at(d)} }
	mapping := callharness.ClockMappingEvidence{LowerProbe: 4, UpperProbe: 5, Probes: []callharness.MappingProbe{
		{Index: 4, Age: callharness.OffsetInterval{Lower: 90 * time.Millisecond, Upper: 120 * time.Millisecond}, Drift: 12 * time.Microsecond},
		{Index: 5, Age: callharness.OffsetInterval{Lower: -160 * time.Millisecond, Upper: -130 * time.Millisecond}, Drift: 16 * time.Microsecond},
	}}
	call := map[string]any{
		"distributed": map[string]any{"probes": []any{probe(0), probe(10 * time.Second), probe(150 * time.Second), probe(150*time.Second + 250*time.Millisecond), probe(150*time.Second + 500*time.Millisecond)}},
		"Moves": []any{
			map[string]any{"Kind": "move", "receipt": map[string]any{"start": at(150*time.Second + 200*time.Millisecond)}, "distributed": map[string]any{"timing_evidence": callharness.TimingEvidence{Version: 1, Start: mapping, End: callharness.ClockMappingEvidence{LowerProbe: 9, UpperProbe: -1}}}},
			map[string]any{"Kind": "takeover", "receipt": map[string]any{"start": at(77 * time.Second)}, "distributed": map[string]any{"causes": []string{"clock probe capacity exceeded"}}},
		},
	}
	line, _ := json.Marshal(map[string]any{"type": "call", "call": call})
	stats, _ := json.Marshal(map[string]any{"type": "stats"})
	pool := operatorevidence.Pool{Phases: []operatorevidence.MediaInput{{Gate: operator.Rollout, RawMedia: operatorevidence.RawMedia{CallerJSONL: string(stats) + "\n" + string(line) + "\n"}}}}
	rows := callerTiming(pool)
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	row := rows[0]
	if row.Gate != operator.Rollout || row.Probes != 5 || !row.CapacityInvalidated || len(row.ProbeGaps) != 1 || row.ProbeGaps[0] != (ProbeGap{at(10 * time.Second).UnixMilli(), at(150 * time.Second).UnixMilli(), 140000}) || len(row.Events) != 2 {
		t.Fatalf("%+v", row)
	}
	move, takeover := row.Events[0], row.Events[1]
	if move.Kind != "move" || !move.TimingEvidence || move.NearestProbeMS == nil || *move.NearestProbeMS != 50 || move.ProbesWithin300MS != 3 || move.StartUnixMS != at(150*time.Second+200*time.Millisecond).UnixMilli() {
		t.Fatalf("move %+v", move)
	}
	if *move.Start != (BindingTiming{AgeNS: [2]int64{120e6, 160e6}, DriftNS: [2]int64{12e3, 16e3}}) || *move.End != (BindingTiming{AgeNS: [2]int64{-1, -1}, DriftNS: [2]int64{-1, -1}}) {
		t.Fatalf("binding %+v %+v", move.Start, move.End)
	}
	if takeover.TimingEvidence || takeover.Start != nil || *takeover.NearestProbeMS != -67000 || takeover.ProbesWithin300MS != 0 {
		t.Fatalf("takeover %+v", takeover)
	}
}
