package operatorgates

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/controlplane"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

func TestRollbackContinuousLeaseCoverage(t *testing.T) {
	for _, mode := range []string{"fresh", "expired", "missing", "gap", "bound", "event-after-read", "no-budget", "unknown", "cached", "zero-failed", "mixed-failed", "partial-hole", "restore-budget", "context-budget", "zero-start", "zero-end", "reversed-move"} {
		t.Run(mode, func(t *testing.T) {
			r, peer, _ := runtimeFixture(t)
			const base int64 = 1793930000
			c := &FakeClock{Now: time.Unix(base, 0), Tick: time.Second}
			r.Session.clock, r.Session.Budget.clock = c, c
			g, initial := leaseGuardFixture(t, c)
			for slot, sample := range initial.Evidence.Clocks {
				sample.TrackingLog.Rows = []clocks.TrackingRow{{UnixSeconds: base, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"}}
				initial.Evidence.Clocks[slot] = sample
			}
			require.NoError(t, g.accept(initial))
			if mode == "partial-hole" {
				partial := LeaseRead{Evidence: clocks.Evidence{Clocks: map[string]clocks.Sample{}}, Sent: initial.Sent}
				for slot, sample := range initial.Evidence.Clocks {
					observation := *sample.Lease
					if slot == "relay" {
						observation.ErrorBoundNS = -1
					}
					sample.Lease = &observation
					sample.TrackingLog = &clocks.TrackingLog{ReadComplete: true, Rows: []clocks.TrackingRow{{UnixSeconds: base + 1, LeapStatus: "N", MaxErrorSeconds: .001, MaxErrorDecimal: "1.000e-03"}}}
					partial.Evidence.Clocks[slot] = sample
				}
				require.Error(t, g.accept(partial)) // Session cursors already consumed these rows.
			}
			c.Advance(200 * time.Second)
			start, finish := c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond)
			calls := 0
			g.read = func(context.Context, time.Duration) (LeaseRead, error) {
				calls++
				if mode == "unknown" {
					r.Session.TransportLost()
					return LeaseRead{}, fault("command-result-unknown")
				}
				next := LeaseRead{Sent: map[string]time.Duration{}, Evidence: clocks.Evidence{Clocks: map[string]clocks.Sample{}}}
				for slot, old := range initial.Evidence.Clocks {
					o := *old.Lease
					o.ReferenceUnixNS, o.MeasuredUnixNS = fmt.Sprint((base+200)*1e9), fmt.Sprint((base+200)*1e9+100000000)
					if mode == "expired" {
						o.ReferenceUnixNS, o.ReferenceAgeNS = fmt.Sprint((base+194)*1e9), 6100000000
					}
					if mode == "event-after-read" {
						o.ReferenceUnixNS, o.MeasuredUnixNS, o.ReferenceAgeNS = fmt.Sprint((base+198)*1e9), fmt.Sprint((base+199)*1e9), 1000000000
					}
					rows := []clocks.TrackingRow{}
					for second := base + 1; second <= base+200; second++ {
						if (mode == "gap" && second < base+100) || (mode == "partial-hole" && second == base+1) {
							continue
						}
						value, token := 1e-6, "1.000e-06"
						if mode == "bound" && second == base+100 {
							value, token = .001, "1.000e-03"
						}
						rows = append(rows, clocks.TrackingRow{UnixSeconds: second, LeapStatus: "N", MaxErrorSeconds: value, MaxErrorDecimal: token})
					}
					next.Evidence.Clocks[slot] = clocks.Sample{Lease: &o, TrackingLog: &clocks.TrackingLog{ReadComplete: true, Rows: rows}}
					next.Sent[slot] = c.Elapsed() - 50*time.Millisecond
				}
				return next, nil
			}
			if mode == "cached" {
				c.Advance(-200 * time.Second)
				start, finish = c.Wall(), c.Wall()
			}
			if mode == "missing" {
				delete(g.leases, "relay")
			}
			if mode == "zero-failed" || mode == "mixed-failed" || mode == "zero-start" || mode == "zero-end" || mode == "reversed-move" {
				g.invalid = true
			}
			if mode == "zero-failed" || mode == "zero-start" || mode == "zero-end" || mode == "reversed-move" {
				start, finish = c.Wall().Add(-time.Second), c.Wall()
			}
			end := 10 * time.Minute
			if mode == "restore-budget" {
				end = c.Elapsed() + 120*time.Second
			}
			if mode == "no-budget" {
				end = c.Elapsed() + time.Minute
			}
			r.Config.Operation, r.Config.Target = "rollback", "from"
			r.BeforeMutation = func(context.Context) error { t.Fatal("restoration armed a fault"); return nil }
			previous := peer.respond
			executed := false
			peer.respond = func(q operator.Request) operator.Reply {
				if q.Action != "drain" {
					return previous(q)
				}
				executed = true
				if mode == "no-budget" || mode == "restore-budget" {
					c.Advance(59 * time.Second)
				}
				move := controlplane.MoveResult{ID: "call", Kind: "move", Start: start, End: finish}
				moves := []controlplane.MoveResult{move}
				failed := controlplane.MoveResult{ID: "failed", Error: "move-in-progress"}
				if mode == "zero-start" {
					moves = []controlplane.MoveResult{{End: finish}}
				}
				if mode == "zero-end" {
					moves = []controlplane.MoveResult{{Start: start}}
				}
				if mode == "reversed-move" {
					moves = []controlplane.MoveResult{{Start: finish, End: start}}
				}
				if mode == "zero-failed" {
					moves = []controlplane.MoveResult{failed}
				}
				if mode == "mixed-failed" {
					moves = append([]controlplane.MoveResult{failed}, moves...)
				}
				return hostReply(t, oracleJSON(map[string]any{"kind": "ok", "started": c.Wall().Add(-time.Second), "ended": c.Wall(), "data": controlplane.DrainResult{Moves: moves}}))
			}
			ctx := context.Background()
			if mode == "context-budget" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, RestoreReserve+time.Second)
				defer cancel()
			}
			_, err := r.StepWithLeases(ctx, "drain", "shared", end, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g})
			if mode == "unknown" {
				require.EqualError(t, err, "command-result-unknown")
				require.True(t, r.Session.Latched())
			} else {
				require.NoError(t, err, "restoration must execute even without timing")
				require.False(t, r.Session.Latched())
			}
			require.True(t, executed)
			covered := mode == "fresh" || mode == "cached"
			if covered != (len(r.Session.clockMissingIntervals) == 0) {
				t.Fatal("MUTATION: rollback-continuous-lease", mode, r.Session.clockMissingIntervals)
			}
			wantReads := 1
			if mode == "missing" || mode == "cached" || mode == "no-budget" || mode == "partial-hole" || mode == "restore-budget" || mode == "context-budget" || mode == "zero-failed" || mode == "mixed-failed" || mode == "zero-start" || mode == "zero-end" || mode == "reversed-move" {
				wantReads = 0
			}
			if wantReads != calls {
				t.Fatal("MUTATION: rollback-read-budget", mode, calls, wantReads)
			}
			if mode == "restore-budget" {
				_, err = r.StepWithLeases(context.Background(), "worker-start", "w1", end, ProcessExtra{Worker: "w1"}, LeaseStep{Guard: g})
				require.NoError(t, err, "optional proof must leave restoration its existing floor")
			}
			if mode == "fresh" {
				timelines := clocks.NewBoundCoverageAttempt(clocks.Evidence{Clocks: g.samples}).Clocks
				window := []observe.CoverageWindow{{Kind: "move", StartUnixNS: fmt.Sprint(start.UnixNano()), EndUnixNS: fmt.Sprint(finish.UnixNano())}}
				evaluate := func() []string {
					return observe.EvaluateBoundClockCoverage(timelines, g.frontend, g.control, 120000, 170000, 50000, window, nil)[0].Causes
				}
				require.Contains(t, evaluate(), "clock-coverage-missing", "endpoint proof must not waive the open event tail")
				for slot, timeline := range timelines {
					timeline.Rows = append(timeline.Rows, observe.CoverageRow{UnixSeconds: base + 201, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"})
					timelines[slot] = timeline
				}
				require.Empty(t, evaluate(), "a genuinely closed, bounded event qualifies")
			}
			if !covered {
				missing := r.Session.clockMissingIntervals["shared"]
				kind := "clock-lease-missing"
				switch mode {
				case "expired", "no-budget", "restore-budget", "context-budget":
					kind = "clock-lease-expired"
				case "bound":
					kind = "clock-lease-bound"
				case "zero-failed", "mixed-failed", "zero-start", "zero-end", "reversed-move":
					kind = "clock-config-differ"
				}
				if missing.Kind != kind {
					t.Fatal("MUTATION: rollback-lease-cause", mode, missing.Kind, kind)
				}
				if missing.FromUnixSeconds < base || missing.FromUnixSeconds != start.Unix() || missing.UnixSeconds != finish.Unix() {
					t.Fatal("MUTATION: rollback-local-span", mode, missing, start, finish)
				}
				require.Equal(t, start.Unix(), missing.FromUnixSeconds)
				require.Equal(t, finish.Unix(), missing.UnixSeconds)
				rows := []observe.CoverageRow{{UnixSeconds: start.Unix() - 2, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"}, {UnixSeconds: finish.Unix() + 2, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"}}
				results := observe.EvaluateBoundClockCoverage(map[string]observe.CoverageTimeline{"shared": {ReadComplete: true, Rows: rows, Breaks: []observe.CoverageBreak{missing}}}, "shared", "shared", 120000, 170000, 50000, []observe.CoverageWindow{{Kind: "move", StartUnixNS: fmt.Sprint(start.UnixNano()), EndUnixNS: fmt.Sprint(finish.UnixNano())}}, nil)
				require.NotEmpty(t, results[0].Causes, "uncovered restoration lost timing")
				if mode == "zero-failed" || mode == "mixed-failed" || mode == "zero-start" || mode == "zero-end" || mode == "reversed-move" {
					timeline := observe.CoverageTimeline{ReadComplete: true, Breaks: []observe.CoverageBreak{missing}}
					for second := base + 90; second <= base+202; second++ {
						timeline.Rows = append(timeline.Rows, observe.CoverageRow{UnixSeconds: second, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"})
					}
					unaffected := observe.EvaluateBoundClockCoverage(map[string]observe.CoverageTimeline{"shared": timeline}, "shared", "shared", 120000, 170000, 50000, []observe.CoverageWindow{{Kind: "move", StartUnixNS: fmt.Sprint((base + 100) * 1e9), EndUnixNS: fmt.Sprint((base + 100) * 1e9)}}, nil)
					require.Empty(t, unaffected[0].Causes, "failed sub-move must not poison unrelated windows")
				}
			}
		})
	}
}

func TestRollbackProofBoundaries(t *testing.T) {
	for _, mode := range []string{"old-after-event", "new-before-old", "empty-old", "empty-new", "short-prefix", "three-second-prefix", "same-row"} {
		t.Run(mode, func(t *testing.T) {
			const base int64 = 1793930000
			c := &FakeClock{Now: time.Unix(base+200, 0), Tick: 201 * time.Second}
			g, read := leaseGuardFixture(t, c)
			before := clocks.CoverageAttempt{Clocks: map[string]observe.CoverageTimeline{}}
			oldLast, newFirst, newLast := base, base+1, base+200
			want := "clock-lease-missing"
			switch mode {
			case "old-after-event":
				oldLast = base + 200
			case "new-before-old":
				oldLast, newLast = base+150, base+149
			case "short-prefix":
				oldLast, newFirst = base+198, base+199
				want = ""
			case "three-second-prefix":
				oldLast, newFirst = base+197, base+198
				want = "clock-lease-bound"
			case "same-row":
				oldLast, newFirst, newLast = base+199, base+199, base+199
				want = ""
			}
			for slot, sample := range read.Evidence.Clocks {
				before.Clocks[slot] = observe.CoverageTimeline{ReadComplete: true, Rows: []observe.CoverageRow{{UnixSeconds: oldLast, LeapStatus: "N", MaxErrorSeconds: 1e-6, MaxErrorDecimal: "1.000e-06"}}}
				sample.TrackingLog = &clocks.TrackingLog{ReadComplete: true}
				for second := newFirst; second <= newLast; second++ {
					value, token := 1e-6, "1.000e-06"
					if mode == "three-second-prefix" && second == base+198 {
						value, token = .001, "1.000e-03"
					}
					sample.TrackingLog.Rows = append(sample.TrackingLog.Rows, clocks.TrackingRow{UnixSeconds: second, LeapStatus: "N", MaxErrorSeconds: value, MaxErrorDecimal: token})
				}
				if mode == "empty-new" {
					sample.TrackingLog.Rows = nil
				}
				if mode == "empty-old" {
					timeline := before.Clocks[slot]
					timeline.Rows = nil
					before.Clocks[slot] = timeline
				}
				observation := *sample.Lease
				observation.ReferenceUnixNS, observation.MeasuredUnixNS = fmt.Sprint((base+200)*1e9), fmt.Sprint((base+200)*1e9+100000000)
				sample.Lease = &observation
				read.Evidence.Clocks[slot] = sample
			}
			require.NoError(t, g.accept(read))
			err := g.restorationProof(before, read, c.Wall().Add(-100*time.Millisecond), c.Wall().Add(-90*time.Millisecond))
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != want {
				t.Fatal("MUTATION: rollback-proof-boundary", mode, got, want)
			}
		})
	}
}
