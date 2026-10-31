package operatorrehearsal

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

type batchRunner struct {
	mu      sync.Mutex
	started int
	release chan struct{}
}

func (r *batchRunner) Run(ctx context.Context, _ Command) CommandResult {
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r.mu.Lock()
	r.started++
	if r.started == 4 {
		close(r.release)
	}
	r.mu.Unlock()
	select {
	case <-r.release:
		return CommandResult{Code: code(0), Stdout: []byte("observation")}
	case <-wait.Done():
		return CommandResult{Err: wait.Err()}
	}
}
func batchRequest() operator.Request {
	req := request()
	req.Operation = operator.HostReadBatch
	req.Target = "hosts"
	req.Action = "inventory"
	req.Script = nil
	args := operator.HostReadBatchArgs{}
	for _, slot := range []string{"relay", "w1", "w2", "shared"} {
		args.Reads = append(args.Reads, operator.HostRead{Target: slot, Script: []byte("printf observation")})
	}
	req.Args, _ = json.Marshal(args)
	return req
}
func TestReadBatchRunsFourHostsTogetherAndJoins(t *testing.T) {
	runner := &batchRunner{release: make(chan struct{})}
	a := &Adapter{Topology: fixtureTopology(t), Runner: runner, Containers: fixtureContainerIDs()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reply := a.Execute(ctx, batchRequest())
	var out operator.HostReadBatchOutput
	if reply.Transport != operator.Succeeded || json.Unmarshal(reply.Payload, &out) != nil || len(out.Replies) != 4 {
		t.Fatalf("MUTATION: batch-parallel-join reply=%+v", reply)
	}
	for i, child := range out.Replies {
		if child.ID != []string{"relay", "w1", "w2", "shared"}[i] || child.Transport != operator.Succeeded || child.ExitCode == nil || *child.ExitCode != 0 {
			t.Fatalf("MUTATION: batch-parallel-join child=%+v", child)
		}
	}
	if again := a.Execute(ctx, batchRequest()); again.Transport != operator.Unknown {
		t.Fatal("duplicate batch replayed")
	}
}
func TestReadBatchValidatesAllChildrenBeforeSubmission(t *testing.T) {
	for _, kind := range []string{"action", "target", "duplicate", "missing", "script", "floor"} {
		t.Run(kind, func(t *testing.T) {
			runner := &fakeRunner{}
			a := &Adapter{Topology: fixtureTopology(t), Runner: runner, Containers: fixtureContainerIDs()}
			req := batchRequest()
			var args operator.HostReadBatchArgs
			_ = json.Unmarshal(req.Args, &args)
			switch kind {
			case "action":
				req.Action = "freeze"
			case "target":
				args.Reads[3].Target = "caller"
			case "duplicate":
				args.Reads[3].Target = "relay"
			case "missing":
				args.Reads = args.Reads[:3]
			case "script":
				args.Reads[3].Script = nil
			case "floor":
				req.RemainingMS = 29999
			}
			req.Args, _ = json.Marshal(args)
			reply := a.Execute(context.Background(), req)
			if !reply.NotSubmitted || reply.Transport != operator.Failed || len(runner.commands) != 0 {
				t.Fatal("MUTATION: batch-validation-before-submit", reply)
			}
		})
	}
}

func TestReadBatchSurvivesProtocolPipe(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	runner := &batchRunner{release: make(chan struct{})}
	adapter := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner}
	done := make(chan error, 1)
	go func() { _, err := adapter.Serve(ctx, peer, "batch-pipe", operator.Init{}); done <- err }()
	defer func() { _ = engine.Close(); _ = peer.Close(); <-done }()
	if err := engine.Send(ctx, operator.Frame{Type: "hello", Hello: &operator.Hello{Protocol: operator.ProtocolVersion}}); err != nil {
		t.Fatal(err)
	}
	if f, err := engine.Receive(ctx); err != nil || f.Type != "init" {
		t.Fatal("init", err)
	}
	req := batchRequest()
	if err := engine.Send(ctx, operator.Frame{Type: "request", Session: "batch-pipe", Request: &req}); err != nil {
		t.Fatal(err)
	}
	f, err := engine.Receive(ctx)
	var out operator.HostReadBatchOutput
	if err != nil || f.Result == nil || f.Result.Transport != operator.Succeeded || json.Unmarshal(f.Result.Payload, &out) != nil || len(out.Replies) != 4 {
		t.Fatalf("batch pipe lost evidence: %v %+v", err, f)
	}
}

func TestBatchChildRechecksRoundedHostFloor(t *testing.T) {
	runner := &fakeRunner{}
	a := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner}
	req := request()
	req.Operation = operator.HostReadBatch
	ctx, cancel := context.WithTimeout(context.Background(), operator.HostSubmissionMinimum-time.Microsecond)
	defer cancel()
	reply := a.executeHost(ctx, req)
	if !reply.NotSubmitted || reply.RefusalCode != "deadline-before-submission" || len(runner.commands) != 0 {
		t.Fatal("MUTATION: batch-child-floor", reply)
	}
}
