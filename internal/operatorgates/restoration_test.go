package operatorgates

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

func TestFailedRestorationCannotQualifyForPool(t *testing.T) {
	for _, failure := range []string{"refused-unfreeze", "restore-unsettled", "restore-unconfirmed"} {
		for _, release := range []bool{false, true} {
			t.Run(failure+map[bool]string{false: "/process", true: "/release"}[release], func(t *testing.T) {
				g, clock := testProcedure(t)
				s, _ := fixtureSession(t)
				s.clock, s.Budget.clock = clock, clock
				g.c.Pool = true
				g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
				if failure == "refused-unfreeze" {
					g.frozen = true
					clock.Advance(g.end - 29*time.Second)
				} else {
					g.down = []string{"w1"}
				}
				g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, _ ProcessExtra) (StepResult, error) {
					if op == "unfreeze" {
						certainty, err := s.Exchange(ctx, &observationPeer{}, operator.HostExecute, slot, op, nil, nil, end, func(operator.Reply) operator.Certainty { return operator.Failed })
						if err != nil {
							return StepResult{Certainty: certainty}, err
						}
						return StepResult{Certainty: certainty}, fault("precondition-failed:deadline-before-submission")
					}
					return StepResult{Status: controlplane.Status{Workers: []controlplane.WorkerStatus{{Name: "w1", Dead: failure == "restore-unconfirmed", Recovering: failure == "restore-unsettled"}}}}, nil
				}
				r := g.finish(context.Background(), nil, release)
				if r.Status != operator.Inconclusive || r.Reason != "restore-failed" || r.Completed || r.AwaitingPool {
					t.Fatalf("MUTATION: restoration-required: failed restoration qualified: %+v", r)
				}
				if failure != "refused-unfreeze" && r.ProcedureReason != failure {
					t.Fatal("restoration cause lost", r)
				}
				s.gates = []operator.Gate{operator.Rollout, operator.ControlRestart}
				commands, pools := 0, 0
				out := s.RunSequence(context.Background(), SequencePorts{
					Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult { commands++; return r },
					Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
						pools++
						return &MediaSummary{Status: operator.Passed}, nil
					},
				})
				if commands != 1 || pools != 1 || !s.Latched() || out.Status != operator.Inconclusive || out.Gates[0].Reason != "restore-failed" || out.Gates[1].Reason != "prior-gate-stopped" {
					t.Fatal("failed restoration escaped sequence fence", out, commands, pools)
				}
			})
		}
	}
}

func TestPoolPromotionRequiresCompletedProcedure(t *testing.T) {
	for _, completed := range []bool{false, true} {
		s, _ := fixtureSession(t)
		out := s.RunSequence(context.Background(), SequencePorts{
			Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
				return ProcedureResult{Status: operator.Inconclusive, AwaitingPool: true, Completed: completed, Media: &MediaSummary{Status: operator.Passed}}
			},
			Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
				return &MediaSummary{Status: operator.Passed}, nil
			},
		})
		if (out.Gates[0].Status == operator.Passed) != completed {
			t.Fatal("MUTATION: pool-requires-completion: pool promoted an incomplete procedure", out)
		}
	}
}

func TestFailedRestorationCannotPassWithoutPool(t *testing.T) {
	g, _ := testProcedure(t)
	g.frozen = true
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		return StepResult{}, fault("precondition-failed:deadline-before-submission")
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	r := g.finish(context.Background(), nil, false)
	if r.Status != operator.Inconclusive || r.Reason != "restore-failed" || r.Completed || r.AwaitingPool {
		t.Fatal("unrestored procedure passed without a pool", r)
	}
}

func TestRestorationFailureStopsSuccessors(t *testing.T) {
	s, _ := fixtureSession(t)
	s.gates = []operator.Gate{operator.Rollout, operator.ControlRestart}
	commands, pools := 0, 0
	out := s.RunSequence(context.Background(), SequencePorts{
		Command: func(context.Context, operator.Gate, time.Duration) ProcedureResult {
			commands++
			return ProcedureResult{Status: operator.Inconclusive, Reason: "restore-failed", Media: &MediaSummary{Status: operator.Passed}}
		},
		Finalize: func(context.Context, time.Duration) (*MediaSummary, error) {
			pools++
			return &MediaSummary{Status: operator.Passed}, nil
		},
	})
	if commands != 1 || pools != 1 || !s.Latched() || out.Gates[1].Reason != "prior-gate-stopped" || out.Status != operator.Inconclusive {
		t.Fatal("restoration failure must latch and retain the final pool", out)
	}
}

func TestProcedureReservesUnfreezeAdmission(t *testing.T) {
	for _, gate := range []operator.Gate{operator.Rollout, operator.UpgradeRollback} {
		t.Run(string(gate), func(t *testing.T) {
			g, clock := testProcedure(t)
			s, _ := fixtureSession(t)
			s.clock, s.Budget.clock = clock, clock
			var mainEnd, restoreEnd time.Duration
			var admitted int64
			peer := &observationPeer{respond: func(req operator.Request) operator.Reply {
				if req.Action == "unfreeze" {
					admitted = req.RemainingMS
				}
				clock.Advance(3050 * time.Millisecond)
				return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int)}
			}}
			g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, _ ProcessExtra) (StepResult, error) {
				switch op {
				case "status":
					return StepResult{Status: controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{{ID: "original", Owner: "w1", Epoch: 1}}, Workers: []controlplane.WorkerStatus{{Name: "w1", Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}}, nil
				case "freeze":
					mainEnd = end
					clock.Advance(end - clock.Elapsed())
					return StepResult{}, fault("gate-deadline")
				case "unfreeze":
					restoreEnd = end
					for _, action := range []string{"inventory", "inventory", "inventory", "inventory", "clock", "clock", "clock", "clock", "unfreeze"} {
						certainty, err := s.Exchange(ctx, peer, operator.HostExecute, slot, action, nil, nil, end, func(r operator.Reply) operator.Certainty {
							if r.NotSubmitted {
								return operator.Failed
							}
							return operator.Succeeded
						})
						if err != nil || certainty != operator.Succeeded {
							return StepResult{Certainty: certainty}, fault("precondition-failed:deadline-before-submission")
						}
					}
					return StepResult{}, nil
				default:
					t.Fatal("unexpected step", op)
				}
				return StepResult{}, nil
			}
			g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
			var got ProcedureResult
			if gate == operator.UpgradeRollback {
				got = RunReleaseGate(context.Background(), g.c, g.p, g.end)
			} else {
				got = RunProcessGate(context.Background(), gate, g.c, g.p, g.end)
			}
			if mainEnd != g.end-time.Minute || restoreEnd != g.end || admitted < 31000 || peer.sent != 9 || got.Completed || got.AwaitingPool || got.Status != operator.Inconclusive || got.Reason != "gate-deadline" {
				t.Fatalf("restoration reserve missing: main=%s restore=%s admission=%d result=%+v", mainEnd, restoreEnd, admitted, got)
			}
			t.Logf("main cutoff=%s restoration cutoff=%s unfreeze remaining=%dms", mainEnd, restoreEnd, admitted)
		})
	}
}

func TestUnfreezePrecedesWorkerRestorationWait(t *testing.T) {
	g, _ := testProcedure(t)
	g.frozen, g.down = true, []string{"w1"}
	var operations []string
	g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
		operations = append(operations, op)
		return StepResult{Status: controlplane.Status{Workers: []controlplane.WorkerStatus{{Name: "w1", Recovering: true}}}}, nil
	}
	if err := g.restoreGate(context.Background()); code(err) != "restore-unsettled" || len(operations) == 0 || operations[0] != "unfreeze" || g.frozen {
		t.Fatal("worker restoration consumed the unfreeze reserve", operations, err)
	}
}

func TestDefiniteUnfreezeFailureStillRestoresWorkers(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker-recovered", true: "known-call-loss"}[lost], func(t *testing.T) {
			g, _ := testProcedure(t)
			g.frozen, g.down = true, []string{"w1"}
			first := fault("precondition-failed:deadline-before-submission")
			var operations []string
			started := false
			g.p.Step = func(_ context.Context, op, _ string, end time.Duration, _ ProcessExtra) (StepResult, error) {
				operations = append(operations, op)
				if end != g.restoreEnd {
					t.Fatal("restoration changed its original cutoff")
				}
				switch op {
				case "unfreeze":
					return StepResult{}, first
				case "status":
					s := controlplane.Status{Workers: []controlplane.WorkerStatus{{Name: "w1", Dead: !started, Address: g.c.Parameters.Workers["w1"].Media}}}
					if lost {
						s.LostCount = 1
					}
					return StepResult{Status: s}, nil
				case "replace":
					return StepResult{}, nil
				case "worker-start":
					started = true
					return StepResult{}, nil
				default:
					t.Fatal("unexpected restoration step", op)
				}
				return StepResult{}, nil
			}
			err := g.restoreGate(context.Background())
			want := "unfreeze,status,replace,worker-start,status"
			if lost {
				want = "unfreeze,status"
			}
			if err != first || strings.Join(operations, ",") != want || !g.frozen || !lost && (len(g.down) != 0 || !started) {
				t.Fatal("definite unfreeze refusal skipped recovery or replaced the first failure", err, operations)
			}
		})
	}
}

func TestUnknownUnfreezeStillFencesWorkerRestoration(t *testing.T) {
	g, _ := testProcedure(t)
	g.frozen, g.down = true, []string{"w1"}
	steps := 0
	g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
		steps++
		if op != "unfreeze" {
			t.Fatal("worker restoration followed unknown unfreeze", op)
		}
		return StepResult{}, fault("command-result-unknown")
	}
	if err := g.restoreGate(context.Background()); code(err) != "command-result-unknown" || !g.unknown || steps != 1 {
		t.Fatal("unknown unfreeze did not fence restoration", err, steps)
	}
}

func TestUnfreezeFailureDoesNotHideWorkerRestoreLoss(t *testing.T) {
	g, _ := testProcedure(t)
	g.frozen, g.down = true, []string{"w1"}
	g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
		if op == "unfreeze" {
			return StepResult{}, fault("compatibility-refused")
		}
		if op != "status" {
			t.Fatal("unexpected step after known loss", op)
		}
		return StepResult{Status: controlplane.Status{LostCount: 1}}, nil
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	r := g.finish(context.Background(), nil, false)
	if r.Status != operator.Failure || r.Reason != "restore-failed" || r.ProcedureReason != "compatibility-refused" || r.Completed || r.AwaitingPool {
		t.Fatal("first restoration error hid known call loss", r)
	}
}

func TestRestoreFailedClearsMainFailureCategory(t *testing.T) {
	g, _ := testProcedure(t)
	g.frozen = true
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		return StepResult{}, fault("compatibility-refused")
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	r := g.finish(context.Background(), fault("precondition-failed:deadline-before-submission"), false)
	if r.Status != operator.Inconclusive || r.Reason != "restore-failed" || r.Category != "" || r.ProcedureReason != "compatibility-refused" || r.Completed || r.AwaitingPool {
		t.Fatal("restore-failed retained an unrelated main category or lost its cause", r)
	}
}

func TestRestorationDoesNotHideKnownFailureOrUncertainty(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		g, _ := testProcedure(t)
		g.frozen = true
		g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
			if unknown {
				return StepResult{}, fault("command-result-unknown")
			}
			return StepResult{}, fault("precondition-failed:deadline-before-submission")
		}
		g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Failure}, nil }
		r := g.finish(context.Background(), nil, false)
		want := "restore-failed"
		if unknown {
			want = "command-result-unknown"
		}
		if r.Status != operator.Failure || r.Reason != want || r.Completed || r.AwaitingPool {
			t.Fatal("restoration hid failure or uncertainty", r)
		}
	}
}

func TestWorkerRestorationUnknownStaysUnknown(t *testing.T) {
	g, _ := testProcedure(t)
	g.down = []string{"w1"}
	steps := 0
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		steps++
		return StepResult{}, fault("command-result-unknown")
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	r := g.finish(context.Background(), nil, false)
	if r.Reason != "command-result-unknown" || !g.unknown || steps != 1 || r.Status != operator.Inconclusive || r.Completed || r.AwaitingPool {
		t.Fatal("MUTATION: worker-restoration-unknown: unknown recovery was renamed or retried", r)
	}
}

func TestRestorationCannotUseCollectionReserve(t *testing.T) {
	g, clock := testProcedure(t)
	g.c.Pool, g.frozen = true, true
	g.p.CollectionEnd = g.end + time.Minute
	g.p.Step = func(context.Context, string, string, time.Duration, ProcessExtra) (StepResult, error) {
		clock.Advance(g.end - clock.Elapsed() + time.Second)
		return StepResult{}, nil
	}
	g.p.Media = func(context.Context) (*MediaSummary, error) { return &MediaSummary{Status: operator.Passed}, nil }
	r := g.finish(context.Background(), nil, false)
	if r.Status != operator.Inconclusive || r.Reason != "gate-deadline" || r.Completed || r.AwaitingPool {
		t.Fatal("late restoration borrowed the collection reserve", r)
	}
}

func TestTraceLabelsAreAllowListed(t *testing.T) {
	s, _ := fixtureSession(t)
	for _, labels := range [][2]string{{"shared", "unfreeze"}, {"private-value", "private-value"}} {
		r, err := s.Issue(operator.HostExecute, labels[0], labels[1], nil, nil, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Accept(s.ID, reply(r, operator.Succeeded), operator.Succeeded); err != nil {
			t.Fatal(err)
		}
	}
	for _, purpose := range []string{"trial-takeover", "trial-settled", "trial-ready", "private-value"} {
		s.recordWait(purpose)
	}
	trace := s.Final(nil).Trace
	if len(trace) != 7 || trace[0].Operation != operator.HostExecute || trace[0].Target != "shared" || trace[0].Action != "unfreeze" || trace[1].Action != "unfreeze" || trace[6].WaitPurpose != "trial-ready" {
		t.Fatal("trace labels missing", trace)
	}
	raw, _ := json.Marshal(trace)
	if strings.Contains(string(raw), "private-value") {
		t.Fatal("unsafe trace labels escaped")
	}
}

func TestStalledTrialReadinessHasWaitPurpose(t *testing.T) {
	g, clock := testProcedure(t)
	s, _ := fixtureSession(t)
	g.p.Wait = s.recordWait
	call := controlplane.CallStatus{ID: "original", Owner: "w1", Epoch: 1}
	moved := false
	g.p.Step = func(_ context.Context, op, _ string, _ time.Duration, _ ProcessExtra) (StepResult, error) {
		if op == "move" {
			move := controlplane.MoveResult{ID: call.ID, From: "w1", To: "w2", Kind: "move", Start: clock.Wall(), End: clock.Wall()}
			call.Owner, call.Epoch, moved = "w2", 2, true
			return StepResult{Move: move, Raw: oracleJSON(move)}, nil
		}
		return StepResult{LossKnown: true, Status: controlplane.Status{Ready: true, Pool: controlplane.PoolSample{Cap: 2}, Calls: []controlplane.CallStatus{call}, Workers: []controlplane.WorkerStatus{{Name: "w1", Recovering: moved, Address: g.c.Parameters.Workers["w1"].Media}, {Name: "w2", Address: g.c.Parameters.Workers["w2"].Media}}}}, nil
	}
	sleep := g.p.Sleep
	g.p.Sleep = func(ctx context.Context, d time.Duration) error {
		if d == 500*time.Millisecond {
			return fault("gate-deadline")
		}
		return sleep(ctx, d)
	}
	err := g.trials(context.Background(), TrialTarget{Moves: 1}, "stable")
	trace := s.Final(nil).Trace
	if code(err) != "gate-deadline" || len(trace) != 1 || trace[0].WaitPurpose != "trial-ready" {
		t.Fatal("stalled readiness lacks a wait purpose", err, trace)
	}
}
