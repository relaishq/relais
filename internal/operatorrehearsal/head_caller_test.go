package operatorrehearsal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/clockprobe"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/pkg/callharness"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
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

// A head phase on an idle deployment names the checkout and caller digest it
// built in both report.json's headCaller section and head-caller-evidence.json.
func TestHeadCallerPhaseNamesItsBuildInReportAndEvidence(t *testing.T) {
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
	work, bin, output := t.TempDir(), t.TempDir(), t.TempDir()
	judgeBinary := filepath.Join(work, "judge")
	if err = os.WriteFile(judgeBinary, []byte("judge-stub"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(bin, "relais-load"), []byte("head-load"), 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("head-load"))
	loadSHA, commit := hex.EncodeToString(sum[:]), strings.Repeat("c", 40)

	fixtures, pins := idleInventory(t)
	clock := operatorgates.NewRealClock()
	now := clock.Wall()
	scenario := operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: CoveredGates, ReleaseA: operator.Identity{SourceCommit: pins[0].Commit, SHA256: pins[0].ArchiveSHA256}, ReleaseB: operator.Identity{SourceCommit: pins[1].Commit, SHA256: pins[1].ArchiveSHA256}}
	session, err := operatorgates.NewSession("idle", operator.Init{Scenario: scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute}}, clock)
	if err != nil {
		t.Fatal(err)
	}
	topology := fixtureTopology(t)
	runner := commandRunner(func(context.Context, Command) CommandResult { return CommandResult{Err: context.DeadlineExceeded} })
	r := &rehearsal{work: work, bridge: filepath.Join(bin, "relais-gates"), run: "run-head", clock: clock, session: session, runner: runner, diagnostics: io.Discard, topology: topology, pins: fixture.Config.Pins, c: RehearseConfig{Commit: commit, Output: output}, report: &Report{Bounds: clocks.Bounds{FrontendControlBoundNS: 2000000, FrontendWorkerBoundNS: 3000000, DriftPPM: 100}}, judge: &recordingJudge{pinned: operatorevidence.PinnedJudge{Binary: judgeBinary, SHA256: fixture.Scenario.Judge.SHA256}}}
	r.deployment = &Deployment{Topology: topology, containerIDs: fixtureContainerIDs()}
	r.process = &operatorgates.ProcessConfig{Parameters: r.parameters(), ArmedAt: now}
	idle := controlplane.Status{Ready: true}
	for _, name := range []string{"w1", "w2"} {
		idle.Workers = append(idle.Workers, controlplane.WorkerStatus{Name: name, Address: r.process.Parameters.Workers[name].Media})
	}
	peer := &idlePeer{respond: func(req operator.Request) operator.Reply {
		switch req.Action {
		case "clock":
			return hostReply(t, oracletest.Marshal(t, clocks.Sample{MeasuredUnixNS: strconv.FormatInt(now.UnixNano(), 10)}))
		case "inventory":
			return hostReply(t, fixtures[req.Target])
		}
		var product any = idle
		if req.Action == "worker-status" {
			product = mediaworker.WorkerStatus{InventoryKnown: true, Inventory: []mediaworker.InstalledSession{}}
		}
		return hostReply(t, oracletest.Marshal(t, map[string]any{"kind": "ok", "started": now, "ended": now, "data": product}))
	}}
	r.runtime = &operatorgates.ProcessRuntime{Session: session, Peer: peer, Config: r.process, Pins: pins, Architecture: "x86_64", Bounds: &clocks.Bounds{FrontendControlBoundNS: 1000000, FrontendWorkerBoundNS: 1000000}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	head := r.runHeadCaller(ctx, fixture.Scenario)
	// The fake rollout yields no media, so only the judge step may report an error.
	if head.Commit != commit || head.CallerSHA256 != loadSHA || !fullDigest.MatchString(head.ScenarioSHA256) || len(head.Gates) != 1 || head.Gates[0].Gate != operator.Rollout || strings.Join(head.Errors, ",") != "media-judge-failed" {
		t.Fatalf("head phase did not run with its build: %+v", head)
	}
	if _, err = os.Stat(filepath.Join(work, "head-session-spool")); err != nil {
		t.Fatal("head phase did not reach its session", err)
	}
	report := passingReport()
	report.HeadCaller = head
	var out bytes.Buffer
	if err = report.Write(&out); err != nil {
		t.Fatal(err)
	}
	var written struct {
		HeadCaller headCallerBuild `json:"headCaller"`
	}
	if err = json.Unmarshal(out.Bytes(), &written); err != nil || written.HeadCaller != (headCallerBuild{commit, loadSHA}) {
		t.Fatalf("report headCaller %+v %v", written.HeadCaller, err)
	}

	// The fake sequence yields no media phases, so judge the head copy directly.
	_, _ = r.headJudge(head).Judge(ctx, operatorevidence.Pool{SourceCommit: ReleaseACommit, RunID: "run-head"})
	raw, err = os.ReadFile(filepath.Join(output, HeadCallerEvidence))
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		SourceCommit string          `json:"sourceCommit"`
		HeadCaller   headCallerBuild `json:"headCaller"`
	}
	if err = json.Unmarshal(raw, &evidence); err != nil || evidence.SourceCommit != ReleaseACommit || evidence.HeadCaller != (headCallerBuild{commit, loadSHA}) {
		t.Fatalf("evidence %s %v", raw, err)
	}
}

func TestClockFalsificationSeedsLaterSequenceWindows(t *testing.T) {
	raw, err := os.ReadFile("../operatorgates/testdata/resource-session.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Scenario operator.Scenario
		Config   operatorgates.ResourceConfig
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	binary := filepath.Join(work, "judge")
	if err := os.WriteFile(binary, []byte("judge-stub"), 0600); err != nil {
		t.Fatal(err)
	}
	inventories, pins := idleInventory(t)
	topology := fixtureTopology(t)
	r := &rehearsal{work: work, run: "idle", diagnostics: io.Discard, topology: topology, pins: pins,
		runner: commandRunner(func(context.Context, Command) CommandResult { return CommandResult{Err: context.DeadlineExceeded} }),
		report: &Report{Bounds: clocks.FixedCoverageBounds()}}
	r.deployment = &Deployment{Topology: topology, containerIDs: fixtureContainerIDs()}
	judge := &recordingJudge{pinned: operatorevidence.PinnedJudge{Binary: binary, SHA256: fixture.Scenario.Judge.SHA256}}
	now := time.Now()
	init := operator.Init{Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10)}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	line, err := (clockprobe.Record{Target: clockprobe.WorkerOne, FrontendSend: 1793930000100000000, FrontendReceive: 1793930000102000000, HostReceive: 1793930000111000000, HostSend: 1793930000111000000}).MarshalLine()
	if err != nil {
		t.Fatal(err)
	}
	readWindow := func(s *operatorgates.Session, falsify bool) (observe.CoverageQualification, error) {
		peer := &idlePeer{respond: func(req operator.Request) operator.Reply {
			var reads operator.HostReadBatchArgs
			if err := json.Unmarshal(req.Args, &reads); err != nil {
				return operator.Reply{Transport: operator.Failed}
			}
			out := operator.HostReadBatchOutput{}
			for _, read := range reads.Reads {
				data := inventories[read.Target]
				if req.Action == "clock" {
					rows := []clocks.TrackingRow{}
					for n := int64(-3); n <= 3; n++ {
						rows = append(rows, clocks.TrackingRow{UnixSeconds: 1793930000 + n, MaxErrorSeconds: .0005, LeapStatus: "N"})
					}
					sample := struct {
						clocks.Sample
						ProbeLog *clocks.ProbeBatch `json:"probeLog,omitempty"`
					}{Sample: clocks.Sample{ErrorBoundNS: 1, MeasuredUnixNS: "1793930000000000000", TrackingLog: &clocks.TrackingLog{Rows: rows, ReadComplete: true}}}
					if read.Target == "shared" {
						batch := clocks.ProbeBatch{Cursor: json.RawMessage(`null`)}
						if falsify {
							batch.Lines = string(line)
							batch.Cursor = json.RawMessage(`{"file":"1:2","position":84,"anchor":"` + strings.Repeat("a", 64) + `","skipLine":false}`)
						}
						sample.ProbeLog = &batch
					}
					data = oracletest.Marshal(t, sample)
				}
				child := hostReply(t, data)
				child.ID = read.Target
				out.Replies = append(out.Replies, child)
			}
			return resourceReply(req.ID, operator.Succeeded, out)
		}}
		if _, err := s.FreshInventory(ctx, peer, pins, time.Minute, "", false); err != nil {
			return observe.CoverageQualification{}, err
		}
		var observed clocks.Evidence
		runtime := &operatorgates.ProcessRuntime{Session: s, Peer: peer, Bounds: &r.report.Bounds, Config: &operatorgates.ProcessConfig{Parameters: operatorgates.ProcessParameters{ClockProbeFile: "/probe-fixture"}}, ClockObserved: func(e clocks.Evidence) { observed = e }}
		if err := runtime.VerifyClocks(ctx, time.Minute); err != nil {
			return observe.CoverageQualification{}, err
		}
		e := clocks.CoverageEvidence{Bounds: r.report.Bounds, FrontendSlot: "shared", ControlSlot: "shared", Falsified: s.ClockCoverageFalsified(), Attempts: []clocks.CoverageAttempt{clocks.NewCoverageAttempt(observed)}, Windows: []observe.CoverageWindow{{Phase: 0, Event: 0, Kind: "move", StartUnixNS: "1793930000000000000", EndUnixNS: "1793930000200000000"}}}
		return e.Qualification(), nil
	}
	var alreadyJudged, firstAfterRead observe.CoverageQualification
	var earlierWire []byte
	var readErr error
	first := resourceSequence{spool: "first", judge: judge, started: func(s *operatorgates.Session) {
		alreadyJudged, readErr = readWindow(s, false)
		if readErr == nil {
			earlierWire, _ = json.Marshal(alreadyJudged)
			firstAfterRead, readErr = readWindow(s, true)
		}
	}, finished: func(*operatorgates.Session, operatorgates.ResourceResult) {}}
	if err := r.runResourceSequence(ctx, init, first); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(alreadyJudged.WindowCauses(0, 0, false)) != 0 || !slices.Contains(firstAfterRead.WindowCauses(0, 0, false), "clock-coverage-falsified") {
		t.Fatal("normal probe read did not change clock trust", alreadyJudged, firstAfterRead)
	}
	var secondCauses []string
	second := resourceSequence{spool: "second", judge: judge, started: func(s *operatorgates.Session) {
		var q observe.CoverageQualification
		q, readErr = readWindow(s, false)
		// This exact lookup supplies the pinned judge's per-window qualification.
		secondCauses = q.WindowCauses(0, 0, false)
	}, finished: func(*operatorgates.Session, operatorgates.ResourceResult) {}}
	if err := r.runResourceSequence(ctx, init, second); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !reflect.DeepEqual(secondCauses, []string{"clock-coverage-falsified"}) {
		t.Fatal("MUTATION: clock-coverage-run-seed", secondCauses)
	}
	afterWire, _ := json.Marshal(alreadyJudged)
	if !bytes.Equal(earlierWire, afterWire) || len(alreadyJudged.WindowCauses(0, 0, false)) != 0 {
		t.Fatal("earlier judgment changed")
	}
}
