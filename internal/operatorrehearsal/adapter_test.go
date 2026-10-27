package operatorrehearsal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

type fakeRunner struct {
	mu       sync.Mutex
	commands []Command
	result   CommandResult
}

func (r *fakeRunner) Run(_ context.Context, c Command) CommandResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, c)
	return r.result
}
func code(n int) *int { return &n }
func request() operator.Request {
	return operator.Request{ID: "1", Operation: operator.HostExecute, Target: "w1", Action: "worker-status", Script: []byte("printf observation"), DeadlineUnixMS: strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10), RemainingMS: 60000}
}

// Uses actual P1 pipes, framing, hello/init and request/reply flow. No socket,
// container, external process, network or substitute codec participates.
func throughPipe(t *testing.T, a *Adapter, r operator.Request) operator.Reply {
	t.Helper()
	toAdapter, fromEngine, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	toEngine, fromAdapter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	engine := operatortransport.NewPipe(toEngine, fromEngine)
	peer := operatortransport.NewPipe(toAdapter, fromAdapter)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, x := a.Serve(ctx, peer, "fixture", operator.Init{}); done <- x }()
	t.Cleanup(func() { _ = engine.Close(); _ = peer.Close(); <-done })
	hello := operator.Hello{Protocol: operator.ProtocolVersion}
	if err = engine.Send(ctx, operator.Frame{Type: "hello", Hello: &hello}); err != nil {
		t.Fatal(err)
	}
	init, err := engine.Receive(ctx)
	if err != nil || init.Type != "init" {
		t.Fatalf("init: %v", err)
	}
	if err = engine.Send(ctx, operator.Frame{Type: "request", Session: "fixture", Request: &r}); err != nil {
		t.Fatal(err)
	}
	f, err := engine.Receive(ctx)
	if err != nil || f.Type != "result" {
		t.Fatalf("result: %v", err)
	}
	return *f.Result
}

func TestLocalAndPipeAdapterOutcomeContracts(t *testing.T) {
	topology := fixtureTopology(t)
	for _, row := range []struct {
		name   string
		result CommandResult
		want   operator.Certainty
	}{
		{"completed", CommandResult{Code: code(0), Stdout: []byte(`{"kind":"completed"}`)}, operator.Succeeded},
		{"failed", CommandResult{Code: code(3), Stdout: []byte(`{"kind":"refused"}`)}, operator.Failed},
		{"lost", CommandResult{Err: errors.New("lost")}, operator.Unknown},
		{"timeout", CommandResult{Code: code(137)}, operator.Unknown},
		{"deadline", CommandResult{Code: code(124)}, operator.Unknown},
		{"signal", CommandResult{Code: code(-1)}, operator.Unknown},
		{"stdout-limit", CommandResult{Code: code(0), Stdout: bytes.Repeat([]byte("x"), operator.MaxHostOutputBytes+1)}, operator.Unknown},
		{"stderr-limit", CommandResult{Code: code(0), Truncated: true}, operator.Unknown},
		{"encoded-limit", CommandResult{Code: code(0), Stdout: bytes.Repeat([]byte{0}, operator.MaxHostOutputBytes), Stderr: bytes.Repeat([]byte{0}, operator.MaxHostOutputBytes)}, operator.Unknown},
		{"invalid-utf8", CommandResult{Code: code(0), Stdout: []byte{0xff}}, operator.Unknown},
	} {
		t.Run(row.name, func(t *testing.T) {
			directRunner := &fakeRunner{result: row.result}
			direct := &Adapter{Topology: topology, Containers: fixtureContainerIDs(), Runner: directRunner}
			wireRunner := &fakeRunner{result: row.result}
			wire := &Adapter{Topology: topology, Containers: fixtureContainerIDs(), Runner: wireRunner}
			r := request()
			local := direct.Execute(context.Background(), r)
			remote := throughPipe(t, wire, r)
			if local.Transport != row.want || !reflect.DeepEqual(local, remote) {
				t.Fatalf("contract mismatch local=%+v pipe=%+v", local, remote)
			}
			for _, runner := range []*fakeRunner{directRunner, wireRunner} {
				if len(runner.commands) != 1 {
					t.Fatal("submission count")
				}
				c := runner.commands[0]
				if !reflect.DeepEqual(c.Input, r.Script) || !strings.Contains(c.Args[len(c.Args)-1], "nice -n 10") || !strings.Contains(c.Args[len(c.Args)-1], "timeout -s KILL") {
					t.Fatal("wrong command generation")
				}
			}
		})
	}
}

func TestAdapterRefusesBeforeSubmissionAndNeverReplays(t *testing.T) {
	topology := fixtureTopology(t)
	runner := &fakeRunner{result: CommandResult{Code: code(0)}}
	a := &Adapter{Topology: topology, Containers: fixtureContainerIDs(), Runner: runner}
	for i, change := range []func(*operator.Request){func(r *operator.Request) { r.Target = "outside" }, func(r *operator.Request) { r.Script = bytes.Repeat([]byte("x"), operator.MaxScriptBytes+1) }, func(r *operator.Request) { r.DeadlineUnixMS = "0" }, func(r *operator.Request) { r.Operation = operator.StoreFailover }, func(r *operator.Request) { r.RemainingMS = 0 }} {
		r := request()
		r.ID = strconv.Itoa(i + 1)
		change(&r)
		reply := a.Execute(context.Background(), r)
		if reply.Transport != operator.Failed || !reply.NotSubmitted || reply.RefusalCode == "" {
			t.Fatal(reply)
		}
	}
	if len(runner.commands) != 0 {
		t.Fatal("refusal submitted an effect")
	}
	r := request()
	r.ID = "9"
	_ = a.Execute(context.Background(), r)
	if reply := a.Execute(context.Background(), r); reply.Transport != operator.Unknown || reply.NotSubmitted {
		t.Fatal("duplicate lost uncertainty")
	}
	if len(runner.commands) != 1 {
		t.Fatal("duplicate resubmitted")
	}
}

func TestFailedEffectStaysUnknownInSession(t *testing.T) {
	topology := fixtureTopology(t)
	for _, pipe := range []bool{false, true} {
		now := time.Now()
		init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute}}
		s, err := operatorgates.NewSession("fixture", init, operatorgates.NewRealClock())
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Issue(operator.HostExecute, "w1", "worker-stop", nil, []byte("effect"), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		a := &Adapter{Topology: topology, Containers: fixtureContainerIDs(), Runner: &fakeRunner{result: CommandResult{Code: code(3)}}}
		var reply operator.Reply
		if pipe {
			reply = throughPipe(t, a, r)
		} else {
			reply = a.Execute(context.Background(), r)
		}
		certainty, err := s.Accept("fixture", reply, operator.Failed)
		if err != nil || certainty != operator.Unknown || !s.Latched() {
			t.Fatalf("failed effect cleared uncertainty: %v %v", certainty, err)
		}
		if _, err = s.Issue(operator.HostExecute, "w1", "worker-start", nil, []byte("restore"), time.Minute); err == nil {
			t.Fatal("restore admitted after unknown")
		}
	}
}

func TestFailedStepsRedactBothStreams(t *testing.T) {
	topology := fixtureTopology(t)
	secret := topology.Secrets["WorkerPassword"]
	runner := &fakeRunner{result: CommandResult{Code: code(2), Stdout: []byte("out " + secret), Stderr: []byte("error " + secret)}}
	var log bytes.Buffer
	a := &Adapter{Topology: topology, Containers: fixtureContainerIDs(), Runner: runner, Diagnostics: &log}
	reply := a.Execute(context.Background(), request())
	if strings.Contains(log.String(), secret) || bytes.Contains(reply.Payload, []byte(secret)) {
		t.Fatal("secret leaked")
	}
	if !strings.Contains(log.String(), "stdout:\nout [secret]") || !strings.Contains(log.String(), "stderr:\nerror [secret]") {
		t.Fatal("missing redacted failure streams")
	}
	var output operator.HostOutput
	if json.Unmarshal(reply.Payload, &output) != nil || output.Stdout != "out [secret]" {
		t.Fatal("wrong host envelope")
	}
}

func TestLocalRunnerStripsCredentialsAndBoundsOutput(t *testing.T) {
	t.Setenv("REHEARSAL_GITHUB_TOKEN", "job-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud-secret")
	runner := LocalRunner{}
	result := runner.Run(context.Background(), Command{Args: []string{"/bin/sh", "-c", `test -z "${REHEARSAL_GITHUB_TOKEN:-}" && test -z "${AWS_SECRET_ACCESS_KEY:-}"`}})
	if result.Code == nil || *result.Code != 0 || result.Err != nil {
		t.Fatal("credentials inherited")
	}
	var output boundedOutput
	n, err := output.Write(bytes.Repeat([]byte("x"), operator.MaxHostOutputBytes+100))
	if err != nil || n != operator.MaxHostOutputBytes+100 || !output.truncated || len(output.data) != 0 {
		t.Fatal("output not bounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result = runner.Run(ctx, Command{Args: []string{"/bin/true"}}); result.Code != nil || result.Err == nil {
		t.Fatal("cancel became success")
	}
}

var _ io.Writer = (*boundedOutput)(nil)

func TestSessionUsesSameAdapterThroughLocalPeerAndPipes(t *testing.T) {
	topology := fixtureTopology(t)
	for _, outcome := range []struct {
		name string
		code int
		want operator.Verdict
	}{{"complete", 0, operator.Passed}, {"failed-effect", 3, operator.Inconclusive}, {"timeout", 137, operator.Inconclusive}} {
		t.Run(outcome.name, func(t *testing.T) {
			for _, wire := range []bool{false, true} {
				now := time.Now()
				init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("b", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute}}
				a := &Adapter{Topology: topology, Containers: fixtureContainerIDs(), Runner: &fakeRunner{result: CommandResult{Code: code(outcome.code), Stdout: []byte(`{"kind":"completed"}`)}}}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				run := func(ctx context.Context, s *operatorgates.Session, p operatorgates.Peer, g operator.Gate) operator.GateResult {
					certainty, err := s.Exchange(ctx, p, operator.HostExecute, "w1", "worker-stop", nil, []byte("effect"), time.Minute, func(r operator.Reply) operator.Certainty { return r.Transport })
					status := operator.Passed
					if err != nil || certainty != operator.Succeeded {
						status = operator.Inconclusive
					}
					return operator.GateResult{Gate: g, Status: status}
				}
				var final *operator.Result
				if wire {
					toAdapter, fromEngine, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					toEngine, fromAdapter, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					engine := operatortransport.NewPipe(toEngine, fromEngine)
					adapter := operatortransport.NewPipe(toAdapter, fromAdapter)
					done := make(chan error, 1)
					go func() {
						done <- operatorgates.Serve(ctx, engine, operator.Hello{Protocol: operator.ProtocolVersion}, run)
					}()
					final, err = a.Serve(ctx, adapter, "fixture", init)
					if err != nil {
						t.Fatal(err)
					}
					if err = <-done; err != nil {
						t.Fatal(err)
					}
				} else {
					peer := NewLocalPeer(a, "fixture", init)
					if err := operatorgates.Serve(ctx, peer, operator.Hello{Protocol: operator.ProtocolVersion}, run); err != nil {
						t.Fatal(err)
					}
					final = peer.Final()
				}
				if final == nil || final.Status != outcome.want || final.Latched != (outcome.code != 0) {
					t.Fatalf("wire=%v final=%+v", wire, final)
				}
			}
		})
	}
}

type p2ClockRunner struct {
	t      *testing.T
	sample clocks.Sample
	calls  int
}

func (r *p2ClockRunner) Run(_ context.Context, c Command) CommandResult {
	r.t.Helper()
	if string(c.Input) != host.Clock() {
		r.t.Fatal("adapter did not execute P2's clock script")
	}
	r.calls++
	raw, err := json.Marshal(r.sample)
	if err != nil {
		r.t.Fatal(err)
	}
	var packed bytes.Buffer
	zip := gzip.NewWriter(&packed)
	if _, err = zip.Write(raw); err != nil {
		r.t.Fatal(err)
	}
	if err = zip.Close(); err != nil {
		r.t.Fatal(err)
	}
	return CommandResult{Code: code(0), Stdout: []byte(base64.StdEncoding.EncodeToString(packed.Bytes()))}
}
func TestP2ClockObservationsUseSharedHostOutput(t *testing.T) {
	now := time.Now()
	init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute}}
	runner := &p2ClockRunner{t: t, sample: clocks.Sample{ErrorBoundNS: 1, RateErrorPPM: 1, MeasuredUnixNS: strconv.FormatInt(now.UnixNano(), 10)}}
	adapter := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner}
	peer := NewLocalPeer(adapter, "fixture", init)
	run := func(ctx context.Context, s *operatorgates.Session, p operatorgates.Peer, g operator.Gate) operator.GateResult {
		evidence, err := s.FreshClocks(ctx, p, clocks.Bounds{FrontendControlBoundNS: 2, FrontendWorkerBoundNS: 2, DriftPPM: 2}, time.Minute)
		if err != nil || evidence.Control != 2 || len(evidence.Clocks) != 4 {
			t.Fatalf("P2 clock evidence failed: %v", err)
		}
		return operator.GateResult{Gate: g, Status: operator.Passed}
	}
	if err := operatorgates.Serve(context.Background(), peer, operator.Hello{Protocol: operator.ProtocolVersion}, run); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 4 || peer.Final() == nil || peer.Final().Status != operator.Passed {
		t.Fatal("P2 clock observations did not complete through the real adapter")
	}
}
