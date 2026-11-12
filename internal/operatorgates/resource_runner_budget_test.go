package operatorgates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/oracletest"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

// Run the real resource preparation, command and collection wiring. Only the
// peer's fixed protocol replies and elapsed sleeps are simulated. The coverage
// case retains one receipt and checks that open-tail drainage happens once,
// with byte-identical coverage in the final judge input and report.
func TestResourceRunnerCallerStartBudget(t *testing.T) {
	for _, mode := range []string{"two-minute-preparation", "two-minute-caller-start", "late-caller-start", "coverage-drain", "coverage-skipped", "coverage-judge-error"} {
		t.Run(mode, func(t *testing.T) {
			coverageMode := strings.HasPrefix(mode, "coverage-")
			runtime, peer, _ := runtimeFixture(t)
			s := runtime.Session
			clock := s.clock.(*FakeClock)
			clock.Now = time.Now().Truncate(time.Millisecond)
			start := clock.Now
			var err error
			s.Budget, err = NewBudget(clock, strconv.FormatInt(start.UnixMilli(), 10), strconv.FormatInt(start.Add(SessionMaximum).UnixMilli(), 10))
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range []operator.Operation{operator.CallerReady, operator.CallerStart, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate} {
				s.capabilities[op] = true
			}
			judge := []byte("local judge identity")
			sum := sha256.Sum256(judge)
			binary := filepath.Join(t.TempDir(), "judge")
			if err := os.WriteFile(binary, judge, 0600); err != nil {
				t.Fatal(err)
			}
			s.scenario.Judge = operator.Identity{SourceCommit: runtime.Pins[0].Commit, SHA256: hex.EncodeToString(sum[:])}
			s.scenario.Caller = s.scenario.Judge
			var manifest operatorcaller.Manifest
			var mediaStart time.Time
			replies := map[string][]byte{}
			prepared, procedure, collection, observedAfterStart := false, false, false, false
			preparationDelayed := false
			drainReads := 0
			var judgedCoverage []byte
			original := peer.respond
			peer.respond = func(q operator.Request) operator.Reply {
				payload := func(v any) operator.Reply {
					return operator.Reply{Transport: operator.Succeeded, ExitCode: new(int), Payload: oracletest.Marshal(t, v)}
				}
				switch q.Operation {
				case operator.CallerReady:
					var args CallerRequest
					if err := json.Unmarshal(q.Args, &args); err != nil {
						t.Fatal(err)
					}
					manifest = args.Manifest
					if mode == "two-minute-caller-start" {
						clock.Advance(2 * time.Minute)
					}
					if mode == "late-caller-start" {
						clock.Advance(4 * time.Minute)
					}
					return payload(CallerProof{Ready: true})
				case operator.CallerStart:
					id := manifest.Identity
					host := operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "test", CallerGOMAXPROCS: 2, CapturedAt: clock.Wall()}
					replies[id.Job()+"/setup-ready.json"] = oracletest.Marshal(t, operatorcaller.Setup{RunID: id.RunID, PublicSHA: id.PublicSHA, CallerID: id.CallerID, Attempt: id.Attempt, Job: "caller-1", SignalRTTsNS: []int64{1}})
					replies[id.Job()+"/ready.json"] = oracletest.Marshal(t, operatorcaller.Ready{RunID: id.RunID, PublicSHA: id.PublicSHA, CallerID: id.CallerID, Attempt: id.Attempt, Job: "caller-1", PID: 1, Sessions: []string{"original"}, Host: host})
					replies[id.Job()+"/finished.json"] = oracletest.Marshal(t, map[string]any{"runId": id.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": []string{"original"}})
					replies[id.Job()+"/caller.jsonl"] = []byte("incomplete evidence")
					return payload(CallerProof{Start: operatorcaller.Started})
				case operator.CallerStop:
					if !clock.Wall().Before(time.UnixMilli(manifest.Deadline)) {
						t.Fatal("stop exceeded immutable caller cutoff")
					}
					return payload(CallerProof{Inactive: true})
				case operator.MailboxCreate:
					var args operatorcaller.MailboxArgs
					if err := json.Unmarshal(q.Args, &args); err != nil {
						t.Fatal(err)
					}
					if strings.Contains(args.Key, "/feed/") {
						var feed operatorcaller.Feed
						if err := json.Unmarshal(args.Data, &feed); err != nil {
							t.Fatal(err)
						}
						if feed.Sequence >= 0 {
							burst := operatorcaller.Burst{StartedAt: clock.Wall().Add(-time.Second), FinishedAt: clock.Wall()}
							for range operatorcaller.ClockBurstCount {
								burst.Probes = append(burst.Probes, operatorcaller.Probe{RTTNS: 1, CallerSend: burst.StartedAt, CallerReceive: burst.FinishedAt, ServerReceive: burst.StartedAt, ServerSend: burst.StartedAt})
							}
							ack := operatorcaller.Ack{RunID: manifest.RunID, Job: "caller-1", Sequence: feed.Sequence, OperationID: feed.OperationID, Identities: feed.Payload.Identities, OK: true, ClockProbes: oracletest.Marshal(t, burst)}
							replies[manifest.Job()+"/acks/"+fmt.Sprintf("%05d", feed.Sequence)+".json"] = oracletest.Marshal(t, ack)
						}
						if feed.Payload.Kind == "start" {
							prepared = true
							mediaStart = feed.Payload.At
						}
					}
					return payload(operatorcaller.MailboxValue{State: operatorcaller.Committed})
				case operator.MailboxRead:
					var args operatorcaller.MailboxArgs
					if err := json.Unmarshal(q.Args, &args); err != nil {
						t.Fatal(err)
					}
					if strings.HasSuffix(args.Key, "/finished.json") {
						collection = true
						// Completion is unavailable until all 17 minutes of media have run.
						clock.Advance(max(0, mediaStart.Add(17*time.Minute).Sub(clock.Wall())) + 30*time.Second)
						if mode == "coverage-skipped" {
							s.Budget.end = clock.Elapsed() + referenceTiming.judgeReserve()
							s.Budget.wallEnd = clock.Wall().Add(referenceTiming.judgeReserve()).UnixMilli()
						}
						cutoff := time.UnixMilli(manifest.Deadline)
						if !clock.Wall().Before(cutoff) {
							t.Fatal("MUTATION: resource-budget-collection-cutoff")
						}
					}
					data, found := replies[args.Key]
					if !found {
						t.Fatal("unexpected mailbox read")
					}
					return payload(operatorcaller.MailboxValue{Found: &found, Data: data})
				}
				if coverageMode && q.Action == "drain" {
					move := controlplane.MoveResult{ID: "original", From: "w1", To: "w2", Kind: "move", Start: clock.Wall(), End: clock.Wall()}
					return hostReply(t, oracletest.Marshal(t, map[string]any{"kind": "ok", "started": clock.Wall(), "ended": clock.Wall(), "data": controlplane.DrainResult{Moves: []controlplane.MoveResult{move}}}))
				}
				// Stop the simulated procedure after one real receipt has been fed.
				if coverageMode && q.Action == "worker-empty" {
					return hostReply(t, []byte(`{"kind":"failed","code":"not-empty"}`))
				}
				if q.Action == "status" {
					status := controlplane.Status{Ready: true}
					if coverageMode {
						status.Pool.Cap = 2
						status.Calls = []controlplane.CallStatus{{ID: "original", Owner: "w1", Epoch: 1}}
					}
					for _, w := range []string{"w1", "w2"} {
						status.Workers = append(status.Workers, controlplane.WorkerStatus{Name: w, Address: runtime.Config.Parameters.Workers[w].Media})
					}
					return hostReply(t, oracletest.Marshal(t, map[string]any{"kind": "ok", "started": start, "ended": clock.Wall(), "data": status}))
				}
				if q.Action == "inventory" && prepared {
					observedAfterStart = true
					if mode == "two-minute-preparation" && q.Target == "shared" && !preparationDelayed {
						clock.Advance(2 * time.Minute)
						preparationDelayed = true
					}
				}
				return original(q)
			}
			var result ResourceResult
			configured, fixture := bindFixture(t, runtime, peer, "run-budget")
			wrapped := peer.respond
			peer.respond = func(q operator.Request) operator.Reply {
				if q.Action == "inventory" && s.clockBinding == nil {
					return original(q)
				}
				return wrapped(q)
			}
			runner := ResourceRunner{Config: ResourceConfig{Parameters: runtime.Config.Parameters, Pins: runtime.Pins, Bounds: configured.Bounds, Architecture: "arm64", Token: strings.Repeat("x", 64), JudgeBinary: binary, Endpoints: operatorcaller.Endpoints{RunID: "run-budget", Signaling: "https://signal.test/calls", Relay: runtime.Config.Parameters.Workers["w1"].Media.String(), DriftPPM: 1}}, Sleep: func(ctx context.Context, d time.Duration) error {
				procedure = true
				deadline, ok := ctx.Deadline()
				if !coverageMode && (!ok || time.Until(deadline) > time.Duration(manifest.Deadline-start.UnixMilli())*time.Millisecond-2*time.Minute-clock.Elapsed()+time.Second) {
					t.Fatal("MUTATION: resource-budget-procedure-cutoff")
				}
				clock.Advance(d)
				return nil
			}, Finished: func(_ *Session, r ResourceResult) { result = r }}
			runner.Config.Bounds = configured.Bounds
			runner.Config.ClockPolicy = fixture.Policy
			runner.Config.ClockLeapMode = "unsmeared"
			runner.Config.Endpoints.FrontendControlBoundNS = configured.Bounds.FrontendControlBoundNS
			runner.Config.Endpoints.FrontendWorkerBoundNS = configured.Bounds.FrontendWorkerBoundNS
			runner.Config.Endpoints.DriftPPM = 100
			judgeCalls := 0
			var poolJudgedCoverage []byte
			if coverageMode {
				runner.Judge = mediaJudgeFunc(func(_ context.Context, p operatorevidence.Pool) (operatorevidence.Summary, error) {
					judgeCalls++
					if mode == "coverage-judge-error" {
						if judgeCalls == 1 {
							return operatorevidence.Summary{}, fmt.Errorf("judge-unavailable")
						}
						return operatorevidence.Summary{Status: operator.Passed}, nil
					}
					var err error
					judgedCoverage, err = json.Marshal(p.ClockCoverage)
					if judgeCalls == 2 {
						poolJudgedCoverage = append([]byte{}, judgedCoverage...)
					}
					return operatorevidence.Summary{Status: operator.Inconclusive, InconclusiveMetrics: judgeCalls}, err
				})
				runner.CoverageRead = func(context.Context, operator.HostLeaseReadInput) (string, error) {
					drainReads++
					return packedCoverageSample(t, 1793930021, false), nil
				}
			}
			var leaseDiagnostics bytes.Buffer
			runner.Timing = &TimingRecorder{Writer: &leaseDiagnostics}
			runner.Run(context.Background(), s, peer)
			if mode == "coverage-judge-error" && (result.Status == operator.Passed || result.Gates[0].Status == operator.Passed || result.Pool.Status != operator.Passed || judgeCalls != 2) {
				t.Fatal("MUTATION: gate-media-initial-error", judgeCalls, result)
			}
			if coverageMode {
				refreshes := 0
				for _, line := range strings.Split(leaseDiagnostics.String(), "\n") {
					if !strings.Contains(line, `"type":"gate-media-refresh"`) {
						continue
					}
					var row struct {
						Gate       operator.Gate `json:"gate"`
						DurationNS *int64        `json:"durationNs"`
						Reason     string        `json:"reason"`
					}
					want := ""
					if mode == "coverage-judge-error" {
						want = "media-refresh-unavailable"
					}
					if json.Unmarshal([]byte(line), &row) != nil || row.Gate != operator.Rollout || row.DurationNS == nil || *row.DurationNS < 0 || row.Reason != want {
						t.Fatal("MUTATION: gate-media-refresh-timing", line)
					}
					refreshes++
				}
				if refreshes != 1 {
					t.Fatal("MUTATION: gate-media-refresh-timing", refreshes)
				}
			}
			if !strings.Contains(leaseDiagnostics.String(), `"type":"clock-lease-observation"`) {
				t.Fatal("MUTATION: lease-observation-wired", leaseDiagnostics.String())
			}
			if mode == "coverage-drain" {
				if judgeCalls != 3 || result.Procedures[0].Media.InconclusiveMetrics != judgeCalls {
					t.Fatal("MUTATION: resource-gate-media-refresh", judgeCalls, result.Procedures)
				}
				reportCoverage, err := json.Marshal(result.ClockCoverage)
				if err != nil || drainReads != 40 || result.ClockCoverage == nil || len(result.ClockCoverage.Windows) != 1 || len(result.ClockCoverage.DrainAttempts) != 10 || string(poolJudgedCoverage) != string(reportCoverage) {
					t.Fatalf("MUTATION: resource-final-drain reads=%d procedures=%+v", drainReads, result.Procedures)
				}
			}
			if mode == "coverage-skipped" {
				var input struct {
					DrainAttempts []clocks.CoverageAttempt `json:"drainAttempts"`
				}
				if json.Unmarshal(judgedCoverage, &input) != nil || len(input.DrainAttempts) != 0 || drainReads == 0 {
					t.Fatal("MUTATION: coverage-prepared", drainReads, "remaining", s.Budget.Remaining(), "report-drain", len(result.ClockCoverage.DrainAttempts), "windows", len(result.ClockCoverage.Windows), "complete", result.ClockCoverage.DrainComplete)
				}
			}
			if !prepared {
				t.Fatal("caller preparation was not exercised")
			}
			if mode != "late-caller-start" {
				if !procedure || !collection || !observedAfterStart {
					t.Fatal("MUTATION: resource-budget-admission", result.Gates)
				}
				if manifest.Deadline-manifest.CreatedAt != operatorcaller.CallerMaximum.Milliseconds() || !result.CallerStopped[operator.Rollout] {
					t.Fatal("caller cutoff or stop confirmation changed")
				}
			} else if procedure || collection || observedAfterStart || result.Gates[0].Reason != "gate-deadline" {
				t.Fatal("MUTATION: resource-budget-refusal", result.Gates)
			}
		})
	}
}
