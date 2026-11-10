package operatorrehearsal

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestHostReadCommandPriority(t *testing.T) {
	for _, operation := range []operator.Operation{operator.HostExecute, operator.HostReadBatch, operator.HostLeaseRead, operator.HostLeaseReadBatch} {
		t.Run(string(operation), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lease := operation == operator.HostLeaseRead || operation == operator.HostLeaseReadBatch
				batch := operation == operator.HostReadBatch || operation == operator.HostLeaseReadBatch
				budget := time.Minute
				if lease {
					budget = 1800 * time.Millisecond
				}
				deadline := time.Now().Add(budget)
				ids := fixtureContainerIDs()
				runner := &fakeRunner{result: CommandResult{Code: code(0)}}
				adapter := &Adapter{Topology: fixtureTopology(t), Containers: ids, Runner: leaseRunner(func(ctx context.Context, c Command) CommandResult {
					got, ok := ctx.Deadline()
					require.True(t, ok)
					require.Equal(t, deadline, got, "MUTATION: lease-command-budget")
					return runner.Run(ctx, c)
				})}
				req := operator.Request{ID: "1", Operation: operation, Action: "clock", Target: "w1", DeadlineUnixMS: strconv.FormatInt(deadline.UnixMilli(), 10), RemainingMS: budget.Milliseconds()}
				slots := []string{req.Target}
				if batch {
					req.Target = "hosts"
					slots = Slots
				}
				script := []byte("printf observation")
				if lease {
					args := operator.HostLeaseReadArgs{}
					for _, slot := range slots {
						args.Reads = append(args.Reads, operator.HostLeaseReadInput{Target: slot})
					}
					req.Args, _ = json.Marshal(args)
					script = []byte(host.ClockWithLease(nil, 0, "", nil))
				} else if batch {
					args := operator.HostReadBatchArgs{}
					for _, slot := range slots {
						args.Reads = append(args.Reads, operator.HostRead{Target: slot, Script: script})
					}
					req.Args, _ = json.Marshal(args)
				} else {
					req.Script = script
				}
				require.Equal(t, operator.Succeeded, adapter.Execute(context.Background(), req).Transport)
				require.Len(t, runner.commands, len(slots))
				cleanup := `rm -f "$f"`
				if lease {
					cleanup = `status=$?; rm -f "$f"; printf "\nclock-lease-cpu\n" >&2; times >&2; exit "$status"`
				}
				wrapper := `f=$(mktemp) || exit 125; trap '` + cleanup + `' EXIT; cat > "$f" || exit 125; timeout -s KILL ` + strconv.Itoa(int(budget/time.Second)) + `s nice -n 10 sh "$f"`
				seen := map[string]bool{}
				for _, c := range runner.commands {
					require.Equal(t, "host-step", c.Label)
					require.Equal(t, script, c.Input)
					require.Equal(t, lease, c.LeaseCPU)
					require.Len(t, c.Args, 7, "MUTATION: lease-command-priority")
					id := c.Args[3]
					require.False(t, seen[id])
					seen[id] = true
					want := []string{"docker", "exec", "-i", id, "sh", "-c", wrapper}
					require.Equal(t, want, c.Args, "MUTATION: lease-command-priority")
				}
				for _, slot := range slots {
					require.True(t, seen[ids[slot]])
				}
			})
		})
	}
}
