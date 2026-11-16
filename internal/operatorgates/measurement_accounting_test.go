package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/mediaworker"
	"github.com/relaishq/relais/pkg/operator"
)

type accountingStore struct {
	roundTripStore
	before StoreObservation
}

func (s *accountingStore) Observe(ctx context.Context, end time.Duration) (StoreObservation, error) {
	if !s.changed {
		return s.before, nil
	}
	return s.roundTripStore.Observe(ctx, end)
}

// Pin both current production conversions and their public projection. The
// executable legacy characterization separately retains the six old outputs.
func TestMeasurementProcedureAccounting(t *testing.T) {
	for _, gate := range []operator.Gate{operator.Failover, operator.Load} {
		for _, path := range []string{"success", "refusal", "error"} {
			t.Run(fmt.Sprintf("%s/%s", gate, path), func(t *testing.T) {
				w, p := newRoundTripWorld(t, false)
				ports := MeasurePorts{ProcedurePorts: p, Calls: 1, Observe: w.runtime.Observe,
					Store:       &accountingStore{before: StoreObservation{Primary: "before", Available: true, Durability: "sync"}},
					BeforeFault: func(context.Context, time.Duration) error { return nil }, MarkFault: func(context.Context) error { return nil },
					Collect: func(context.Context, time.Duration) <-chan MeasurementCollection {
						ch := make(chan MeasurementCollection, 1)
						ch <- MeasurementCollection{}
						close(ch)
						return ch
					},
				}
				if path == "refusal" {
					if gate == operator.Failover {
						ports.Store = &accountingStore{before: StoreObservation{Primary: "before", Available: true, Durability: "async"}}
					} else {
						ports.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
							return MeasurementObservation{}, fault("missing-live-inventory")
						}
					}
				}
				if path == "error" {
					ports.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
						return MeasurementObservation{}, errors.New("unsafe raw payload 192.0.2.1")
					}
				}
				measured := RunMeasureGate(context.Background(), gate, w.runtime.Config, ports, GateMaximum)
				out := measureProcedureResult(measured)
				if gate == operator.Load {
					admitted := RunAdmission(context.Background(), AdmissionPorts{Survivor: func(context.Context, time.Duration) MeasureResult { return measured }, StopSurvivor: func(context.Context, time.Duration) bool { return true }, Idle: func(context.Context, time.Duration) error { return nil }, Boundary: func(context.Context, time.Duration) MeasureResult { return measured }}, GateMaximum)
					out = admissionProcedureResult(admitted, nil)
				}
				wantReason := ""
				completed := path == "success"
				measuredSteps, procedureSteps := 1, 1
				if path == "error" {
					wantReason = "command-result-unknown"
				}
				if gate == operator.Failover {
					if path == "success" {
						measuredSteps, procedureSteps = 19, 19
					}
					if path == "refusal" {
						wantReason = "missing-synchronous-primary:durability"
						measuredSteps, procedureSteps = 3, 3
					}
				} else {
					if path == "success" {
						measuredSteps, procedureSteps = 2, 6
					}
					if path == "refusal" {
						wantReason = "missing-live-inventory"
						procedureSteps = 4
					}
				}
				outcomeReason := ""
				if path == "error" {
					outcomeReason = "command-result-unknown"
				}
				if measured.Status != operator.Inconclusive || measured.Reason != outcomeReason || measured.Measurement.ErrorReason != wantReason || measured.Completed != completed || measured.CompletedSteps != measuredSteps || out.Completed != completed || out.Process.CompletedSteps != procedureSteps || out.Reason != outcomeReason {
					t.Fatalf("MUTATION: measurement-accounting: measured=%+v procedure=%+v", measured, out)
				}
				s, _ := fixtureSession(t)
				seq := SequenceResult{Gates: []operator.GateResult{out.GateResult(gate)}, Procedures: []ProcedureResult{out}}
				s.retainGateDiagnostics(&seq, nil)
				raw, err := json.Marshal(s.Final(seq.Gates).Diagnostics)
				if err != nil {
					t.Fatal(err)
				}
				procedureReason := wantReason
				gateReason := outcomeReason
				if !completed && gateReason == "" {
					gateReason = "procedure-incomplete"
				}
				reasonJSON := ""
				if procedureReason != "" {
					reasonJSON = fmt.Sprintf(`,"reason":%q`, procedureReason)
				} else {
					gateReason = "media-unavailable"
				}
				want := fmt.Sprintf(`[{"reason":%q,"gate":%q,"procedure":{"completed":%t%s,"completedSteps":%d},"poolPending":false,"callerStopped":false}]`, gateReason, gate, completed, reasonJSON, procedureSteps)
				if string(raw) != want {
					t.Fatalf("MUTATION: measurement-diagnostics: got %s want %s", raw, want)
				}
			})
		}
	}
}

func TestFailoverPrimaryRefusalReason(t *testing.T) {
	for _, condition := range []string{"durability", "availability", "primary"} {
		t.Run(condition, func(t *testing.T) {
			w, p := newRoundTripWorld(t, false)
			store := &accountingStore{before: StoreObservation{Primary: "before", Available: true, Durability: "sync"}}
			switch condition {
			case "durability":
				store.before.Durability = "async"
			case "availability":
				store.before.Available = false
			case "primary":
				store.before.Primary = ""
			}
			m := RunMeasureGate(context.Background(), operator.Failover, w.runtime.Config, MeasurePorts{ProcedurePorts: p, Calls: 1, Observe: w.runtime.Observe, Store: store}, GateMaximum)
			want := "missing-synchronous-primary:" + condition
			out := measureProcedureResult(m)
			s, _ := fixtureSession(t)
			s.gates = []operator.Gate{operator.Failover}
			seq := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult { return out }})
			final := s.Final(seq.Gates)
			if m.Completed || store.changed || m.Reason != "" || m.Measurement.ErrorReason != want || m.CompletedSteps != 2 || m.Measurement.MissingSamples != 2 || final.Diagnostics[0].Procedure.Reason != want || final.Diagnostics[0].Reason != "procedure-incomplete" || final.Gates[0].Reason != "" {
				t.Fatalf("MUTATION: failover-primary-refusal: measured=%+v final=%+v", m, final)
			}
		})
	}
}

func TestMeasurementErrorRetention(t *testing.T) {
	for _, phase := range []string{"observe", "collection", "persist", "collection-observe", "collection-unclassified", "persist-unclassified", "unsafe"} {
		t.Run(phase, func(t *testing.T) {
			g, clock := testProcedure(t)
			var reads []time.Duration
			p := measureFixture(g, clock, 1, &reads)
			fail := fault("precondition-failed")
			want := "precondition-failed"
			switch phase {
			case "observe":
				p.Observe = func(context.Context, time.Duration) (MeasurementObservation, error) {
					return MeasurementObservation{}, fail
				}
			case "collection":
				p.Collect = func(context.Context, time.Duration) <-chan MeasurementCollection {
					ch := make(chan MeasurementCollection, 1)
					ch <- MeasurementCollection{Err: fail}
					close(ch)
					return ch
				}
			case "persist":
				p.Persist = func(context.Context, []MeasurementObservation) error { return fail }
			case "collection-observe":
				ch := make(chan MeasurementCollection, 1)
				p.Collect = func(context.Context, time.Duration) <-chan MeasurementCollection { return ch }
				observe := p.Observe
				p.Observe = func(ctx context.Context, end time.Duration) (MeasurementObservation, error) {
					if len(reads) == 1 {
						ch <- MeasurementCollection{Err: fault("invalid-blob")}
						close(ch)
						return MeasurementObservation{}, fail
					}
					return observe(ctx, end)
				}
			case "collection-unclassified":
				want = "measurement-collection-failed"
				p.Collect = func(context.Context, time.Duration) <-chan MeasurementCollection {
					ch := make(chan MeasurementCollection, 1)
					ch <- MeasurementCollection{Err: errors.New("unsafe evidence payload")}
					close(ch)
					return ch
				}
			case "persist-unclassified":
				want = "measurement-persist-failed"
				p.Persist = func(context.Context, []MeasurementObservation) error { return errors.New("unsafe persistence payload") }
			case "unsafe":
				want = "unclassified"
				p.Persist = func(context.Context, []MeasurementObservation) error {
					return fault("unsafe raw payload 192.0.2.1 " + strings.Repeat("secret", 1000))
				}
			}
			m := RunMeasureGate(context.Background(), operator.Load, g.c, p, g.end)
			out := measureProcedureResult(m)
			s, _ := fixtureSession(t)
			s.gates = []operator.Gate{operator.Load}
			seq := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult { return out }})
			final := s.Final(seq.Gates)
			raw, err := json.Marshal(final.Diagnostics)
			if err != nil {
				t.Fatal(err)
			}
			if m.Completed || m.Measurement.MissingSamples < 1 || m.Measurement.ErrorReason != want || m.Reason != "" || s.Latched() || final.Diagnostics[0].Procedure.Reason != want || len(raw) > 1024 || strings.Contains(string(raw), "unsafe") || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "192.0.2.1") {
				t.Fatalf("MUTATION: measurement-error-retention: measured=%+v diagnostic=%s", m, raw)
			}
			if phase == "collection-observe" && m.Measurement.MissingSamples != 3 {
				t.Fatal("measurement errors lost their existing counts", m.Measurement.MissingSamples)
			}
		})
	}
}

// The two populations each run the real measurement definition. Stop and idle
// are separate confirmed admission steps. Media qualification stays inconclusive.
func TestLoadAdmissionCompletesBothMeasurements(t *testing.T) {
	g, clock := testProcedure(t)
	measure := func(calls int) MeasureResult {
		var reads []time.Duration
		p := measureFixture(g, clock, 1, &reads)
		p.Calls = calls
		observe := p.Observe
		p.Observe = func(ctx context.Context, end time.Duration) (MeasurementObservation, error) {
			o, err := observe(ctx, end)
			if calls == 2 {
				o.Status.Calls = append(o.Status.Calls, controlplane.CallStatus{ID: "boundary", Owner: "w2", Epoch: 1})
				local := o.Local["w2"]
				local.Sessions = 1
				local.Inventory = []mediaworker.InstalledSession{{ID: "boundary", Epoch: 1, Owner: g.c.Parameters.Workers["w2"].Media}}
				o.Local["w2"] = local
			}
			return o, err
		}
		return RunMeasureGate(context.Background(), operator.Load, g.c, p, g.end)
	}
	r := RunAdmission(context.Background(), AdmissionPorts{Clock: clock, Survivor: func(context.Context, time.Duration) MeasureResult { return measure(1) }, StopSurvivor: func(context.Context, time.Duration) bool { return true }, Idle: func(context.Context, time.Duration) error { return nil }, Boundary: func(context.Context, time.Duration) MeasureResult { return measure(2) }}, g.end)
	out := admissionProcedureResult(r, nil)
	s, _ := fixtureSession(t)
	s.gates = []operator.Gate{operator.Load}
	seq := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult { return out }})
	d := s.Final(seq.Gates).Diagnostics[0]
	if !r.Completed || !r.Survivor.Completed || !r.Boundary.Completed || r.CompletedSteps != 4 || *r.Survivor.Measurement.AdmittedCalls != 1 || *r.Boundary.Measurement.AdmittedCalls != 2 || !d.Procedure.Completed || d.Procedure.CompletedSteps != 4 || d.Procedure.Reason != "" || d.Reason == "procedure-incomplete" || seq.Gates[0].Status != operator.Inconclusive {
		t.Fatalf("MUTATION: load-admission-completion: result=%+v diagnostic=%+v", r, d)
	}
}

func TestAdmissionPartialAccounting(t *testing.T) {
	for _, phase := range []string{"stop", "idle", "boundary"} {
		t.Run(phase, func(t *testing.T) {
			r := RunAdmission(context.Background(), AdmissionPorts{
				Survivor: func(context.Context, time.Duration) MeasureResult {
					return MeasureResult{Completed: true, CompletedSteps: 3, Status: operator.Inconclusive}
				},
				StopSurvivor: func(context.Context, time.Duration) bool { return phase != "stop" },
				Idle: func(context.Context, time.Duration) error {
					if phase == "idle" {
						return fault("precondition-failed")
					}
					return nil
				},
				Boundary: func(context.Context, time.Duration) MeasureResult {
					return MeasureResult{CompletedSteps: 1, Status: operator.Inconclusive, Reason: "precondition-failed"}
				},
			}, time.Minute)
			wantSteps, wantReason := 3, "command-result-unknown"
			if phase == "idle" {
				wantSteps, wantReason = 4, "precondition-failed"
			}
			if phase == "boundary" {
				wantSteps, wantReason = 6, "precondition-failed"
			}
			out := admissionProcedureResult(r, nil)
			if out.Completed || out.Process.CompletedSteps != wantSteps || out.Reason != wantReason || out.ProcedureReason != wantReason {
				t.Fatalf("MUTATION: admission-partial-accounting: %+v", out)
			}
		})
	}
}

type recoveryErrorStore struct {
	roundTripStore
	refused bool
}

func (s *recoveryErrorStore) Observe(ctx context.Context, end time.Duration) (StoreObservation, error) {
	if s.changed && !s.refused {
		s.refused = true
		return StoreObservation{}, fault("precondition-failed")
	}
	return s.roundTripStore.Observe(ctx, end)
}
func TestFailoverRecoveryMeasurementError(t *testing.T) {
	w, p := newRoundTripWorld(t, false)
	m := RunMeasureGate(context.Background(), operator.Failover, w.runtime.Config, MeasurePorts{ProcedurePorts: p, Calls: 1, Observe: w.runtime.Observe, Store: &recoveryErrorStore{}, BeforeFault: func(context.Context, time.Duration) error { return nil }, MarkFault: func(context.Context) error { return nil }}, GateMaximum)
	out := measureProcedureResult(m)
	if m.Completed || m.Measurement.RecoveredMS == nil || m.Measurement.ErrorReason != "precondition-failed" || m.Measurement.MissingSamples < 1 || out.ProcedureReason != "precondition-failed" {
		t.Fatalf("MUTATION: failover-recovery-error: %+v", m)
	}
}

func TestMeasurementWarmupRefusalReason(t *testing.T) {
	g, clock := testProcedure(t)
	g.p.Broker.(*testGateBroker).reg.RegisteredAt = nil
	var reads []time.Duration
	p := measureFixture(g, clock, 1, &reads)
	p.Store = &roundTripStore{}
	m := RunMeasureGate(context.Background(), operator.Failover, g.c, p, g.end)
	if m.Completed || m.Reason != "" || m.Measurement.ErrorReason != "missing-fault-registration" {
		t.Fatalf("missing warmup refusal: %+v", m)
	}
}

func TestAdmissionRetainsSurvivorError(t *testing.T) {
	const reason = "measurement-collection-failed"
	boundary := false
	r := RunAdmission(context.Background(), AdmissionPorts{
		Survivor: func(context.Context, time.Duration) MeasureResult {
			return MeasureResult{CompletedSteps: 1, Status: operator.Inconclusive, Reason: reason, Measurement: Measurement{ErrorReason: reason}}
		},
		StopSurvivor: func(context.Context, time.Duration) bool { return true },
		Idle:         func(context.Context, time.Duration) error { return nil },
		Boundary: func(context.Context, time.Duration) MeasureResult {
			boundary = true
			return MeasureResult{Completed: true, CompletedSteps: 2, Status: operator.Inconclusive}
		},
	}, time.Minute)
	out := admissionProcedureResult(r, nil)
	s, _ := fixtureSession(t)
	s.gates = []operator.Gate{operator.Load}
	seq := s.RunSequence(context.Background(), SequencePorts{Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult { return out }})
	final := s.Final(seq.Gates)
	if !boundary || r.Completed || r.Reason != reason || out.Reason != reason || out.ProcedureReason != reason || s.Latched() || out.Process.CompletedSteps != 5 || final.Diagnostics[0].Procedure.Reason != reason {
		t.Fatalf("MUTATION: admission-survivor-error: result=%+v procedure=%+v", r, out)
	}
}

func TestAdmissionKeepsFirstDiagnosticAndBoundaryOutcome(t *testing.T) {
	r := RunAdmission(context.Background(), AdmissionPorts{
		Survivor: func(context.Context, time.Duration) MeasureResult {
			return MeasureResult{Status: operator.Inconclusive, Reason: "precondition-failed", Measurement: Measurement{ErrorReason: "missing-live-inventory"}}
		},
		StopSurvivor: func(context.Context, time.Duration) bool { return true },
		Idle:         func(context.Context, time.Duration) error { return nil },
		Boundary: func(context.Context, time.Duration) MeasureResult {
			return MeasureResult{Status: operator.Inconclusive, Reason: "caller-not-near", Measurement: Measurement{ErrorReason: "measurement-collection-failed"}}
		},
	}, time.Minute)
	out := admissionProcedureResult(r, nil)
	if out.ProcedureReason != "missing-live-inventory" || out.Reason != "caller-not-near" {
		t.Fatalf("MUTATION: admission-first-diagnostic: %+v", out)
	}
}
