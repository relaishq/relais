package operatorgates

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

// statusFailedClause is the host wrapper's definite read-failure branch.
const statusFailedClause = "except Exception as error:\n    emit({'kind':'failed','code':read_failure(error)})"

// failoverStatusStore can keep the original primary after the failover request.
type failoverStatusStore struct {
	roundTripStore
	stuck bool
}

func (s *failoverStatusStore) Observe(ctx context.Context, end time.Duration) (StoreObservation, error) {
	o, err := s.roundTripStore.Observe(ctx, end)
	if s.stuck {
		o.Primary = "before"
	}
	return o, err
}

// runFailoverStatus runs the real failover gate through ProcessRuntime and the
// production measure ports. After the store failover, answer(w, n, req) may
// replace the n-th recovery reply for an action; false keeps the world's reply.
func runFailoverStatus(t *testing.T, stuck bool, answer func(w *roundTripWorld, n int, req operator.Request) (operator.Reply, bool)) (MeasureResult, *roundTripWorld, *failoverStatusStore) {
	t.Helper()
	w, p := newRoundTripWorld(t, false)
	store := &failoverStatusStore{stuck: stuck}
	peer := w.runtime.Peer.(*observationPeer)
	world := peer.respond
	seen := map[string]int{}
	peer.respond = func(req operator.Request) operator.Reply {
		if !store.changed && req.Action == "status" && strings.Contains(string(req.Script), statusFailedClause) {
			t.Fatal("MUTATION: failover-status-read-opt-in-scope: status read before the failover opted in")
		}
		if store.changed {
			seen[req.Action]++
			if reply, ok := answer(w, seen[req.Action], req); ok {
				return reply
			}
		}
		return world(req)
	}
	ports := w.runtime.measurePorts(&CallerRuntime{}, 1)
	ports.ProcedurePorts = p
	ports.Store = store
	ports.BeforeFault = func(context.Context, time.Duration) error { return nil }
	ports.MarkFault = func(context.Context) error { return nil }
	ports.Collect = nil
	return RunMeasureGate(context.Background(), operator.Failover, w.runtime.Config, ports, GateMaximum), w, store
}

// hostStatusFailure answers a refused control status read the way the real host
// wrapper does: a definite failed read only when the rendered script carries
// the read-failure branch, otherwise the unknown result of an effectful step.
func hostStatusFailure(t *testing.T, req operator.Request) operator.Reply {
	if strings.Contains(string(req.Script), statusFailedClause) {
		return hostReply(t, []byte(`{"kind":"failed","code":"connection-refused-error"}`))
	}
	return hostReply(t, []byte(`{"kind":"unknown"}`))
}

// Characterization: results that are not a definite failed status read keep
// today's outcome in the failover recovery wait.
func TestFailoverRecoveryStatusOutcomesUnchanged(t *testing.T) {
	for _, mode := range []string{"unknown", "malformed-code", "worker-status-failed"} {
		t.Run(mode, func(t *testing.T) {
			m, w, store := runFailoverStatus(t, false, func(_ *roundTripWorld, n int, req operator.Request) (operator.Reply, bool) {
				switch {
				case mode == "unknown" && req.Action == "status":
					return hostReply(t, []byte(`{"kind":"unknown"}`)), true
				case mode == "malformed-code" && req.Action == "status":
					return hostReply(t, []byte(`{"kind":"failed","code":"Connection Refused"}`)), true
				case mode == "worker-status-failed" && req.Action == "worker-status":
					return hostReply(t, []byte(`{"kind":"failed","code":"connection-refused-error"}`)), true
				}
				return operator.Reply{}, false
			})
			out := measureProcedureResult(m)
			if !store.changed || m.Measurement.RecoveredMS != nil {
				t.Fatalf("recovery wait not reached or recovered: %+v", m)
			}
			if mode == "worker-status-failed" {
				if w.runtime.Session.Latched() || m.Reason != "" || out.ProcedureReason != "unclassified" || m.Measurement.MissingSamples < 1 {
					t.Fatalf("MUTATION: failover-recovery-worker-status-failure: %+v", m)
				}
				return
			}
			if !w.runtime.Session.Latched() || m.Reason != "command-result-unknown" {
				t.Fatalf("MUTATION: failover-recovery-status-unknown: %+v", m)
			}
		})
	}
}

// A definite failed status read after the failover request is a missing sample.
// The wait keeps its 5 s poll, and a later read completes recovery.
func TestFailoverRecoveryFailedStatusReadIsMissingSample(t *testing.T) {
	var scripts []string
	m, w, _ := runFailoverStatus(t, false, func(_ *roundTripWorld, n int, req operator.Request) (operator.Reply, bool) {
		if req.Action != "status" {
			return operator.Reply{}, false
		}
		scripts = append(scripts, string(req.Script))
		if n == 1 {
			return hostStatusFailure(t, req), true
		}
		return operator.Reply{}, false
	})
	if w.runtime.Session.Latched() || m.Reason == "command-result-unknown" || m.Measurement.RecoveredMS == nil || *m.Measurement.RecoveredMS != 5000 || m.Measurement.MissingSamples < 1 || m.Measurement.ErrorReason == "" {
		t.Fatalf("MUTATION: failover-status-read-missing-sample: %+v", m)
	}
	// Only the two recovery reads opt in; the trial reads after recovery do not.
	if len(scripts) < 3 || !strings.Contains(scripts[0], statusFailedClause) || !strings.Contains(scripts[1], statusFailedClause) || strings.Contains(scripts[2], statusFailedClause) {
		t.Fatalf("MUTATION: failover-status-read-opt-in-scope: %d scripts", len(scripts))
	}
}

// Failed status reads until the gate deadline end the gate exactly as a
// primary that never changes does, with the read error retained, never a latch.
func TestFailoverRecoveryFailedStatusReadsUntilDeadline(t *testing.T) {
	keep := func(*roundTripWorld, int, operator.Request) (operator.Reply, bool) { return operator.Reply{}, false }
	baseline, _, _ := runFailoverStatus(t, true, keep)
	reads := 0
	m, w, _ := runFailoverStatus(t, false, func(_ *roundTripWorld, n int, req operator.Request) (operator.Reply, bool) {
		if req.Action != "status" {
			return operator.Reply{}, false
		}
		reads++
		return hostStatusFailure(t, req), true
	})
	if w.runtime.Session.Latched() || m.Reason != baseline.Reason || m.Status != baseline.Status || m.Completed || m.Measurement.RecoveredMS != nil || baseline.Measurement.RecoveredMS != nil ||
		m.Measurement.MissingSamples < reads || m.Measurement.ErrorReason == "" || m.ExpiredWait == nil || baseline.ExpiredWait == nil || m.ExpiredWait.Purpose != "failover-primary-recovered" || m.ExpiredWait.Purpose != baseline.ExpiredWait.Purpose || reads < 300 {
		t.Fatalf("MUTATION: failover-status-read-deadline: reads=%d %+v", reads, m)
	}
}

// A failed read may have reconciled a loss. The next successful read exposes
// lost_count, and the gate fails instead of counting recovery.
func TestFailoverRecoveryFailedStatusReadThenLossFails(t *testing.T) {
	m, w, _ := runFailoverStatus(t, false, func(w *roundTripWorld, n int, req operator.Request) (operator.Reply, bool) {
		if req.Action != "status" {
			return operator.Reply{}, false
		}
		if n == 1 {
			return hostStatusFailure(t, req), true
		}
		s := w.status()
		s.LostCount = 1
		return hostReply(t, oracleJSON(map[string]any{"kind": "ok", "started": time.Unix(1793930000, 0), "ended": time.Unix(1793930000, 0), "data": s})), true
	})
	if w.runtime.Session.Latched() || m.Status != operator.Failure || m.Measurement.RecoveredMS != nil {
		t.Fatalf("MUTATION: failover-status-read-loss: %+v", m)
	}
}

// The step classifier accepts a failed control status envelope only with the
// recovery opt-in. Without it the reply stays unknown and latches.
func TestStatusFailedEnvelopeNeedsRecoveryOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(strconv.FormatBool(optIn), func(t *testing.T) {
			w, p := newRoundTripWorld(t, false)
			w.runtime.Peer.(*observationPeer).respond = func(req operator.Request) operator.Reply {
				return hostReply(t, []byte(`{"kind":"failed","code":"connection-refused-error"}`))
			}
			_, err := p.Step(context.Background(), "status", "shared", GateMaximum, ProcessExtra{RecoveryStatus: optIn})
			if !optIn {
				if code(err) != "command-result-unknown" || !w.runtime.Session.Latched() {
					t.Fatalf("MUTATION: plain-status-failed-latches: %v", err)
				}
				return
			}
			if !errors.As(err, new(failedStatusRead)) || code(err) != "precondition-failed:connection-refused-error" || w.runtime.Session.Latched() {
				t.Fatalf("MUTATION: recovery-status-failed-definite: %v", err)
			}
		})
	}
}
