package callharness

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalledPath is a caller socket on a UDP path that loses the caller's first
// dropChecks connectivity checks, as a firewall rule that is still
// propagating would, and with dropMedia every packet that is not a check.
type stalledPath struct {
	net.PacketConn
	dropChecks int
	dropMedia  bool

	mu      sync.Mutex
	dropped int
}

func (s *stalledPath) WriteTo(p []byte, addr net.Addr) (int, error) {
	_, check := stunMessage(p, stunBindingRequest)
	s.mu.Lock()
	drop := (check && s.dropped < s.dropChecks) || (!check && s.dropMedia)
	if drop && check {
		s.dropped++
	}
	s.mu.Unlock()
	if drop {
		return len(p), nil
	}

	return s.PacketConn.WriteTo(p, addr)
}

func (s *stalledPath) droppedChecks() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.dropped
}

// stalledCaller attaches an external caller to system whose socket is path.
func stalledCaller(t *testing.T, system *Harness, path *stalledPath) *Harness {
	t.Helper()
	h, err := Start(Options{External: &ExternalTopology{
		SignalingURL: system.SignalingURL(), RelayAddr: system.RelayAddr(),
		CallerSocket: func() (net.PacketConn, error) {
			conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			path.PacketConn = conn
			return path, nil
		},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, h.Close()) })

	return h
}

func hasState(changes []StateChange, state string) bool {
	for _, change := range changes {
		if change.State == state {
			return true
		}
	}

	return false
}

// TestDialRidesOutShortPathStall (#122): the UDP path loses the caller's
// first 12 checks (about 2.4 s). With Pion's default the caller gives its
// only pair up after 8 unanswered checks; the ICE-lite server never checks
// back, so the dial waits out its deadline and reports a path that never
// opened. With the browser-like budget the caller keeps checking and
// connects once the path opens.
func TestDialRidesOutShortPathStall(t *testing.T) {
	system, err := Start(Options{Relay: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, system.Close()) })

	t.Run("Pion default gives up", func(t *testing.T) {
		path := &stalledPath{dropChecks: 12}
		h := stalledCaller(t, system, path)
		h.iceChecks = 8
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		call, err := h.Dial(ctx, CallOptions{Worker: -1})
		require.Nil(t, call)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		var failure *DialError
		require.ErrorAs(t, err, &failure)
		report := failure.Report
		assert.Equal(t, DialFailed, report.Outcome)
		assert.Equal(t, DialFailurePathNeverOpened, report.Failure)
		assert.Equal(t, err.Error(), report.Error)
		assert.NotEmpty(t, report.Session)
		assert.GreaterOrEqual(t, report.Elapsed, 4*time.Second)
		assert.Equal(t, "checking", report.ICEState)
		assert.Equal(t, "new", report.DTLSState)
		assert.Equal(t, "connecting", report.ConnectionState)
		assert.True(t, hasState(report.ICEStates, "checking"), "ICE history %v", report.ICEStates)
		assert.Empty(t, report.DTLSStates, "DTLS never started")
		require.Len(t, report.CandidatePairs, 1)
		pair := report.CandidatePairs[0]
		assert.Equal(t, system.RelayAddr().String(), pair.Remote)
		assert.EqualValues(t, 8, pair.ChecksSent, "Pion's default number of checks")
		assert.Zero(t, pair.ResponsesReceived)
		assert.Zero(t, pair.LastResponse)
		assert.Less(t, pair.LastCheck, 3*time.Second, "the caller stopped checking during the stall")
		assert.Equal(t, 8, path.droppedChecks())
	})

	t.Run("browser-like budget connects", func(t *testing.T) {
		path := &stalledPath{dropChecks: 12}
		h := stalledCaller(t, system, path)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call, err := h.Dial(ctx, CallOptions{Worker: -1})
		require.NoError(t, err)
		report := call.DialReport()
		assert.Equal(t, DialConnected, report.Outcome)
		assert.Empty(t, report.Failure)
		assert.Equal(t, call.SessionID(), report.Session)
		assert.Equal(t, "connected", report.ConnectionState)
		assert.Equal(t, "connected", report.DTLSState)
		assert.True(t, hasState(report.ICEStates, "connected"), "ICE history %v", report.ICEStates)
		assert.True(t, hasState(report.DTLSStates, "connected"), "DTLS history %v", report.DTLSStates)
		require.Len(t, report.CandidatePairs, 1)
		pair := report.CandidatePairs[0]
		assert.Greater(t, pair.ChecksSent, uint64(12), "checks continued past the stall")
		assert.Positive(t, pair.ResponsesReceived)
		assert.Greater(t, report.Elapsed, 2*time.Second, "connected only after the stall")
		assert.Equal(t, 12, path.droppedChecks())
		_, err = call.Hangup(ctx)
		require.NoError(t, err)
	})
}

// TestDialReportsICEConnectedStall: checks are answered and ICE connects,
// but DTLS never completes. That is a product failure, reported apart from
// a path that never opened.
func TestDialReportsICEConnectedStall(t *testing.T) {
	system, err := Start(Options{Relay: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, system.Close()) })
	h := stalledCaller(t, system, &stalledPath{dropMedia: true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = h.Dial(ctx, CallOptions{Worker: -1})
	var failure *DialError
	require.ErrorAs(t, err, &failure)
	report := failure.Report
	assert.Equal(t, DialFailureICEConnectedStalled, report.Failure)
	assert.True(t, hasState(report.ICEStates, "connected"), "ICE history %v", report.ICEStates)
	assert.NotEqual(t, "connected", report.DTLSState)
	assert.NotEqual(t, "connected", report.ConnectionState)
	require.Len(t, report.CandidatePairs, 1)
	assert.Positive(t, report.CandidatePairs[0].ResponsesReceived)
}

// TestDialFailureKinds: early failures are DialErrors too, and the kind
// follows how far the dial got.
func TestDialFailureKinds(t *testing.T) {
	h, err := Start(Options{External: &ExternalTopology{
		SignalingURL: "http://127.0.0.1:1/calls", RelayAddr: netip.MustParseAddrPort("127.0.0.1:9"),
	}})
	require.NoError(t, err)
	_, err = h.Dial(context.Background(), CallOptions{Worker: -1})
	var failure *DialError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, DialFailureSignaling, failure.Report.Failure)
	assert.Empty(t, failure.Report.Session)
	assert.Empty(t, failure.Report.CandidatePairs)
	require.NoError(t, h.Close())
	_, err = h.Dial(context.Background(), CallOptions{Worker: -1})
	require.ErrorIs(t, err, ErrHarnessClosed)
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, DialFailureHarnessClosed, failure.Report.Failure)

	answered := DialReport{CandidatePairs: []CandidatePairChecks{{ChecksSent: 9, ResponsesReceived: 1}}}
	connected := answered
	connected.ICEStates = []StateChange{{State: "checking"}, {State: "connected"}}
	for _, tc := range []struct {
		name                        string
		closed, offered, negotiated bool
		report                      DialReport
		want                        string
	}{
		{"setup", false, false, false, DialReport{}, DialFailureSetup},
		{"signaling", false, true, false, DialReport{}, DialFailureSignaling},
		{"path never opened", false, true, true, DialReport{CandidatePairs: []CandidatePairChecks{{ChecksSent: 50}}}, DialFailurePathNeverOpened},
		{"ICE stalled", false, true, true, answered, DialFailureICEStalled},
		{"ICE connected, DTLS stalled", false, true, true, connected, DialFailureICEConnectedStalled},
		{"harness closed", true, true, true, connected, DialFailureHarnessClosed},
	} {
		assert.Equal(t, tc.want, dialFailure(tc.closed, tc.offered, tc.negotiated, tc.report), tc.name)
	}
}
