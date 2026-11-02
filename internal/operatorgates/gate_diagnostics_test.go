package operatorgates

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func TestSessionRetainsGateDiagnostics(t *testing.T) {
	s, _ := fixtureSession(t)
	s.gates = []operator.Gate{operator.ControlRestart, operator.UpgradeRollback}
	seq := s.RunSequence(context.Background(), SequencePorts{
		Command: func(_ context.Context, g operator.Gate, _ time.Duration) ProcedureResult {
			if g == operator.ControlRestart {
				return ProcedureResult{Completed: true, AwaitingPool: true, Status: operator.Inconclusive, Media: &MediaSummary{Status: operator.Inconclusive, InconclusiveMetrics: 3}}
			}
			p, _ := testProcedure(t)
			return p.finish(context.Background(), fault("missing-live-call"), false)
		},
		Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
			return &MediaSummary{Status: operator.Inconclusive, InconclusiveMetrics: 4}, nil
		},
	})
	final := s.Final(seq.Gates)
	raw, _ := json.Marshal(final)
	var wire struct {
		Diagnostics []struct {
			Gate      operator.Gate `json:"gate"`
			Reason    string        `json:"reason"`
			Procedure struct {
				Completed bool
				Reason    string
			} `json:"procedure"`
			Media       *struct{ InconclusiveMetrics int } `json:"media"`
			PoolPending bool                               `json:"poolPending"`
			Pool        *struct{ InconclusiveMetrics int } `json:"pool"`
		}
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Diagnostics) != 2 {
		t.Fatal("session discarded per-gate diagnostics")
	}
	first, second := wire.Diagnostics[0], wire.Diagnostics[1]
	if !first.Procedure.Completed || !first.PoolPending || first.Media == nil || first.Media.InconclusiveMetrics != 3 || first.Pool == nil || first.Pool.InconclusiveMetrics != 4 {
		t.Fatal("undecided media and pending pool not retained")
	}
	if second.Procedure.Completed || second.Procedure.Reason != "missing-live-call" {
		t.Fatal("procedure failure lost")
	}
	for i, g := range final.Gates {
		if g.Status == operator.Inconclusive && wire.Diagnostics[i].Reason == "" {
			t.Fatal("reasonless inconclusive diagnostic")
		}
		if g.Reason != "" {
			t.Fatal("diagnostics changed a legacy gate reason")
		}
	}
	// Arbitrary callback text cannot become report diagnostics.
	s2, _ := fixtureSession(t)
	result := s2.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
		return ProcedureResult{Status: operator.Inconclusive, Reason: "sensitive arbitrary output", Category: "sensitive-category"}
	}})
	raw, _ = json.Marshal(s2.Final(result.Gates))
	if strings.Contains(string(raw), "sensitive") {
		t.Fatal("unsafe diagnostic escaped")
	}
}

func TestGateDiagnosticMediaOutcomes(t *testing.T) {
	for _, verdict := range []operator.Verdict{operator.Passed, operator.Failure, operator.Inconclusive} {
		t.Run(string(verdict), func(t *testing.T) {
			s, _ := fixtureSession(t)
			seq := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
				return ProcedureResult{Completed: true, Status: verdict, Media: &MediaSummary{Status: verdict}}
			}})
			final := s.Final(seq.Gates)
			if final.Status != verdict || len(final.Diagnostics) != 1 || final.Diagnostics[0].Media.Status != verdict || !final.Diagnostics[0].Procedure.Completed {
				t.Fatal("diagnostics changed verdict or procedure completion")
			}
			if verdict == operator.Inconclusive && final.Diagnostics[0].Reason != "media-undecided" || final.Gates[0].Reason != "" {
				t.Fatal("undecided media needs its own reason")
			}
		})
	}
}

func TestUnusedPendingReasonIsRejected(t *testing.T) {
	reason := "pooled-qualification-pending"
	if diagnosticCode(reason) != "unclassified" || ErrorCategory("Error", reason) != "unclassified" {
		t.Fatal("unused pending reason remains allow-listed")
	}
}
