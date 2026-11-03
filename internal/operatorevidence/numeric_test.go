package operatorevidence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestNumericTimingProjectionIsBoundedAndHasNoDeploymentLabels(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d := &callharness.DistributedMeasurement{Mapped: true,
		Start:     callharness.TimeInterval{Earliest: base.Add(time.Second), Latest: base.Add(time.Second + time.Millisecond)},
		End:       callharness.TimeInterval{Earliest: base.Add(2 * time.Second), Latest: base.Add(2*time.Second + time.Millisecond)},
		Durations: map[string]callharness.DurationInterval{"first_content": {Lower: time.Second, Upper: 2 * time.Second}, "private-label": {}},
		Timing:    &callharness.TimingEvidence{Version: 1, Candidates: []callharness.ClassificationCandidate{{Metric: "private-label", Label: "private-label"}}},
	}
	call := &callharness.Report{StartedAt: base}
	for range maxNumericEvents + 7 {
		call.Moves = append(call.Moves, callharness.MoveReport{Kind: "takeover", Identity: &callharness.OperationIdentity{SessionID: "private-label"}, Distributed: d})
	}
	raw, _ := json.Marshal(loadgen.Message{Type: "call", Session: "private-label", Call: call})
	got := TimingDiagnostics(Pool{RunID: "private-label", Phases: []MediaInput{{RawMedia: RawMedia{CallerJSONL: string(raw)}}}})
	if len(got.Events) != maxNumericEvents || got.OmittedEvents != 7 || got.UnavailableStreams != 0 {
		t.Fatal("numeric row accounting", len(got.Events), got.OmittedEvents, got.UnavailableStreams)
	}
	first := got.Events[0]
	if first.Move != 7 || got.Events[len(got.Events)-1].Move != maxNumericEvents+6 || first.Event != -1 || first.Start.Lower != time.Second || first.Start.Width != time.Millisecond || first.End.Upper != 2*time.Second+time.Millisecond || first.Durations["first_content"].Upper != 2*time.Second {
		t.Fatal("numeric endpoints or durations lost", first)
	}
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), "private-label") {
		t.Fatal("deployment label escaped", err)
	}
	if d.Timing.Candidates[0].Label != "private-label" {
		t.Fatal("projection mutated classifier input")
	}
}

func TestNumericTimingProjectionCountsByteAndProbeOmissions(t *testing.T) {
	e := &callharness.TimingEvidence{Version: 1}
	e.Start.Probes = make([]callharness.MappingProbe, 20)
	e.Candidates = make([]callharness.ClassificationCandidate, 30)
	projected := publicTiming(e)
	if len(projected.Start.Probes) != 8 || projected.Start.OmittedProbes != 12 || len(projected.Candidates) != 16 || projected.OmittedCandidates != 14 {
		t.Fatal("unbounded nested diagnostics")
	}
	call := &callharness.Report{}
	for range 300 {
		call.Moves = append(call.Moves, callharness.MoveReport{Distributed: &callharness.DistributedMeasurement{Timing: e}})
	}
	raw, _ := json.Marshal(loadgen.Message{Type: "call", Call: call})
	out := TimingDiagnostics(Pool{Phases: []MediaInput{{RawMedia: RawMedia{CallerJSONL: string(raw)}}}})
	encoded, _ := json.Marshal(out)
	if len(encoded) > maxNumericBytes+10000 || len(out.Events)+out.OmittedEvents != 300 || len(out.Events) == 0 || len(out.Events) >= maxNumericEvents || out.Events[len(out.Events)-1].Move != 299 {
		t.Fatal("byte bound or omitted inventory lost", len(encoded), len(out.Events), out.OmittedEvents)
	}
}

func TestNumericTimingUsesScheduledEventIndexesForReorderedReceipts(t *testing.T) {
	first := callharness.OperationIdentity{RunID: "run", SessionID: "session", OperationID: "first", Worker: "worker", Epoch: 1}
	second := first
	second.OperationID = "second"
	call := &callharness.Report{Moves: []callharness.MoveReport{
		{Kind: "takeover", Identity: &second, Distributed: &callharness.DistributedMeasurement{}},
		{Kind: "takeover", Identity: &first, Distributed: &callharness.DistributedMeasurement{}},
	}}
	raw, _ := json.Marshal(loadgen.Message{Type: "call", Call: call})
	input := MediaInput{RawMedia: RawMedia{CallerJSONL: string(raw), Events: []loadgen.EventResult{
		{Event: loadgen.Event{Kind: "kill"}, Identities: []callharness.OperationIdentity{first}},
		{Event: loadgen.Event{Kind: "kill"}, Identities: []callharness.OperationIdentity{second}},
	}}}
	got := TimingDiagnostics(Pool{Phases: []MediaInput{input}})
	if len(got.Events) != 2 || got.Events[0].Move != 0 || got.Events[0].Event != 1 || got.Events[1].Event != 0 {
		t.Fatal("receipt order changed the event diagnostic join", got)
	}
	input.Events = append(input.Events, input.Events[0])
	got = TimingDiagnostics(Pool{Phases: []MediaInput{input}})
	if got.Events[1].Event != -1 {
		t.Fatal("ambiguous event was assigned a confident index")
	}
}
