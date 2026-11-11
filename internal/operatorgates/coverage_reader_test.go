package operatorgates

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

// coveragePeer is the isolated adapter: four seconds per read, and hosts that
// log one tracking row per eight-second poll.
type coveragePeer struct {
	t        *testing.T
	clock    *FakeClock
	started  time.Time // Host time when the session clock read zero.
	requests []operator.Request
	reply    func(operator.Request, operator.Frame) operator.Frame
}

func (p *coveragePeer) Send(_ context.Context, f operator.Frame) error {
	if f.Type != "request" || f.Session != CoverageSession || f.Request.Operation != operator.HostExecute || f.Request.Action != "clock" || len(f.Request.Script) == 0 || f.Request.RemainingMS <= 0 {
		p.t.Fatal("coverage read is not one bounded host.execute clock request", f)
	}
	p.requests = append(p.requests, *f.Request)
	return nil
}
func (p *coveragePeer) Receive(context.Context) (operator.Frame, error) {
	q := p.requests[len(p.requests)-1]
	p.clock.Advance(4 * time.Second)
	poll := 8 * time.Second
	now := p.started.Add(p.clock.Elapsed())
	r := hostReply(p.t, oracleJSON(clocks.Sample{MeasuredUnixNS: fmt.Sprint(now.UnixNano()), TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: pollRows(time.Unix(slowHostEpoch, 0).Add(-2*poll), now, poll)}}))
	r.ID = q.ID
	f := operator.Frame{Type: "result", Session: CoverageSession, Result: &r}
	if p.reply != nil {
		f = p.reply(q, f)
	}
	return f, nil
}

// The last takeover of a run has no later policy read. Its closing rows exist
// on the hosts; only the trailing drain can retain them.
func lastTakeoverFixture(t *testing.T) (*Session, *coveragePeer, []observe.CoverageWindow) {
	t.Helper()
	s, c := coverageFixtureSession(t)
	s.inventoryComponents = map[string][]string{"shared": {"signal", "control"}, "w1": {"w1"}, "w2": {"w2"}}
	epoch := time.Unix(slowHostEpoch, 0)
	kill := epoch.Add(3 * time.Second)
	end := kill.Add(4500 * time.Millisecond)
	// The policy reads stopped at the kill: every host has rows up to the epoch.
	before := clocks.Evidence{Version: clocks.EvidenceVersion, At: c.Wall(), Clocks: map[string]clocks.Sample{}}
	for _, slot := range inventory.Slots {
		before.Clocks[slot] = clocks.Sample{TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: pollRows(epoch.Add(-16*time.Second), kill, 8*time.Second)}}
	}
	retainTestClockAttempt(s, before)
	at := fmt.Sprintf("%019d", kill.UnixNano())
	s.retainClockBracket("w1", at, at, at, nil)
	windows := []observe.CoverageWindow{{Kind: "takeover", Worker: "w1", StartUnixNS: at, EndUnixNS: fmt.Sprintf("%019d", end.UnixNano())}}
	// The drain starts as the takeover ends.
	return s, &coveragePeer{t: t, clock: c, started: end.Add(-c.Elapsed())}, windows
}

func TestProductionDrainCoversLastTakeover(t *testing.T) {
	for _, mode := range []string{"reader", "no-reader", "reference-limit"} {
		t.Run(mode, func(t *testing.T) {
			s, peer, windows := lastTakeoverFixture(t)
			var err error
			s.timing, err = slowTiming.budget(2)
			require.NoError(t, err)
			if mode == "reference-limit" {
				s.timing = referenceTiming
			}
			read := NewCoverageReader(peer, nil)
			if mode == "no-reader" {
				read = nil
			}
			before := s.Final(nil)
			e := s.collectCoverage(context.Background(), clocks.Bounds{}, windows, read, func(_ context.Context, d time.Duration) error { peer.clock.Advance(d); return nil }, s.Budget.end)
			require.Len(t, e.Results, 1)
			missing := slices.Contains(e.Results[0].Causes, "clock-coverage-missing")
			if mode == "reader" {
				// One read of all four hosts takes sixteen seconds: more than the
				// reference limit, inside the configured one.
				if !e.DrainComplete || len(e.Results[0].Causes) != 0 || len(peer.requests) != 4 {
					t.Fatalf("MUTATION: production-drain-reader: complete=%t causes=%v reads=%d", e.DrainComplete, e.Results[0].Causes, len(peer.requests))
				}
			} else if e.DrainComplete || !missing {
				t.Fatalf("MUTATION: drain-limit-or-reader: %s complete=%t causes=%v", mode, e.DrainComplete, e.Results[0].Causes)
			}
			require.False(t, s.Latched())
			require.Equal(t, before, s.Final(nil), "the drain must not change the session's requests, trace or latch")
		})
	}
}

// A reply the reader cannot match closes the reader, never the session.
func TestCoverageReaderFailuresStayIsolated(t *testing.T) {
	for _, mode := range []string{"wrong-id", "wrong-session", "unknown-transport", "not-submitted", "exit-1", "unbounded"} {
		t.Run(mode, func(t *testing.T) {
			s, peer, _ := lastTakeoverFixture(t)
			peer.reply = func(_ operator.Request, f operator.Frame) operator.Frame {
				one := 1
				switch mode {
				case "wrong-id":
					f.Result.ID = "0"
				case "wrong-session":
					f.Session = s.ID
				case "unknown-transport":
					f.Result.Transport = operator.Unknown
				case "not-submitted":
					f.Result.Transport, f.Result.NotSubmitted, f.Result.RefusalCode = operator.Failed, true, "deadline-before-submission"
				case "exit-1":
					f.Result.ExitCode = &one
				}
				return f
			}
			read := NewCoverageReader(peer, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if mode == "unbounded" {
				ctx = context.Background()
			}
			_, err := read(ctx, "w1", "script")
			require.EqualError(t, err, "clock-coverage-read-unavailable")
			sent := len(peer.requests)
			peer.reply = nil
			_, err = read(ctx, "w2", "script")
			// An uncorrelated reply may still arrive later: the peer is not reused.
			// A read without a deadline is never sent.
			closed := mode == "wrong-id" || mode == "wrong-session" || mode == "unbounded"
			if closed != (err != nil && len(peer.requests) == sent) || mode == "unbounded" && sent != 0 {
				t.Fatal("MUTATION: coverage-reader-reuse", mode, err, sent, len(peer.requests))
			}
			require.False(t, s.Latched())
		})
	}
}
