package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
)

type resourceFixture struct {
	Clock                bindingtest.Fixture
	Scenario             operator.Scenario
	Config               operatorgates.ResourceConfig
	Inventories          map[string]json.RawMessage
	ExpectedUnknownStart []string
	Replies              map[string]json.RawMessage
	OptionalCapabilities map[operator.Gate][]operator.Operation
}

func sessionFixture(t *testing.T) resourceFixture {
	t.Helper()
	raw, err := os.ReadFile("../../internal/operatorgates/testdata/resource-session.json")
	if err != nil {
		t.Fatal(err)
	}
	var f resourceFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	f.Config.JudgeBinary = filepath.Join(t.TempDir(), "judge")
	if err = os.WriteFile(f.Config.JudgeBinary, []byte("judge-stub"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *resourceFixture) bind(t *testing.T) {
	t.Helper()
	var err error
	f.Clock, err = bindingtest.New(f.Config.Endpoints.RunID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.Scenario, err = operator.BuildScenarioIdentity(f.Scenario, f.Clock.Selection.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	f.Config.Bounds = f.Clock.Selection.Receipt.Bounds
	f.Config.ClockPolicy = f.Clock.Policy
	f.Config.ClockLeapMode = "unsmeared"
	f.Config.Endpoints.FrontendControlBoundNS = f.Config.Bounds.FrontendControlBoundNS
	f.Config.Endpoints.FrontendWorkerBoundNS = f.Config.Bounds.FrontendWorkerBoundNS
}
func (f resourceFixture) clockReply(q operator.Request) (operator.Reply, bool) {
	if q.Action != "clock" {
		return operator.Reply{}, false
	}
	if q.Operation == operator.HostLeaseRead {
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := json.Marshal(operator.HostOutput{Stdout: f.Clock.Wire(string(q.Script), time.Now())})
	return operator.Reply{ID: q.ID, Transport: operator.Succeeded, ExitCode: new(int), Payload: raw}, true
}

func packedHost(t *testing.T, data []byte) json.RawMessage {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if _, err := z.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(operator.HostOutput{Stdout: base64.StdEncoding.EncodeToString(b.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestSessionBinaryUsesResourceSequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "relais-gates")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build session: %v %s", err, out)
	}
	for _, mode := range []string{"unknown-start", "unknown-start-evidence", "unknown-create", "not-near", "missing-capability", "definite-refusal", "unknown-inventory", "operator-source", "altered-server"} {
		t.Run(mode, func(t *testing.T) {
			fixture := sessionFixture(t)
			if mode == "operator-source" || mode == "altered-server" {
				fixture.Scenario.Caller.SourceCommit = strings.Repeat("c", 40)
				fixture.Scenario.Judge.SourceCommit = fixture.Scenario.Caller.SourceCommit
			}
			if mode == "altered-server" {
				fixture.Inventories["w1"] = bytes.ReplaceAll(fixture.Inventories["w1"], []byte(fixture.Config.Pins[0].Commit), []byte(fixture.Scenario.Caller.SourceCommit))
			}
			fixture.bind(t)
			config := filepath.Join(t.TempDir(), "config.json")
			raw, _ := json.Marshal(fixture.Config)
			if err := os.WriteFile(config, raw, 0600); err != nil {
				t.Fatal(err)
			}
			timingEvidence := filepath.Join(t.TempDir(), "timing.jsonl")
			peer, err := operatortransport.Start(ctx, binary, []string{"session", "-config", config, "-timing-evidence", timingEvidence})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			hello, err := peer.Receive(ctx)
			if err != nil || hello.Type != "hello" {
				t.Fatal(hello, err)
			}
			now := time.Now()
			init := operator.Init{ClockBinding: &fixture.Clock.Artifacts, Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostLeaseRead, operator.HostExecute, operator.CallerReady, operator.CallerStart, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate}}
			if mode == "missing-capability" {
				init.Capabilities = []operator.Operation{operator.HostLeaseRead, operator.HostExecute, operator.CallerReady, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate}
			}
			if err = peer.Send(ctx, operator.Frame{Type: "init", Session: "run-resources", Init: &init}); err != nil {
				t.Fatal(err)
			}
			requests := []string{}
			requestCount := 0
			traces := []operator.Trace{}
			var final *operator.Result
			var manifest operatorcaller.Manifest
			for final == nil {
				f, err := peer.Receive(ctx)
				if err != nil {
					t.Fatal(err)
				}
				switch f.Type {
				case "request":
					q := f.Request
					requestCount++
					if mode == "unknown-inventory" || mode == "altered-server" || requestCount > 12 {
						requests = append(requests, string(q.Operation)+"/"+q.Target+"/"+q.Action)
					}
					if q.ID != strconv.Itoa(requestCount) || q.RemainingMS <= 0 || q.RemainingMS > 15*time.Minute.Milliseconds() || q.DeadlineUnixMS == "" {
						t.Fatal("MUTATION: session-resource-sequence request identity/deadline", q)
					}
					reply := operator.Reply{ID: q.ID, Transport: operator.Succeeded, ExitCode: new(int)}
					switch q.Operation {
					case operator.HostExecute, operator.HostLeaseRead:
						switch q.Action {
						case "clock":
							reply, _ = fixture.clockReply(*q)
						case "inventory":
							reply.Payload = packedHost(t, fixture.Inventories[q.Target])
							if mode == "unknown-inventory" {
								reply.Transport = operator.Unknown
							}
						case "status":
							status := controlplane.Status{Ready: true, Workers: []controlplane.WorkerStatus{}, Calls: []controlplane.CallStatus{}, Takeovers: []controlplane.MoveResult{}}
							for _, worker := range []string{"w1", "w2"} {
								status.Workers = append(status.Workers, controlplane.WorkerStatus{Name: worker, Address: fixture.Config.Parameters.Workers[worker].Media})
							}
							status.Pool.Cap = 2
							envelope, _ := json.Marshal(map[string]any{"kind": "ok", "started": time.Now(), "ended": time.Now(), "data": status})
							reply.Payload = packedHost(t, envelope)
						default:
							t.Fatal("unexpected host action", q.Action)
						}
					case operator.CallerReady:
						var args operatorgates.CallerRequest
						if err := json.Unmarshal(q.Args, &args); err != nil || args.Manifest.Validate(args.Manifest.Identity, time.Now()) != nil {
							t.Fatal("invalid public manifest", err)
						}
						manifest = args.Manifest
						reply.Payload = fixture.Replies["caller.ready"]
					case operator.CallerStart:
						reply.Transport = operator.Unknown
						reply.Payload = fixture.Replies["caller.start"]
						if mode == "unknown-start-evidence" {
							reply.Transport = operator.Succeeded
							reply.Payload = fixture.Replies["caller.start.unknown"]
						}
						if mode == "not-near" || mode == "unknown-create" {
							reply.Transport = operator.Succeeded
						}
						if mode == "definite-refusal" {
							reply = operator.Reply{ID: q.ID, Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
						}
					case operator.MailboxCreate:
						var args operatorcaller.MailboxArgs
						if json.Unmarshal(q.Args, &args) != nil || !strings.HasPrefix(args.Key, manifest.Root()+"/") || len(args.Data) == 0 {
							t.Fatal("invalid public mailbox create")
						}
						reply.Payload = fixture.Replies["mailbox.create"]
						if mode == "unknown-create" {
							reply.Transport = operator.Unknown
						}
					case operator.MailboxRead:
						setup := operatorcaller.Setup{SchemaVersion: 1, RunID: manifest.RunID, PublicSHA: manifest.PublicSHA, Job: "caller-1", CallerID: manifest.CallerID, Attempt: manifest.Attempt, RuleAddedAt: time.Now(), SetupReadyAt: time.Now(), SignalRTTsNS: []int64{int64(20 * time.Millisecond)}}
						data, _ := json.Marshal(setup)
						found := true
						reply.Payload, _ = json.Marshal(operatorcaller.MailboxValue{Found: &found, Data: data})
					case operator.CallerStop:
						reply.Payload = fixture.Replies["caller.stop"]
					default:
						// Keep observing a bad mutant's requests so the named assertion, rather
						// than a broken pipe or timeout, kills it.
						reply.Transport = operator.Unknown
					}
					if err = peer.Send(ctx, operator.Frame{Type: "result", Session: "run-resources", Result: &reply}); err != nil {
						t.Fatal(err)
					}
				case "trace":
					traces = append(traces, *f.Trace)
				case "final":
					final = f.Final
				default:
					t.Fatal("unexpected frame", f.Type)
				}
			}
			_ = peer.Close()
			if err = peer.Wait(); err != nil {
				t.Fatal(err)
			}
			// The explicit evidence file is separate from protocol stdout, even
			// when the fixture stops before a media pool can be judged.
			timingRaw, err := os.ReadFile(timingEvidence)
			if err != nil || !bytes.Contains(timingRaw, []byte(`"type":"numeric-timing-end"`)) {
				t.Fatal("numeric evidence file was not retained", err)
			}
			expected := append([]string{}, fixture.ExpectedUnknownStart...)
			if mode == "unknown-create" {
				expected = append(expected, "mailbox.create/caller/mailbox.create")
			}
			if mode == "not-near" {
				expected = append(expected, "mailbox.create/caller/mailbox.create", "mailbox.create/caller/mailbox.create", "mailbox.read/caller/mailbox.read", "caller.stop/caller/caller.stop")
			}
			if mode == "missing-capability" {
				expected = []string{}
			}
			if mode == "unknown-inventory" {
				expected = expected[:1]
			}
			if mode == "altered-server" {
				expected = expected[:2]
			}
			if mode != "missing-capability" && mode != "unknown-inventory" && mode != "altered-server" {
				leases := []string{}
				for _, slot := range []string{"relay", "w1", "w2", "shared"} {
					leases = append(leases, "host.lease-read/"+slot+"/clock")
				}
				expected = append(append(append([]string{}, expected[:5]...), leases...), expected[5:]...)
			}
			if !reflect.DeepEqual(requests, expected) {
				t.Fatalf("MUTATION: session-resource-sequence mode=%s requests=%v expected=%v", mode, requests, expected)
			}
			wantLatch := strings.HasPrefix(mode, "unknown") || mode == "operator-source"
			if final.Latched != wantLatch || final.Status != operator.Inconclusive || final.Complete || len(final.Gates) != 1 || len(final.Trace) != 0 {
				t.Fatalf("MUTATION: session-resource-sequence mode=%s final=%+v", mode, final)
			}
			if wantLatch && (final.FirstError == nil || final.FirstError.Certainty != operator.Unknown || len(final.Uncertainties) == 0) {
				t.Fatalf("MUTATION: session-resource-sequence unknown became definite: %+v traces=%v", final, traces)
			}
			if mode == "missing-capability" && (len(requests) != 0 || final.Gates[0].Reason != "precondition-failed") {
				t.Fatal("MUTATION: session-resource-sequence fabricated capability", final)
			}
		})
	}
}

// Exercise the shipped session command, broker, pool and PinnedJudge invocation.
// Resource replies and the judge policy are synthetic. The real pre-fault age
// wait remains intact; this is source-binding evidence, not a media pass.
func TestSessionOperatorCallerReachesJudge(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the real pre-fault caller age window")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	work := t.TempDir()
	binary := filepath.Join(work, "relais-gates")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build session: %v %s", err, out)
	}
	fixture := sessionFixture(t)
	source := strings.Repeat("c", 40)
	fixture.Scenario.Caller.SourceCommit = source
	fixture.Scenario.Judge.SourceCommit = source
	captured := filepath.Join(work, "judge-input.json")
	// A small executable probe records the input at the external judge boundary.
	// PinnedJudge must match pool.SourceCommit to G before it invokes this probe.
	summary, _ := json.Marshal(operatorevidence.Summary{Status: operator.Inconclusive, IncludedPhases: 1})
	probe := fmt.Sprintf(`package main
import ("fmt"; "os")
func main() {
 data, err := os.ReadFile(os.Args[1]); if err != nil { panic(err) }
 if err = os.WriteFile(%q, data, 0600); err != nil { panic(err) }
 fmt.Println(%q)
}
`, captured, string(summary))
	probeSource := filepath.Join(work, "judge.go")
	if err := os.WriteFile(probeSource, []byte(probe), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.Config.JudgeBinary = filepath.Join(work, "judge")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", fixture.Config.JudgeBinary, probeSource).CombinedOutput(); err != nil {
		t.Fatalf("build judge probe: %v %s", err, out)
	}
	judgeBytes, err := os.ReadFile(fixture.Config.JudgeBinary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(judgeBytes)
	fixture.Scenario.Judge.SHA256 = hex.EncodeToString(sum[:])
	fixture.bind(t)
	config := filepath.Join(work, "config.json")
	raw, _ := json.Marshal(fixture.Config)
	if err = os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	peer, err := operatortransport.Start(ctx, binary, []string{"session", "-config", config})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	hello, err := peer.Receive(ctx)
	if err != nil || hello.Type != "hello" {
		t.Fatal(hello, err)
	}
	now := time.Now()
	init := operator.Init{ClockBinding: &fixture.Clock.Artifacts, Scenario: fixture.Scenario, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(105*time.Minute).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostLeaseRead, operator.HostExecute, operator.CallerReady, operator.CallerStart, operator.CallerStop, operator.MailboxRead, operator.MailboxCreate}}
	if err = peer.Send(ctx, operator.Frame{Type: "init", Session: "run-separated-source", Init: &init}); err != nil {
		t.Fatal(err)
	}
	var manifest operatorcaller.Manifest
	mailbox := map[string][]byte{}
	frozen, collected, stopped := false, false, false
	const callerEvidence = "synthetic caller evidence from G"
	var final *operator.Result
	for final == nil {
		frame, receiveErr := peer.Receive(ctx)
		if receiveErr != nil {
			t.Fatal(receiveErr)
		}
		if frame.Type == "final" {
			final = frame.Final
			continue
		}
		if frame.Type == "trace" {
			continue
		}
		if frame.Type != "request" {
			t.Fatal("unexpected frame", frame.Type)
		}
		q := frame.Request
		reply := operator.Reply{ID: q.ID, Transport: operator.Succeeded, ExitCode: new(int)}
		payload := func(v any) { reply.Payload, _ = json.Marshal(v) }
		switch q.Operation {
		case operator.HostExecute, operator.HostLeaseRead:
			switch q.Action {
			case "clock":
				reply, _ = fixture.clockReply(*q)
			case "inventory":
				reply.Payload = packedHost(t, fixture.Inventories[q.Target])
			case "status":
				status := controlplane.Status{Ready: true, Calls: []controlplane.CallStatus{}, Workers: []controlplane.WorkerStatus{}, Takeovers: []controlplane.MoveResult{}}
				for _, w := range []string{"w1", "w2"} {
					status.Workers = append(status.Workers, controlplane.WorkerStatus{Name: w, Address: fixture.Config.Parameters.Workers[w].Media})
				}
				status.Pool.Cap = 2
				if manifest.RunID != "" {
					status.Calls = []controlplane.CallStatus{{ID: "original", Owner: "w1", Epoch: 1}}
				}
				data, _ := json.Marshal(map[string]any{"kind": "ok", "started": time.Now(), "ended": time.Now(), "data": status})
				reply.Payload = packedHost(t, data)
			case "freeze":
				frozen = true
				// Stop the procedure after the real fault hook, before any consent waits.
				reply = operator.Reply{ID: q.ID, Transport: operator.Failed, NotSubmitted: true, RefusalCode: "deadline-before-submission"}
			case "unfreeze":
				data, _ := json.Marshal(map[string]any{"kind": "ok", "started": time.Now(), "ended": time.Now(), "data": map[string]bool{"completed": true}})
				reply.Payload = packedHost(t, data)
			default:
				t.Fatal("unexpected host action", q.Action)
			}
		case operator.CallerReady:
			var args operatorgates.CallerRequest
			if err = json.Unmarshal(q.Args, &args); err != nil || args.Manifest.PublicSHA != source || args.Manifest.Plan.PublicSHA != source {
				t.Fatal("MUTATION: session-independent-source manifest", args, err)
			}
			manifest = args.Manifest
			reply.Payload = fixture.Replies["caller.ready"]
		case operator.CallerStart:
			id := manifest.Identity
			at := time.Now()
			host := operatorcaller.Host{SchemaVersion: 1, AvailableCPUs: 2, LogicalCPUs: 2, CPUModel: "synthetic", CallerGOMAXPROCS: 2, CapturedAt: at}
			mailbox[id.Job()+"/setup-ready.json"], _ = json.Marshal(operatorcaller.Setup{SchemaVersion: 1, RunID: id.RunID, PublicSHA: source, CallerID: id.CallerID, Attempt: id.Attempt, Job: "caller-1", SignalRTTsNS: []int64{1}})
			mailbox[id.Job()+"/ready.json"], _ = json.Marshal(operatorcaller.Ready{SchemaVersion: 1, RunID: id.RunID, PublicSHA: source, CallerID: id.CallerID, Attempt: id.Attempt, Job: "caller-1", PID: 1, Sessions: []string{"original"}, Host: host})
			mailbox[id.Job()+"/finished.json"], _ = json.Marshal(map[string]any{"runId": id.RunID, "job": "caller-1", "done": true, "stopped": false, "exitCode": 0, "sessions": []string{"original"}})
			mailbox[id.Job()+"/caller.jsonl"] = []byte(callerEvidence)
			reply.Payload = fixture.Replies["caller.start"]
		case operator.MailboxCreate:
			var args operatorcaller.MailboxArgs
			if err = json.Unmarshal(q.Args, &args); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(args.Key, "/feed/") {
				var feed operatorcaller.Feed
				if err = json.Unmarshal(args.Data, &feed); err != nil {
					t.Fatal(err)
				}
				if feed.Sequence >= 0 {
					at := time.Now().Add(-time.Millisecond)
					burst := operatorcaller.Burst{StartedAt: at, FinishedAt: at}
					for range operatorcaller.ClockBurstCount {
						burst.Probes = append(burst.Probes, operatorcaller.Probe{RTTNS: 1, CallerSend: at, CallerReceive: at, ServerReceive: at, ServerSend: at})
					}
					ack := operatorcaller.Ack{RunID: manifest.RunID, Job: "caller-1", Sequence: feed.Sequence, OperationID: feed.OperationID, OK: true, Identities: feed.Payload.Identities}
					ack.ClockProbes, _ = json.Marshal(burst)
					mailbox[manifest.Job()+"/acks/"+fmt.Sprintf("%05d", feed.Sequence)+".json"], _ = json.Marshal(ack)
				}
			}
			reply.Payload = fixture.Replies["mailbox.create"]
		case operator.MailboxRead:
			var args operatorcaller.MailboxArgs
			if err = json.Unmarshal(q.Args, &args); err != nil {
				t.Fatal(err)
			}
			data, found := mailbox[args.Key]
			if strings.HasSuffix(args.Key, "/caller.jsonl") {
				collected = true
			}
			payload(operatorcaller.MailboxValue{Found: &found, Data: data})
		case operator.CallerStop:
			stopped = true
			reply.Payload = fixture.Replies["caller.stop"]
		default:
			t.Fatal("unexpected operation", q.Operation)
		}
		if err = peer.Send(ctx, operator.Frame{Type: "result", Session: "run-separated-source", Result: &reply}); err != nil {
			t.Fatal(err)
		}
	}
	_ = peer.Close()
	if err = peer.Wait(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(captured)
	var input operatorevidence.Pool
	if err != nil || json.Unmarshal(raw, &input) != nil || input.Missing || len(input.Phases) != 1 || input.Phases[0].CallerJSONL != callerEvidence || input.RunID != fixture.Config.Endpoints.RunID || !frozen || !collected || !stopped || final.Latched || final.Status != operator.Inconclusive {
		t.Fatal("MUTATION: session-independent-source judge input", string(raw), final, err)
	}
	// Replay the exact command-produced pool through the checkout's real judge.
	// The deliberately incomplete media must stay inconclusive, not become a pass.
	actualJudge := filepath.Join(work, "media-judge")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", actualJudge, "../../internal/operatorevidence/judge-src").CombinedOutput(); err != nil {
		t.Fatalf("build source judge: %v %s", err, out)
	}
	judgedRaw, err := exec.CommandContext(ctx, actualJudge, captured).Output()
	if err != nil {
		t.Fatal("real judge invocation", err)
	}
	judged, err := operatorevidence.DecodeSummary(judgedRaw)
	if err != nil || judged.Status != operator.Inconclusive || judged.DroppedPhases != 1 || len(judged.PhaseDiagnostics) != 1 || judged.PhaseDiagnostics[0].Gate != string(operator.Rollout) {
		t.Fatal("MUTATION: session-independent-source real judge", judged, err)
	}
	if fixture.Scenario.ReleaseA.SourceCommit != fixture.Config.Pins[0].Commit || fixture.Scenario.ReleaseB.SourceCommit != fixture.Config.Pins[1].Commit || source == fixture.Config.Pins[0].Commit || source == fixture.Config.Pins[1].Commit {
		t.Fatal("server releases changed to caller source")
	}
}
