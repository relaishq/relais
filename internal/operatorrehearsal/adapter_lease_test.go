package operatorrehearsal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

type leaseRunner func(context.Context, Command) CommandResult

func (r leaseRunner) Run(ctx context.Context, c Command) CommandResult { return r(ctx, c) }

func TestLeaseTransportRefresh(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := &operatorgates.FakeClock{Now: time.Now()}
				init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(clock.Now.UnixMilli(), 10), EndUnixMS: strconv.FormatInt(clock.Now.Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute, operator.HostReadBatch, operator.HostLeaseRead}}
				if batch {
					init.Capabilities = append(init.Capabilities, operator.HostLeaseReadBatch)
				}
				session, err := operatorgates.NewSession("lease", init, clock)
				require.NoError(t, err)
				raw, err := os.ReadFile("../operatorgates/clocks/testdata/bound-receipt.json")
				require.NoError(t, err)
				var selection clocks.ClockSelection
				require.NoError(t, json.Unmarshal(raw, &selection))
				receipt := selection.Receipt
				coverage, err := (clocks.CoverageEvidence{FrontendSlot: receipt.FrontendSlot, ControlSlot: receipt.ControlSlot}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: selection.SHA256, EvidenceSHA256: receipt.EvidenceSHA256, Bounds: receipt.Bounds}, receipt)
				require.NoError(t, err)
				calls := 0
				var runnerMu sync.Mutex
				mode := "fresh"
				var cancelRead context.CancelFunc
				runner := leaseRunner(func(ctx context.Context, c Command) CommandResult {
					runnerMu.Lock()
					defer runnerMu.Unlock()
					calls++
					clock.Advance(20 * time.Millisecond)
					deadline, _ := ctx.Deadline()
					if time.Until(deadline) > 4*time.Second {
						t.Fatal("MUTATION: lease-refresh-deadline", time.Until(deadline))
					}
					require.Equal(t, host.ClockWithLease(nil, clock.Now.UnixMilli()-clock.Tick.Milliseconds(), "", nil), string(c.Input))
					if mode == "cancel" {
						cancelRead()
						<-ctx.Done()
						return CommandResult{Err: ctx.Err()}
					}
					if mode == "refuse" {
						return CommandResult{Code: code(3)}
					}
					o := &clocks.LeaseObservation{Available: true, ErrorBoundNS: 1000, MeasuredUnixNS: "1793930005100000000", ReferenceUnixNS: "1793930000000000000", ReferenceAgeNS: 5100000000, PollSeconds: "1", ResidualPPM: "0", SkewPPM: "0", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 20000000, HostElapsedNS: 20000000}
					if calls > 4 {
						o.ReferenceUnixNS = "1793930003000000000"
						o.MeasuredUnixNS = "1793930003100000000"
						o.ReferenceAgeNS = 100000000
					}
					sample := clocks.Sample{MeasuredUnixNS: o.MeasuredUnixNS, Lease: o, TrackingLog: &clocks.TrackingLog{ReadComplete: true}}
					data, e := json.Marshal(sample)
					require.NoError(t, e)
					var packed bytes.Buffer
					z := gzip.NewWriter(&packed)
					_, e = z.Write(data)
					require.NoError(t, e)
					require.NoError(t, z.Close())
					return CommandResult{Code: code(0), Stdout: []byte(base64.StdEncoding.EncodeToString(packed.Bytes()))}
				})
				adapter := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner}
				peer := NewLocalPeer(adapter, "lease", init)
				defer peer.Close()
				read := func(ctx context.Context, end time.Duration) (operatorgates.LeaseRead, error) {
					return session.ReadLeases(ctx, peer, min(end, clock.Elapsed()+3900*time.Millisecond))
				}
				guard, err := operatorgates.NewLeaseGuard(coverage, session, read, func(_ context.Context, d time.Duration) error { clock.Advance(d); return nil })
				require.NoError(t, err)
				if err = guard.Refresh(context.Background(), time.Minute); err != nil {
					t.Fatal("MUTATION: lease-refresh-transport", err)
				}
				require.Equal(t, 8, calls)
				require.NoError(t, guard.Check())
				require.False(t, session.Latched(), "MUTATION: lease-read-refusal-latched")
				clock.Advance(5 * time.Second)
				// The same reference cannot renew the expiry, even when the host reports a young age.
				start := clock.Elapsed()
				require.EqualError(t, guard.Refresh(context.Background(), time.Minute), "clock-lease-expired")
				require.LessOrEqual(t, clock.Elapsed()-start, 10*time.Second)
				require.GreaterOrEqual(t, clock.Elapsed()-start, 8*time.Second)
				mode = "refuse"
				require.Error(t, guard.Refresh(context.Background(), time.Minute))
				require.False(t, session.Latched(), "MUTATION: lease-read-refusal-latched")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cancelRead = cancel
				mode = "cancel"
				before := calls
				require.Error(t, guard.Refresh(ctx, time.Minute))
				if batch {
					// Cancellation can prevent still-unsent parallel children from starting.
					require.GreaterOrEqual(t, calls, before+1)
					require.LessOrEqual(t, calls, before+4)
				} else {
					require.Equal(t, before+1, calls)
				}
				require.True(t, session.Latched())
				require.NotEqual(t, "unavailable", session.Uncertainties()[0].Kind)
			})
		})
	}
}

func TestLeaseTransportRejectsScripts(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, mode := range []string{"script", "unknown", "action", "target", "count", "probe-path", "probe-slot"} {
			t.Run(fmt.Sprintf("%v/%s", batch, mode), func(t *testing.T) {
				req := request()
				req.Action = "clock"
				req.Operation = operator.HostLeaseRead
				req.Script = nil
				req.RemainingMS = 3900
				args := operator.HostLeaseReadArgs{Reads: []operator.HostLeaseReadInput{{Target: req.Target}}}
				if batch {
					req.Operation = operator.HostLeaseReadBatch
					req.Target = "hosts"
					args.Reads = nil
					for _, slot := range Slots {
						args.Reads = append(args.Reads, operator.HostLeaseReadInput{Target: slot})
					}
				}
				switch mode {
				case "probe-path":
					i := len(args.Reads) - 1
					if !batch {
						req.Target = "shared"
						args.Reads[i].Target = "shared"
					}
					args.Reads[i].ProbeFile = "/other"
				case "probe-slot":
					args.Reads[0].ProbeFile = "/configured"
				case "script":
					req.Script = []byte("printf non-lease")
				case "action":
					req.Action = "freeze"
				case "target":
					args.Reads[0].Target = "shared"
				case "count":
					args.Reads = append(args.Reads, args.Reads[0])
				}
				req.Args, _ = json.Marshal(args)
				if mode == "unknown" {
					req.Args = append(req.Args[:len(req.Args)-1], []byte(`,"script":"printf non-lease"}`)...)
				}
				runner := &fakeRunner{result: CommandResult{Code: code(0)}}
				adapter := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner, LeaseProbeFiles: map[string]string{"shared": "/configured"}}
				reply := throughPipe(t, adapter, req)
				if !reply.NotSubmitted || len(runner.commands) != 0 {
					t.Fatal("MUTATION: lease-only-input", mode, reply)
				}
				clock := operatorgates.NewRealClock()
				init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: strings.Repeat("a", 64), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(clock.Wall().UnixMilli(), 10), EndUnixMS: strconv.FormatInt(clock.Wall().Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{req.Operation}}
				session, err := operatorgates.NewSession("reject", init, clock)
				require.NoError(t, err)
				_, err = session.Issue(req.Operation, req.Target, req.Action, req.Args, req.Script, time.Second)
				require.Error(t, err, "MUTATION: lease-only-input")
				require.False(t, session.Latched(), "MUTATION: lease-read-refusal-latched")
			})
		}
	}
}

func TestLeaseConfiguredProbeFile(t *testing.T) {
	req := request()
	req.Operation = operator.HostLeaseRead
	req.Target = "shared"
	req.Action = "clock"
	req.Script = nil
	req.RemainingMS = 3900
	req.Args, _ = json.Marshal(operator.HostLeaseReadArgs{Reads: []operator.HostLeaseReadInput{{Target: "shared", ProbeFile: "/configured"}}})
	runner := &fakeRunner{result: CommandResult{Code: code(0)}}
	adapter := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner, LeaseProbeFiles: map[string]string{"shared": "/configured"}}
	reply := throughPipe(t, adapter, req)
	require.Equal(t, operator.Succeeded, reply.Transport)
	require.Len(t, runner.commands, 1)
	require.Equal(t, host.ClockWithLease(nil, 0, "/configured", nil), string(runner.commands[0].Input))
}
