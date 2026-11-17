package operatorrehearsal

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates"
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/stretchr/testify/require"
)

// Hosted rehearsal uses the same poll as its chrony configuration, while
// retaining the measured local-read reference latency.
func TestRehearsalClockTiming(t *testing.T) {
	r := &rehearsal{topology: fixtureTopology(t), report: &Report{}, clock: &operatorgates.FakeClock{Now: time.Unix(1793930000, 0)}}
	c := r.clockConfig("/judge")
	require.Equal(t, operatorgates.ClockTiming{PollSeconds: 8, HostReadLatencyMS: 400}, c.ClockTiming)
	require.NoError(t, c.CheckClockTiming())
	a := r.clockAdmission("relay", "shared", nil)
	require.Equal(t, 8*time.Second, a.PollPeriod)
	require.Equal(t, 400*time.Millisecond, a.HostReadLatency)
	require.Equal(t, int64(240), a.Policy.WindowSeconds)
	require.EqualError(t, CheckClockAdmissionPolicy(a.Policy), "clock-select-policy", "paid admission still requires 120 s")
	require.Equal(t, r.clock.Wall().Add(180*time.Minute), a.HelpersUntil)
	p := a.ports()
	require.Equal(t, a.PollPeriod, p.poll)
	require.Equal(t, a.HostReadLatency, p.latency)
}

// Exercise the hosted inputs against real selection and host readers at every
// phase of an eight-second tracking row, with delayed reads and helper setup.
func TestRehearsalPollAdmission(t *testing.T) {
	r := &rehearsal{clock: &operatorgates.FakeClock{Now: time.Unix(1793930000, 0)}}
	a := r.clockAdmission("relay", "shared", nil)
	for phase := range int64(8) {
		t.Run(strconv.FormatInt(phase, 10), func(t *testing.T) {
			t.Parallel()
			run := runClockAdmission(t, a.HostReadLatency, a.PollPeriod, a.HostReadLatency, phase, func(p *clockAdmissionPorts) {
				p.converge, p.policy = true, a.Policy
			})
			require.Empty(t, run.result.Cause)
			require.NotNil(t, run.result.Bounds)
			require.Equal(t, 270*time.Second, run.end.Sub(run.began))
			require.LessOrEqual(t, run.setupDone.Sub(run.began), 10*time.Second)
			require.False(t, run.admitted.After(run.end))
		})
	}
}

// A configured eight-second poll can yield tracking updates only every sixteen
// seconds. The rehearsal window must still retain at least ten updates.
func TestRehearsalAdmissionWithSkippedPolls(t *testing.T) {
	for phase := range int64(16) {
		t.Run(strconv.FormatInt(phase, 10), func(t *testing.T) {
			t.Parallel()
			run := runClockAdmission(t, 400*time.Millisecond, 16*time.Second, 400*time.Millisecond, phase, func(p *clockAdmissionPorts) {
				p.converge = true
				p.poll = 8 * time.Second
				p.policy.WindowSeconds = 240
				selectBounds := p.selectBounds
				p.selectBounds = func(ctx context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
					require.Equal(t, int64(240), e.EndUnixSeconds-e.StartUnixSeconds)
					return selectBounds(ctx, e, policy)
				}
			})
			require.Empty(t, run.result.Cause)
			require.NotNil(t, run.result.Bounds)
			require.Empty(t, run.result.FailedWindows)
			require.Equal(t, 270*time.Second, run.end.Sub(run.began))
			for _, updates := range run.result.Updates {
				require.GreaterOrEqual(t, updates, 10)
			}
		})
	}
}

// Both timings fit the existing admission limits. A 5.8 s reference gap
// exhausts a 1 s poll's lease; an 8 s poll keeps the same five-poll rule.
func TestRehearsalPollTimingCharacterization(t *testing.T) {
	for _, poll := range []int64{1, 8} {
		t.Run(strconv.FormatInt(poll, 10), func(t *testing.T) {
			period := time.Duration(poll) * time.Second
			seed, setup, window := clockAdmissionTiming(400*time.Millisecond, period)
			require.Equal(t, 10*time.Second, seed)
			require.Equal(t, 10*time.Second, setup)
			require.Equal(t, 150*time.Second, window)
			seed, setup, window = rehearsalClockAdmissionTiming(400*time.Millisecond, period)
			require.Equal(t, 10*time.Second, seed)
			require.Equal(t, 10*time.Second, setup)
			require.Equal(t, 270*time.Second, window)
			require.NoError(t, CheckClockAdmissionTiming(400*time.Millisecond, period))
			o := clocks.LeaseObservation{Available: true, ErrorBoundNS: 1000, MeasuredUnixNS: "1793930005800000000", ReferenceUnixNS: "1793930000000000000", ReferenceAgeNS: 5800000000, PollSeconds: strconv.FormatInt(poll, 10), ResidualPPM: "0", SkewPPM: "0", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001001", IntervalNS: 20000000, HostElapsedNS: 20000000}
			lease, err := clocks.RecordLease(o, o.HostElapsedNS)
			require.NoError(t, err)
			require.Equal(t, poll == 1, lease.Expired)
			require.Equal(t, max(int64(0), poll*5e9-o.ReferenceAgeNS-o.HostElapsedNS), lease.RemainingNS)
			refresh, err := clocks.LeaseRefreshBudget(o)
			require.NoError(t, err)
			require.Equal(t, 10*period, refresh)
		})
	}
}
