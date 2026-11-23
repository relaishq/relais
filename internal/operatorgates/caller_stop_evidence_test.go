package operatorgates

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/releasecompat"
	"github.com/relaishq/relais/pkg/operator"
)

type stopEvidenceJudge struct {
	err  error
	pool *operatorevidence.Pool
}

func (j stopEvidenceJudge) Judge(_ context.Context, pool operatorevidence.Pool) (operatorevidence.Summary, error) {
	if j.pool != nil {
		*j.pool = pool
	}
	return operatorevidence.Summary{Status: operator.Passed}, j.err
}

func stoppedCallerRuntime(t *testing.T, clock *FakeClock) (*CallerRuntime, map[string][]byte) {
	t.Helper()
	id := operatorcaller.Identity{RunID: "run-stop", Gate: operator.Rollout, Phase: operatorcaller.Survivor, CallerID: "1", Attempt: "1", PublicSHA: strings.Repeat("a", 40)}
	m := operatorcaller.Manifest{Version: 1, Identity: id, ArmedAt: clock.Wall().UnixMilli(), CreatedAt: clock.Wall().UnixMilli(), Deadline: clock.Wall().Add(25 * time.Minute).UnixMilli(), Plan: operatorcaller.NewPlan(id.Phase, 60, id.PublicSHA), Endpoints: operatorcaller.Endpoints{RunID: id.RunID, Signaling: "https://signal.test/calls", Relay: "192.0.2.1:24780", DriftPPM: 1}}
	values := map[string][]byte{}
	store := budgetCallerStore{read: func(key string) operatorcaller.ReadResult {
		data, found := values[key]
		return operatorcaller.ReadResult{Found: found, Data: data}
	}}
	b, err := operatorcaller.NewBroker(m, operatorcaller.NewMailbox(store), oracleCallerClock{clock}, &oracleCallerFence{})
	if err != nil {
		t.Fatal(err)
	}
	at := clock.Wall().Add(-time.Second)
	host := operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "synthetic", CallerGOMAXPROCS: 2, CapturedAt: at}
	burst := operatorcaller.Burst{StartedAt: at, FinishedAt: at}
	for range operatorcaller.ClockBurstCount {
		burst.Probes = append(burst.Probes, operatorcaller.Probe{RTTNS: 1000000, CallerSend: at, CallerReceive: at, ServerReceive: at, ServerSend: at})
	}
	reg, err := operatorcaller.Register(m.Plan, id.RunID, []string{"original"}, burst, host, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Registration = &reg
	firstFault := clock.Wall()
	b.FirstFaultAt = &firstFault
	pool := &EvidenceRuntime{RunID: id.RunID, Judge: stopEvidenceJudge{}}
	phase := operatorevidence.Phase{Clocks: clocks.Evidence{At: at}, Inventory: releasecompat.Pool{Complete: true}, DeployedSHA: id.PublicSHA}
	return NewCallerRuntime(b, clock, 20*time.Minute, pool, phase), values
}

func TestCallerRuntimeRetainsAbortedReceipt(t *testing.T) {
	for _, judgeFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "judge-returned", true: "judge-failed"}[judgeFails], func(t *testing.T) {
			_, clock := testProcedure(t)
			r, values := stoppedCallerRuntime(t, clock)
			var judged operatorevidence.Pool
			judge := stopEvidenceJudge{pool: &judged}
			if judgeFails {
				judge.err = fault("media-unavailable")
			}
			r.Pool.Judge = judge
			job := r.Broker.Manifest.Job()
			values[job+"/finished.json"] = []byte(`{"runId":"run-stop","job":"caller-1","done":false,"stopped":true,"exitCode":1,"sessions":["original"]}`)
			values[job+"/stop.json"] = []byte(`{"runId":"run-stop","job":"caller-1","reason":"Generator saturation hard ceiling"}`)
			values[job+"/caller.jsonl"] = []byte("raw report")
			_, err := r.Media(context.Background())
			wantCode := "precondition-failed:caller"
			if judgeFails {
				wantCode = "media-unavailable"
			}
			if code(err) != wantCode || !r.Pool.missing || len(r.Pool.phases) != 1 {
				t.Fatalf("aborted evidence qualified or lost cause: err %v missing %t phases %d", err, r.Pool.missing, len(r.Pool.phases))
			}
			if !judged.Missing || len(judged.Phases) != 1 || judged.Phases[0].CallerCompletion == nil || judged.Phases[0].CallerCompletion.StopReason != "generator-saturation-hard-ceiling" {
				t.Fatalf("aborted receipt did not reach judge artifacts: %+v", judged)
			}
			var retained struct {
				CallerJSONL string `json:"callerJsonl"`
				Completion  struct {
					Stopped    bool     `json:"stopped"`
					Done       bool     `json:"done"`
					ExitCode   int      `json:"exitCode"`
					Sessions   []string `json:"sessions"`
					StopReason string   `json:"stopReason"`
				} `json:"callerCompletion"`
			}
			if err := json.Unmarshal(r.Pool.phases[0].Raw, &retained); err != nil {
				t.Fatal(err)
			}
			if retained.CallerJSONL != "raw report" || !retained.Completion.Stopped || retained.Completion.Done || retained.Completion.ExitCode != 1 || len(retained.Completion.Sessions) != 1 || retained.Completion.StopReason != "generator-saturation-hard-ceiling" {
				t.Fatalf("aborted receipt missing from artifacts: %+v", retained)
			}
		})
	}
}

type runtimeStopBroker struct {
	GateBroker
	caller *CallerRuntime
}

func (b runtimeStopBroker) CheckStopped(ctx context.Context) error {
	return b.caller.CheckStopped(ctx)
}

func TestTrialReadyObservesMailboxStop(t *testing.T) {
	g, clock, polls := trialReadyProcedure(t, 0)
	r, values := stoppedCallerRuntime(t, clock)
	g.p.Broker = runtimeStopBroker{g.p.Broker, r}
	step := g.p.Step
	g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
		out, err := step(ctx, op, slot, end, extra)
		if *polls == 2 {
			values[r.Broker.Manifest.Job()+"/stop.json"] = []byte(`{"runId":"run-stop","job":"caller-1","reason":"Generator saturation hard ceiling"}`)
		}
		return out, err
	}
	err := g.trials(context.Background(), TrialTarget{Moves: 1}, "stable")
	var cause *ProcedureError
	if !errors.As(err, &cause) || cause.Code != "precondition-failed:caller" || *polls != 2 || clock.Elapsed() != ConsentWindow+time.Second {
		t.Fatalf("mailbox caller stop did not terminate polling: err %v polls %d elapsed %v", err, *polls, clock.Elapsed())
	}
	// A definite caller stop cannot clear an earlier uncertain command.
	r.Broker.Fence.Halt("command-result-unknown")
	if err := r.CheckStopped(context.Background()); code(err) != "command-result-unknown" {
		t.Fatal("caller stop cleared the uncertainty latch", err)
	}
}

func TestCallerRuntimeJudgeUncertaintyWins(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller-completed", true: "caller-stopped"}[stopped], func(t *testing.T) {
			g, clock := testProcedure(t)
			r, values := stoppedCallerRuntime(t, clock)
			r.Pool.Judge = stopEvidenceJudge{err: errors.New("judge-source-differs")}
			job := r.Broker.Manifest.Job()
			finished := map[string]any{"runId": r.RunID(), "job": "caller-1", "done": true, "stopped": stopped, "exitCode": 0, "sessions": []string{"original"}}
			var err error
			values[job+"/finished.json"], err = json.Marshal(finished)
			if err != nil {
				t.Fatal(err)
			}
			values[job+"/caller.jsonl"] = []byte("raw report")
			if stopped {
				values[job+"/stop.json"] = []byte(`{"runId":"run-stop","job":"caller-1","reason":"Generator saturation hard ceiling"}`)
			}
			var mediaErr error
			g.p.Media = func(ctx context.Context) (*MediaSummary, error) {
				media, err := r.Media(ctx)
				mediaErr = err
				return media, err
			}
			out := g.finish(context.Background(), nil, false)
			if code(mediaErr) != "command-result-unknown" || !strings.Contains(mediaErr.Error(), "judge-source-differs") || !g.unknown || out.Status == operator.Passed || out.Completed || out.AwaitingPool || out.Reason != "command-result-unknown" {
				t.Fatalf("judge uncertainty lost precedence or its latch: err %v unknown %t result %+v", mediaErr, g.unknown, out)
			}
		})
	}
}
