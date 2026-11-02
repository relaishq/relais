package operatorrehearsal

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatortransport"
	"github.com/relaishq/relais/pkg/operator"
)

// Each child represents work on the same service CPU mask. The test controls
// completion so concurrent submissions cannot be hidden by fast fake commands.
type pacedBatchRunner struct {
	entered chan Command
	release chan CommandResult
}

func (r *pacedBatchRunner) Run(_ context.Context, command Command) CommandResult {
	r.entered <- command
	return <-r.release
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
func TestReadBatchSerializesSharedHostWorkAndJoins(t *testing.T) {
	for _, action := range []string{"inventory", "clock"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner := &pacedBatchRunner{entered: make(chan Command, len(Slots)), release: make(chan CommandResult)}
				defer close(runner.release)
				ids := fixtureContainerIDs()
				a := &Adapter{Topology: fixtureTopology(t), Runner: runner, Containers: ids}
				req := batchRequest()
				req.Action = action
				done := make(chan operator.Reply, 1)
				go func() { done <- a.Execute(context.Background(), req) }()
				for i, slot := range Slots {
					synctest.Wait()
					if len(runner.entered) != 1 {
						t.Fatal("MUTATION: batch-shared-host-serialized", "overlapping service probes", len(runner.entered))
					}
					command := <-runner.entered
					if command.Args[3] != ids[slot] {
						t.Fatal("MUTATION: batch-shared-host-serialized", "child order differs")
					}
					select {
					case <-done:
						t.Fatal("MUTATION: batch-shared-host-serialized", "reply before all children completed")
					default:
					}
					result := CommandResult{Code: code(0), Stdout: []byte("observation")}
					if i == 1 {
						result.Code = code(3)
					}
					if i == 2 {
						result.Err = context.DeadlineExceeded
					}
					runner.release <- result
				}
				synctest.Wait()
				reply := <-done
				var out operator.HostReadBatchOutput
				if reply.Transport != operator.Succeeded || reply.ExitCode == nil || *reply.ExitCode != 0 || json.Unmarshal(reply.Payload, &out) != nil || len(out.Replies) != len(Slots) {
					t.Fatalf("MUTATION: batch-shared-host-serialized reply=%+v", reply)
				}
				for i, child := range out.Replies {
					expected := []operator.Certainty{operator.Succeeded, operator.Failed, operator.Unknown, operator.Succeeded}[i]
					if child.ID != Slots[i] || child.Transport != expected {
						t.Fatalf("MUTATION: batch-shared-host-serialized child=%+v", child)
					}
				}
				if again := a.Execute(context.Background(), req); again.Transport != operator.Unknown {
					t.Fatal("duplicate batch replayed")
				}
			})
		})
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
	runner := &fakeRunner{result: CommandResult{Code: code(0), Stdout: []byte("observation")}}
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
