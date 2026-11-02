package operatorgates

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

func TestDiagnosticsRetainExcludedPhaseAndMetricCauses(t *testing.T) {
	summary := operatorevidence.Summary{
		Status: operator.Inconclusive, InconclusiveMetrics: 1, IncludedPhases: 1, DroppedPhases: 1, ExcludedEvents: 2, SaturationOverlaps: 1,
		UndecidedMetrics: []operatorevidence.UndecidedMetric{{Phase: 1, Event: 2, Metric: "first_content", Causes: []string{"duration-bounds-uncertain"}}},
		PhaseDiagnostics: []operatorevidence.PhaseDiagnostic{{Gate: "rollout", Phase: 0, EventCount: 10, Cause: "uncertain-receipt", Issue: observe.EvidenceIssue{Path: "$.callerJsonl[12].call.DecryptionFailures", Reason: "missing-or-null"}}, {Gate: "control-restart", Phase: 1, EventCount: 11, Included: true, ExcludedEvents: 2, SaturationOverlaps: 1}},
	}
	s, _ := fixtureSession(t)
	seq := s.RunSequence(context.Background(), SequencePorts{
		Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
			return ProcedureResult{Completed: true, AwaitingPool: true, Status: operator.Inconclusive, Process: ProcessEvidence{CompletedSteps: 57}, Media: procedureMedia(summary)}
		},
		Finalize: func(context.Context, time.Duration) (*MediaSummary, error) { return procedureMedia(summary), nil },
	})
	raw, err := json.Marshal(s.Final(seq.Gates))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Diagnostics []struct{ Media, Pool operatorevidence.Summary }
		Gates       []operator.GateResult
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, got := range []operatorevidence.Summary{wire.Diagnostics[0].Media, wire.Diagnostics[0].Pool} {
		if got.IncludedPhases != 1 || got.DroppedPhases != 1 || got.ExcludedEvents != 2 || got.SaturationOverlaps != 1 || len(got.UndecidedMetrics) != 1 || len(got.PhaseDiagnostics) != 2 {
			t.Fatal("metric and phase causes/counts discarded")
		}
		metric, phase := got.UndecidedMetrics[0], got.PhaseDiagnostics[0]
		if metric.Phase != 1 || metric.Event != 2 || metric.Metric != "first_content" || strings.Join(metric.Causes, ",") != "duration-bounds-uncertain" || phase.Included || phase.Cause != "uncertain-receipt" || phase.Issue.Path != "$.callerJsonl[12].call.DecryptionFailures" || phase.Issue.Reason != "missing-or-null" {
			t.Fatal("diagnostic cause changed")
		}
	}
	if wire.Gates[0].Reason != "" {
		t.Fatal("legacy reason changed")
	}
	// Diagnostic labels must be allow-listed even for a manually supplied summary.
	summary.UndecidedMetrics[0].Metric = "private-value"
	summary.UndecidedMetrics[0].Causes[0] = "private-value"
	summary.PhaseDiagnostics[0].Gate = "private-value"
	summary.PhaseDiagnostics[0].Cause = "private-value"
	summary.PhaseDiagnostics[0].Issue = observe.EvidenceIssue{Path: "$.private-value", Reason: "private-value"}
	raw, _ = json.Marshal(mediaDiagnostic(procedureMedia(summary)))
	if strings.Contains(string(raw), "private-value") {
		t.Fatal("unsafe diagnostic escaped")
	}
}

func TestReleaseGateWithRemoteHostLatencyAndThirtySecondFloor(t *testing.T) {
	g, clock := testProcedure(t)
	s, _ := fixtureSession(t)
	s.clock = clock
	s.Budget.clock = clock
	clock.Advance(GateMaximum - RestoreReserve - 30500*time.Millisecond)
	submittedTooLate := false
	peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
		if req.RemainingMS < 30000 {
			submittedTooLate = true
			return operator.Reply{Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
		}
		clock.Advance(700 * time.Millisecond)
		return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int)}
	}}
	g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, _ ProcessExtra) (StepResult, error) {
		var refused bool
		certainty, err := s.Exchange(ctx, peer, operator.HostExecute, slot, op, nil, nil, end, func(r operator.Reply) operator.Certainty {
			refused = r.NotSubmitted
			if refused {
				return operator.Failed
			}
			return operator.Succeeded
		})
		if err != nil {
			return StepResult{Certainty: certainty}, err
		}
		if refused {
			return StepResult{Certainty: certainty}, fault("precondition-failed:deadline-before-submission")
		}
		return StepResult{Certainty: certainty, Status: controlplane.Status{Ready: true, Calls: []controlplane.CallStatus{{ID: "original", Owner: "w2", Epoch: 1}}, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}}, nil
	}
	got := RunReleaseGate(context.Background(), g.c, g.p, GateMaximum)
	if submittedTooLate || peer.sent != 2 {
		t.Fatal("MUTATION: host-submission-floor: public driver submitted a host command below its admission floor")
	}
	if got.Status != operator.Inconclusive || got.Reason != "precondition-failed" || got.Category != "deadline-before-submission" || s.Latched() {
		t.Fatal("refusal must remain definite and inconclusive")
	}
	raw, _ := json.Marshal(s.Final([]operator.GateResult{got.GateResult(operator.UpgradeRollback)}))
	var wire struct {
		Refusals []struct {
			RequestID, Operation, Target, Action, DeadlineUnixMS, Code string
			RemainingMS                                                int64
		}
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Refusals) == 0 {
		t.Fatal("typed refusal and request metadata discarded")
	}
	refusal := wire.Refusals[0]
	if refusal.RequestID != "2" || refusal.Operation != "host.execute" || refusal.Target != "shared" || refusal.Action != "freeze" || refusal.DeadlineUnixMS == "" || refusal.RemainingMS != 29800 || refusal.Code != "deadline-before-submission" {
		t.Fatal("refusal metadata changed")
	}
}

func TestHostSubmissionFloorUsesClampedBudget(t *testing.T) {
	for _, remaining := range []time.Duration{operator.HostSubmissionMinimum - time.Millisecond, operator.HostSubmissionMinimum, operator.HostSubmissionMinimum + time.Millisecond} {
		t.Run(remaining.String(), func(t *testing.T) {
			s, clock := fixtureSession(t)
			_, _ = s.BeginGate()
			clock.Advance(GateMaximum - remaining)
			peer := &observationPeer{respond: func(operator.Request) operator.Reply {
				return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int)}
			}}
			out, err := s.Exchange(context.Background(), peer, operator.HostExecute, "shared", "status", nil, nil, SessionMaximum, func(r operator.Reply) operator.Certainty {
				if r.NotSubmitted {
					return operator.Failed
				}
				return operator.Succeeded
			})
			refused := remaining < operator.HostSubmissionMinimum
			if err != nil || refused && (out != operator.Failed || peer.sent != 0) || !refused && (out != operator.Succeeded || peer.sent != 1) {
				t.Fatal("host floor must use clamped, inclusive minimum", out, err)
			}
		})
	}
	// Mailbox reads are not host commands and retain their shorter deadline.
	s, _ := fixtureSession(t)
	peer := &observationPeer{respond: func(operator.Request) operator.Reply {
		return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int)}
	}}
	_, err := s.Exchange(context.Background(), peer, operator.MailboxRead, "caller", "collect", nil, nil, time.Second, func(operator.Reply) operator.Certainty { return operator.Succeeded })
	if err != nil || peer.sent != 1 {
		t.Fatal("host floor changed non-host requests")
	}
}

func TestAdapterRefusalRetainsOriginalRequestMetadata(t *testing.T) {
	s, clock := fixtureSession(t)
	peer := &observationPeer{respond: func(operator.Request) operator.Reply {
		clock.Advance(time.Second)
		return operator.Reply{Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
	}}
	out, err := s.Exchange(context.Background(), peer, operator.HostExecute, "shared", "status", nil, nil, time.Minute, func(operator.Reply) operator.Certainty { return operator.Failed })
	result := s.Final(nil)
	if err != nil || out != operator.Failed || s.Latched() || len(result.Refusals) != 1 {
		t.Fatal("MUTATION: refusal-retention: proved adapter refusal was not retained")
	}
	refusal := result.Refusals[0]
	if refusal.RequestID != peer.request.ID || refusal.Operation != peer.request.Operation || refusal.Target != peer.request.Target || refusal.Action != peer.request.Action || refusal.DeadlineUnixMS != peer.request.DeadlineUnixMS || refusal.RemainingMS != peer.request.RemainingMS || refusal.Code != "deadline-before-submission" {
		t.Fatal("original request metadata discarded")
	}
	if result.FirstError == nil || result.FirstError.Code != refusal.Code {
		t.Fatal("first refusal lost its fixed code")
	}
}

func TestDiagnosticsFitFinalFrameWithOmissionCounts(t *testing.T) {
	summary := operatorevidence.Summary{Status: operator.Inconclusive, InconclusiveMetrics: 270, IncludedPhases: 6}
	for i := range 270 {
		summary.UndecidedMetrics = append(summary.UndecidedMetrics, operatorevidence.UndecidedMetric{Phase: i % 6, Event: i / 6, Metric: "first_new_content", Causes: []string{"classification-unproven-within-clock-interval", "duration-bounds-uncertain", "baseline-intervals-insufficient"}})
	}
	gates := []operator.Gate{operator.Rollout, operator.ControlRestart, operator.UpgradeRollback, operator.Failover, operator.RelayDeploy, operator.Load}
	s, _ := fixtureSession(t)
	s.gates = gates
	seq := SequenceResult{Pool: procedureMedia(summary)}
	for i, gate := range gates {
		summary.PhaseDiagnostics = append(summary.PhaseDiagnostics, operatorevidence.PhaseDiagnostic{Gate: string(gate), Phase: i, Included: true})
	}
	seq.Pool = procedureMedia(summary)
	for _, gate := range gates {
		seq.Gates = append(seq.Gates, operator.GateResult{Gate: gate, Status: operator.Inconclusive})
		seq.Procedures = append(seq.Procedures, ProcedureResult{Completed: true, Media: procedureMedia(summary)})
	}
	s.retainGateDiagnostics(&seq, nil)
	for range 1000 {
		r := issue(t, s, operator.HostExecute, "status")
		_, _ = s.Accept(s.ID, operator.Reply{ID: r.ID, Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}, operator.Failed)
	}
	for range 128 {
		s.RecordUncertainty(operator.Uncertainty{Kind: "host.execute", RequestID: "32768", Target: "shared", Action: "control-start", DeadlineUnixMS: "9999999999999", RemainingMS: 30000, Rule: "operation-evidence-unknown"})
	}
	final := s.Final(seq.Gates)
	// Production sessions stream the complete trace before the final frame.
	final.Trace = nil
	raw, err := json.Marshal(operator.Frame{Type: "final", Session: s.ID, Final: &final})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw)+1 > operator.MaxFrameBytes {
		t.Fatal("retained diagnostics exceed the final protocol frame")
	}
	var wire struct {
		Final struct {
			Refusals        []operator.Refusal
			OmittedRefusals int
			Uncertainties   []operator.Uncertainty
			Diagnostics     []struct {
				Media, Pool struct {
					InconclusiveMetrics                              int
					UndecidedMetrics                                 []operator.MetricDiagnostic
					PhaseDiagnostics                                 []operator.PhaseDiagnostic
					OmittedUndecidedMetrics, OmittedPhaseDiagnostics int
				}
			}
		}
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Final.Refusals) != 64 || wire.Final.OmittedRefusals != 936 || len(wire.Final.Uncertainties) != 128 {
		t.Fatal("frame lost refusal or uncertainty inventories")
	}
	for _, row := range wire.Final.Diagnostics {
		for _, m := range []struct {
			InconclusiveMetrics                              int
			UndecidedMetrics                                 []operator.MetricDiagnostic
			PhaseDiagnostics                                 []operator.PhaseDiagnostic
			OmittedUndecidedMetrics, OmittedPhaseDiagnostics int
		}{row.Media, row.Pool} {
			if m.InconclusiveMetrics != 270 || len(m.UndecidedMetrics) == 0 || len(m.UndecidedMetrics)+m.OmittedUndecidedMetrics != 270 || len(m.PhaseDiagnostics) != 6 || m.OmittedPhaseDiagnostics != 0 {
				t.Fatal("bounded diagnostics silently lost counts or phase causes")
			}
		}
	}
}

func TestMetricDiagnosticStopsAtFirstRowThatDoesNotFit(t *testing.T) {
	causes := make([]string, 1000)
	for i := range causes {
		causes[i] = "duration-bounds-uncertain"
	}
	m := &MediaSummary{UndecidedMetrics: []operatorevidence.UndecidedMetric{
		{Metric: "first_content", Causes: causes},
		{Metric: "resume", Causes: []string{"metric-undecided"}},
	}}
	got := mediaDiagnostic(m)
	if len(got.UndecidedMetrics) != 0 || got.OmittedUndecidedMetrics != 2 {
		t.Fatal("metric projection scanned past the first row that did not fit")
	}
}

func TestRefusalDiagnosticsAreBoundedWithOmittedCount(t *testing.T) {
	s, _ := fixtureSession(t)
	for range 1000 {
		r := issue(t, s, operator.HostExecute, "status")
		out, err := s.Accept(s.ID, operator.Reply{ID: r.ID, Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}, operator.Failed)
		if err != nil || out != operator.Failed {
			t.Fatal("refusal changed certainty")
		}
	}
	raw, _ := json.Marshal(s.Final(nil))
	var got struct {
		Refusals        []operator.Refusal
		OmittedRefusals int
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Refusals) != 64 || got.OmittedRefusals != 936 || len(got.Refusals) != len(s.refusals) {
		t.Fatal("refusal inventory is unbounded or lost omission counts")
	}
	if got.Refusals[0].RequestID != "1" || got.Refusals[63].RequestID != "64" {
		t.Fatal("refusals no longer retain the first request metadata")
	}
}
