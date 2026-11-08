package operatorrehearsal

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/bindingtest"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

type latencyClock struct{ start time.Time }

func (c latencyClock) Elapsed() time.Duration { return time.Since(c.start) }
func (c latencyClock) Wall() time.Time        { return time.Unix(1793930000, 0).Add(c.Elapsed()) }

func TestLeaseSlowAdapter(t *testing.T) {
	for _, batch := range []bool{false, true} {
		modes := []string{"fresh", "retry", "queued-expired"}
		if !batch {
			modes = append(modes, "upper-costs", "upper-queued-expired")
		}
		for _, mode := range modes {
			retry := mode == "retry" || strings.HasPrefix(mode, "upper-")
			t.Run(fmt.Sprintf("batch=%v/%s", batch, mode), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					clock := latencyClock{time.Now()}
					init := operator.Init{Scenario: operator.Scenario{SchemaVersion: 1, Digest: fmt.Sprintf("%064d", 1), Gates: []operator.Gate{operator.Rollout}}, ArmedAtUnixMS: strconv.FormatInt(clock.Wall().UnixMilli(), 10), EndUnixMS: strconv.FormatInt(clock.Wall().Add(time.Hour).UnixMilli(), 10), Capabilities: []operator.Operation{operator.HostExecute, operator.HostLeaseRead}}
					if batch {
						init.Capabilities = append(init.Capabilities, operator.HostLeaseReadBatch)
					}
					session, err := operatorgates.NewSession("slow", init, clock)
					require.NoError(t, err)
					f, err := bindingtest.New("slow", clock.Wall())
					require.NoError(t, err)
					proof, err := (clocks.CoverageEvidence{FrontendSlot: f.Input.FrontendSlot, ControlSlot: f.Input.ControlSlot}).WithBinding(clocks.ReceiptBinding{ReceiptSHA256: f.Selection.SHA256, EvidenceSHA256: f.Selection.Receipt.EvidenceSHA256, Bounds: f.Selection.Receipt.Bounds}, f.Selection.Receipt)
					require.NoError(t, err)
					queued := make(chan struct{})
					reads := 0
					runner := leaseRunner(func(ctx context.Context, c Command) CommandResult {
						telemetry := string(c.Input) == "queued"
						if telemetry {
							close(queued)
						} else {
							reads++
						}
						delay := 300 * time.Millisecond
						if strings.HasPrefix(mode, "upper-") {
							delay = 390 * time.Millisecond
						}
						if reads == 1 && !strings.HasPrefix(mode, "upper-") && !telemetry {
							delay = 550 * time.Millisecond
						}
						if telemetry {
							delay = 100 * time.Millisecond
							if mode == "upper-costs" {
								delay = 50 * time.Millisecond
							}
							if mode == "queued-expired" {
								if batch {
									delay = 250 * time.Millisecond
								} else {
									delay = 1700 * time.Millisecond
								}
							}
						}
						select {
						case <-ctx.Done():
							return CommandResult{Err: ctx.Err()}
						case <-time.After(delay):
						}
						if telemetry {
							return CommandResult{Code: code(0)}
						}
						o := bindingtest.Observation(clock.Wall())
						if retry && reads <= 4 {
							o.ReferenceAgeNS = int64(2100 * time.Millisecond)
							o.ReferenceUnixNS = strconv.FormatInt(clock.Wall().Add(-2100*time.Millisecond).UnixNano(), 10)
						}
						sample := clocks.Sample{MeasuredUnixNS: o.MeasuredUnixNS, Lease: &o, TrackingLog: &clocks.TrackingLog{ReadComplete: true}}
						return CommandResult{Code: code(0), Stdout: []byte(bindingtest.Pack(sample))}
					})
					var diagnostics bytes.Buffer
					adapter := &Adapter{Topology: fixtureTopology(t), Containers: fixtureContainerIDs(), Runner: runner, Diagnostics: &diagnostics}
					peer := NewLocalPeer(adapter, "slow", init)
					defer peer.Close()
					done := make(chan error, 1)
					go func() {
						_, err := session.Exchange(context.Background(), peer, operator.HostExecute, "shared", "telemetry", nil, []byte("queued"), time.Minute, func(operator.Reply) operator.Certainty { return operator.Succeeded })
						done <- err
					}()
					<-queued
					guard, err := operatorgates.NewLeaseGuard(proof, session, func(ctx context.Context, end time.Duration) (operatorgates.LeaseRead, error) {
						return session.ReadLeases(ctx, peer, end)
					}, func(_ context.Context, d time.Duration) error { time.Sleep(d); return nil })
					require.NoError(t, err)
					err = guard.Refresh(context.Background(), 4*time.Second)
					require.NoError(t, <-done)
					if mode == "queued-expired" || mode == "upper-queued-expired" {
						wantReads := 0
						if mode == "upper-queued-expired" {
							wantReads = 3
						}
						if err == nil || err.Error() != "clock-lease-expired" || session.Latched() || reads != wantReads {
							t.Fatal("MUTATION: lease-batch-service-budget", err, session.Latched(), reads)
						}
						return
					}
					if err != nil || session.Latched() || clock.Elapsed() > 4*time.Second {
						t.Fatal("MUTATION: lease-slow-adapter", err, session.Latched(), clock.Elapsed(), reads)
					}
					expected := "durationMs=550"
					if mode == "upper-costs" {
						expected = "durationMs=390"
					}
					if !strings.Contains(diagnostics.String(), "clock-lease-exec slot=") || !strings.Contains(diagnostics.String(), expected) || !strings.Contains(diagnostics.String(), "clock-lease-read operation=") {
						t.Fatal("lease duration diagnostics missing", diagnostics.String())
					}
					want := 4
					if retry {
						want = 8
					}
					require.Equal(t, want, reads)
					require.NoError(t, guard.Check())
				})
			})
		}
	}
}
