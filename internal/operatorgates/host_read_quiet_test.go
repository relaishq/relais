package operatorgates

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorcaller"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryFaultQuietFloorAndInflightRead(t *testing.T) {
	for _, mode := range []string{"zero-gap", "inflight-read"} {
		t.Run(mode, func(t *testing.T) {
			r, peer, _ := runtimeFixture(t)
			c := &leaseTestClock{tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, _ := leaseGuardFixture(t, c, r.Session)
			previous := peer.respond
			peer.respond = func(q operator.Request) operator.Reply {
				if q.Action == "move" {
					t.Fatal("MUTATION: lease-fault-floor-inflight: refused fault was sent")
				}
				return previous(q)
			}
			hooked := false
			r.BeforeMutation = func(context.Context) error {
				hooked = true
				g.mu.Lock()
				g.reading = true
				g.mu.Unlock()
				return nil
			}
			want := "clock-lease-expired"
			if mode == "zero-gap" {
				r.FaultQuietGap, want = 0, "clock-lease-missing"
			}
			_, err := r.StepWithLeases(context.Background(), "move", "shared", time.Minute, ProcessExtra{}, LeaseStep{Guard: g})
			require.EqualError(t, err, want, "MUTATION: lease-fault-floor-inflight")
			require.Equal(t, mode == "inflight-read", hooked, "MUTATION: lease-fault-floor-inflight")
			require.False(t, r.Session.Latched())
		})
	}
}

func TestHostReadCompletionKindsAndDiagnostics(t *testing.T) {
	for _, action := range []string{"inventory", "clock", "status", "worker-status", "move"} {
		t.Run(action, func(t *testing.T) {
			s, _ := fixtureSession(t)
			c := &leaseTestClock{tick: time.Second}
			s.clock, s.Budget.clock = c, c
			var output bytes.Buffer
			recorder := TimingRecorder{Writer: &output}
			s.hostReadObserved = recorder.RecordHostRead
			peer := &observationPeer{respond: func(q operator.Request) operator.Reply {
				c.advance(400 * time.Millisecond)
				return reply(q, operator.Succeeded)
			}}
			certainty, err := s.Exchange(context.Background(), peer, operator.HostExecute, "shared", action, nil, []byte("printf fixture"), time.Minute, func(operator.Reply) operator.Certainty { c.advance(100 * time.Millisecond); return operator.Succeeded })
			require.NoError(t, err)
			require.Equal(t, operator.Succeeded, certainty)
			if action == "move" {
				require.Zero(t, s.hostReadCompleted())
				require.Empty(t, output.String())
				return
			}
			require.Equal(t, 1500*time.Millisecond, s.hostReadCompleted(), "MUTATION: host-read-completion")
			var got struct {
				Type string `json:"type"`
				HostReadTiming
			}
			require.NoError(t, json.Unmarshal(output.Bytes(), &got), "MUTATION: host-read-diagnostic")
			require.Equal(t, "host-read", got.Type, "MUTATION: host-read-diagnostic")
			require.Equal(t, int64(time.Second), got.StartedNS, "MUTATION: host-read-diagnostic")
			require.Equal(t, int64(1500*time.Millisecond), got.CompletedNS, "MUTATION: host-read-diagnostic")
			require.True(t, time.UnixMilli(1000000).Add(time.Second).Equal(got.StartedAt), "MUTATION: host-read-diagnostic")
			require.True(t, c.Wall().Equal(got.CompletedAt), "MUTATION: host-read-diagnostic")
			require.Equal(t, action, got.Action)
			require.NoError(t, recorder.Err())
		})
	}
}

func TestNoHostReadCompletionWithoutSubmission(t *testing.T) {
	s, _ := fixtureSession(t)
	c := &leaseTestClock{tick: time.Second}
	s.clock, s.Budget.clock = c, c
	peer := &observationPeer{respond: func(q operator.Request) operator.Reply {
		t.Fatal("short-budget read submitted")
		return reply(q, operator.Succeeded)
	}}
	_, err := s.Exchange(context.Background(), peer, operator.HostExecute, "shared", "inventory", nil, []byte("printf fixture"), 2*time.Second, func(operator.Reply) operator.Certainty { return operator.Failed })
	require.NoError(t, err)
	require.Zero(t, s.hostReadCompleted())
}

func TestHostReadDiagnosticRedactsLabels(t *testing.T) {
	s, _ := fixtureSession(t)
	s.capabilities[operator.HostReadBatch] = true
	var output bytes.Buffer
	recorder := TimingRecorder{Writer: &output}
	s.hostReadObserved = recorder.RecordHostRead
	peer := &observationPeer{respond: func(q operator.Request) operator.Reply { return reply(q, operator.Succeeded) }}
	_, err := s.Exchange(context.Background(), peer, operator.HostReadBatch, "private-target", "private-action", nil, nil, time.Minute, func(operator.Reply) operator.Certainty { return operator.Succeeded })
	require.NoError(t, err)
	require.NotEmpty(t, output.String())
	require.NotContains(t, output.String(), "private-", "MUTATION: host-read-private-label")
}

func TestLeaseAcceptanceWithoutReadPreservesCompletion(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, _ := leaseGuardFixture(t, c)
	before := g.session.hostReadCompleted()
	c.advance(time.Second)
	read, err := g.readAttempt(context.Background(), c.Elapsed()+time.Millisecond)
	require.EqualError(t, err, "clock-lease-expired")
	require.Error(t, g.accept(read))
	require.Equal(t, before, g.session.hostReadCompleted(), "MUTATION: lease-no-read-completion")
}

func TestQuietReacquisitionWithoutSubmission(t *testing.T) {
	c := &leaseTestClock{tick: time.Second}
	g, read := leaseGuardFixture(t, c)
	for _, sample := range read.Evidence.Clocks {
		sample.Lease.ReferenceAgeNS = 4026000000
		sample.Lease.MeasuredUnixNS = "1793930004026000000"
	}
	require.NoError(t, g.accept(read))
	before := g.session.hostReadCompleted()
	g.read = func(context.Context, time.Duration) (LeaseRead, error) {
		c.advance(200 * time.Millisecond)
		return LeaseRead{}, fault("clock-lease-expired")
	}
	var timing FaultQuietTiming
	err := g.prepareFault(context.Background(), time.Minute, 1100*time.Millisecond, &timing)
	require.EqualError(t, err, "clock-lease-expired")
	require.Zero(t, timing.Reacquires, "MUTATION: lease-unsent-reacquire")
	require.Equal(t, before, g.session.hostReadCompleted(), "MUTATION: lease-no-read-completion")
}

func TestCallerQuietGapUsesTightestProbe(t *testing.T) {
	base := time.Unix(1793930000, 0)
	burst := operatorcaller.Burst{Probes: []operatorcaller.Probe{
		{CallerSend: base, CallerReceive: base.Add(2 * time.Millisecond), ServerReceive: base.Add(time.Millisecond), ServerSend: base.Add(time.Millisecond)},
		{CallerSend: base.Add(time.Second), CallerReceive: base.Add(1010 * time.Millisecond), ServerReceive: base.Add(1005 * time.Millisecond), ServerSend: base.Add(1005 * time.Millisecond)},
	}}
	gap, err := callerQuietGap(burst, 20*time.Minute, clocks.Bounds{FrontendControlBoundNS: 120000, FrontendWorkerBoundNS: 170000})
	require.NoError(t, err, "MUTATION: lease-fault-probe-min")
	require.Equal(t, 1002340*time.Microsecond, gap, "MUTATION: lease-fault-probe-min")
}

func TestOrdinaryFaultWaitsAfterCachedLeaseInventoryBatch(t *testing.T) {
	r, peer, _ := runtimeFixture(t)
	c := &leaseTestClock{tick: time.Second}
	r.Session.clock, r.Session.Budget.clock = c, c
	r.Session.capabilities[operator.HostReadBatch] = true
	r.FaultQuietGap = 1100 * time.Millisecond
	g, _ := leaseGuardFixture(t, c, r.Session)
	var diagnostics bytes.Buffer
	recorder := TimingRecorder{Writer: &diagnostics}
	r.Session.hostReadObserved = recorder.RecordHostRead
	g.read = func(context.Context, time.Duration) (LeaseRead, error) {
		t.Fatal("cached lease was read")
		return LeaseRead{}, nil
	}
	previous := peer.respond
	batched := false
	peer.respond = func(q operator.Request) operator.Reply {
		if q.Operation != operator.HostReadBatch {
			return previous(q)
		}
		require.Equal(t, "inventory", q.Action)
		batched = true
		c.advance(500 * time.Millisecond)
		batch := operator.HostReadBatchOutput{}
		for _, slot := range inventory.Slots {
			child := previous(operator.Request{Action: "inventory", Target: slot})
			child.ID = slot
			batch.Replies = append(batch.Replies, child)
		}
		return operator.Reply{Transport: operator.Succeeded, ExitCode: new(0), Payload: oracleJSON(batch)}
	}
	r.BeforeMutation = func(context.Context) error {
		require.True(t, batched)
		require.GreaterOrEqual(t, c.Elapsed(), 2600*time.Millisecond, "MUTATION: host-read-quiet-gap")
		return fault("stop-after-host-read-gap")
	}
	_, err := r.StepWithLeases(context.Background(), "move", "shared", time.Minute, ProcessExtra{}, LeaseStep{Guard: g})
	require.EqualError(t, err, "stop-after-host-read-gap")
	var timing HostReadTiming
	require.NoError(t, json.Unmarshal(diagnostics.Bytes(), &timing), "MUTATION: host-read-diagnostic")
	require.Equal(t, operator.HostReadBatch, timing.Operation, "MUTATION: host-read-diagnostic")
	require.Equal(t, int64(time.Second), timing.StartedNS, "MUTATION: host-read-diagnostic")
	require.Equal(t, int64(1500*time.Millisecond), timing.CompletedNS, "MUTATION: host-read-diagnostic")
}
