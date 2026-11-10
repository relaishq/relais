package operatorevidence

import (
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/pkg/callharness"
)

func TestCoverageWindowsPreservesEveryReceipt(t *testing.T) {
	at := time.Unix(1793930000, 123).UTC()
	a := callharness.OperationIdentity{RunID: "run", OperationID: "move", SessionID: "a", Worker: "w1", Epoch: 1}
	b := a
	b.SessionID = "b"
	pool := Pool{Phases: []MediaInput{{RawMedia: RawMedia{Events: []loadgen.EventResult{{Event: loadgen.Event{Kind: "move"}, Identities: []callharness.OperationIdentity{a, b}}}, Receipts: []operatorcaller.Receipt{{Identity: a, Kind: "move", Start: at, End: at.Add(time.Second)}, {Identity: b, Kind: "move", Start: at.Add(2 * time.Second), End: at.Add(3 * time.Second)}}}}}}
	windows := CoverageWindows(pool)
	if len(windows) != 2 || windows[0].StartUnixNS != "1793930000000000123" || windows[1].EndUnixNS != "1793930003000000123" {
		t.Fatal("receipt timestamps lost", windows)
	}
	pool.Phases[0].Receipts = pool.Phases[0].Receipts[:1]
	windows = CoverageWindows(pool)
	if len(windows) != 2 || windows[1].StartUnixNS != "" {
		t.Fatal("missing receipt silently omitted", windows)
	}
}
func TestUnboundJudgeInputBytes(t *testing.T) {
	raw, err := MarshalJudgeInput(Pool{RunID: "fixture", PreCoverageEvidence: true})
	if err != nil || string(raw) != `{"runId":"fixture","phases":null,"missing":false,"preCoverageEvidence":true}` {
		t.Fatal("MUTATION: unbound-judge-input", string(raw), err)
	}
}

func TestPublicDiagnosticsKeepsClockCoverageCauses(t *testing.T) {
	for _, cause := range []string{"clock-coverage-missing", "clock-coverage-bound", "clock-coverage-break", "clock-coverage-kill-bracket", "clock-coverage-kill-excluded", "clock-coverage-falsified"} {
		p := Summary{UndecidedMetrics: []UndecidedMetric{{Metric: "audio_loss", Causes: []string{cause}}}}
		if got := p.PublicDiagnostics(); len(got.UndecidedMetrics) != 1 || len(got.UndecidedMetrics[0].Causes) != 1 || got.UndecidedMetrics[0].Causes[0] != cause {
			t.Fatal("coverage cause removed", got)
		}
	}
	if PublicTimingCause("clock-coverage-private-text") != "unclassified" {
		t.Fatal("arbitrary cause escaped")
	}
}
