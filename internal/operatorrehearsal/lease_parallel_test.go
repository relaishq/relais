package operatorrehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestLeaseBatchConcurrentCertainty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan string, 4)
		release := make(chan struct{})
		ids := fixtureContainerIDs()
		defer close(release)
		runner := leaseRunner(func(ctx context.Context, c Command) CommandResult {
			slot := ""
			for k, v := range ids {
				if c.Args[3] == v {
					slot = k
				}
			}
			entered <- slot
			<-release
			if slot == "w1" {
				return CommandResult{Code: code(3)}
			}
			if slot == "w2" {
				return CommandResult{Err: context.DeadlineExceeded}
			}
			return CommandResult{Code: code(0), Stdout: []byte("observation")}
		})
		var diagnostics bytes.Buffer
		adapter := &Adapter{Topology: fixtureTopology(t), Containers: ids, Runner: runner, Diagnostics: &diagnostics}
		req := batchRequest()
		req.Operation = operator.HostLeaseReadBatch
		req.Action = "clock"
		args := operator.HostLeaseReadArgs{}
		for _, slot := range Slots {
			args.Reads = append(args.Reads, operator.HostLeaseReadInput{Target: slot})
		}
		req.Args, _ = json.Marshal(args)
		done := make(chan operator.Reply, 1)
		go func() { done <- adapter.Execute(context.Background(), req) }()
		synctest.Wait()
		if len(entered) != 4 {
			t.Fatal("MUTATION: lease-batch-parallel", len(entered))
		}
		for range 4 {
			release <- struct{}{}
		}
		synctest.Wait()
		reply := <-done
		var out operator.HostReadBatchOutput
		require.NoError(t, json.Unmarshal(reply.Payload, &out))
		want := []operator.Certainty{operator.Succeeded, operator.Failed, operator.Unknown, operator.Succeeded}
		for i, child := range out.Replies {
			if child.ID != Slots[i] || child.Transport != want[i] {
				t.Fatal("MUTATION: lease-child-certainty", child)
			}
		}
		// Drive the real session classifier: the definite failed child must not hide
		// the unknown one, even though all parallel children have joined.
		clock := operatorgates.NewRealClock()
		init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: string(bytes.Repeat([]byte("a"), 64)), Gates: []operator.Gate{operator.Rollout}}, Capabilities: []operator.Operation{operator.HostLeaseReadBatch}}
		init.ArmedAtUnixMS = strconv.FormatInt(clock.Wall().UnixMilli(), 10)
		init.EndUnixMS = strconv.FormatInt(clock.Wall().Add(time.Hour).UnixMilli(), 10)
		session, err := operatorgates.NewSession("idle", init, clock)
		require.NoError(t, err)
		peer := &idlePeer{respond: func(operator.Request) operator.Reply { return reply }}
		_, err = session.ReadLeases(context.Background(), peer, time.Minute)
		if err == nil || !session.Latched() || len(session.Uncertainties()) == 0 {
			t.Fatal("MUTATION: lease-child-certainty", err)
		}
	})
}
