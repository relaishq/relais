package operatorgates

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/relay"
)

func TestRelayWaitCharacterization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*relay.RelayStatus)
		expired bool
		code    string
	}{
		{name: "first-read-rejoined"},
		{name: "deadline-workers", change: func(s *relay.RelayStatus) { s.Stats.Workers = 1 }, expired: true, code: "missing-relay-evidence"},
		{name: "deadline-continuity", change: func(s *relay.RelayStatus) {
			s.Stats.Workers, s.Stats.WorkerPackets = 1, 0
		}, expired: true, code: "relay-continuity-failed"},
		{name: "deadline-packets", change: func(s *relay.RelayStatus) { s.Stats.WorkerPackets = 0 }, expired: true, code: "relay-continuity-failed"},
		{name: "self-fence", change: func(s *relay.RelayStatus) { s.Stats.SelfFences = 1 }, code: "relay-self-fence"},
		{name: "self-fence-before-packets", change: func(s *relay.RelayStatus) {
			s.Stats.SelfFences, s.Stats.WorkerPackets = 1, 0
		}, code: "relay-self-fence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTrialCluster(t)
			if tc.expired {
				// The first post-stop read ends at 4 s; its 500 ms sleep
				// reaches the gate end without another host request.
				c.g.end = 4500 * time.Millisecond
			}
			c.g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
				r, err := c.step(ctx, op, slot, end, extra)
				if op == "relay-inspect" && c.relay > 0 && tc.change != nil {
					tc.change(&r.Inspection.Status)
				}
				return r, err
			}
			err := c.g.relay(context.Background(), false, 1)
			if code(err) != tc.code || c.g.unknown || c.count("relay-inspect") != 2 {
				t.Fatalf("relay wait result: err=%v unknown=%v inspections=%d", err, c.g.unknown, c.count("relay-inspect"))
			}
			wantRestarts := 0
			if tc.code == "" {
				wantRestarts = 1
				if c.g.evidence.RelayEpochDelta == nil || *c.g.evidence.RelayEpochDelta != 1 || c.g.evidence.RoutesRestored == nil || *c.g.evidence.RoutesRestored != 1 {
					t.Fatalf("missing continuity evidence: %+v", c.g.evidence)
				}
			}
			if c.count("relay-replenish") != wantRestarts || c.g.evidence.Restarts != wantRestarts {
				t.Fatalf("relay replenish/restarts: ops=%v evidence=%+v", c.ops, c.g.evidence)
			}
			if got := c.g.evidence.ExpiredWait; tc.expired {
				if got == nil || got.Purpose != "relay-rejoined" || got.Polls != 1 || got.WaitedMS != 1500 || c.clock.Elapsed() != c.g.end {
					t.Fatalf("deadline diagnostic: %+v elapsed=%v", got, c.clock.Elapsed())
				}
			} else if got != nil {
				t.Fatalf("unexpected expired wait: %+v", got)
			}
		})
	}
}

func TestRelayWaitsForFullRejoin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		original int
		change   func(*relay.RelayStatus)
	}{
		{"worker-packets", 1, func(s *relay.RelayStatus) { s.Stats.WorkerPackets = 0 }},
		{"restored-routes", 1, func(s *relay.RelayStatus) { s.Stats.RoutesRestored = 0 }},
		{"all-original-routes", 2, func(s *relay.RelayStatus) { s.Stats.RoutesRestored = 1 }},
		{"new-instance", 1, func(s *relay.RelayStatus) { s.Instance = "relay-0" }},
		{"higher-epoch", 1, func(s *relay.RelayStatus) { s.Continuity.Epoch = 1 }},
		{"public-address", 1, func(s *relay.RelayStatus) { s.Public = netip.MustParseAddrPort("192.0.2.10:3478") }},
		{"private-address", 1, func(s *relay.RelayStatus) { s.Private = netip.MustParseAddrPort("10.0.0.10:3478") }},
		{"continuity-evidence", 1, func(s *relay.RelayStatus) { s.Continuity = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTrialCluster(t)
			var polls []time.Duration
			c.g.p.Step = func(ctx context.Context, op, slot string, end time.Duration, extra ProcessExtra) (StepResult, error) {
				postStop := op == "relay-inspect" && c.relay > 0
				if postStop {
					polls = append(polls, c.clock.Elapsed())
				}
				r, err := c.step(ctx, op, slot, end, extra)
				if postStop {
					r.Inspection.Status.Stats.RoutesRestored = uint64(tc.original)
					if len(polls) == 1 {
						tc.change(&r.Inspection.Status)
					}
				}
				return r, err
			}
			if err := c.g.relay(context.Background(), false, tc.original); err != nil {
				t.Fatalf("relay rejected a transient rejoin status: %v; ops=%v", err, c.ops)
			}
			// Each fake read takes 1 s. The remaining 500 ms is the poll sleep.
			if len(polls) != 2 || polls[1]-polls[0] != 1500*time.Millisecond || c.count("relay-replenish") != 1 {
				t.Fatalf("relay did not wait then replenish: polls=%v ops=%v", polls, c.ops)
			}
			e := c.g.evidence
			if c.g.unknown || e.ExpiredWait != nil || e.Restarts != 1 || e.RelayEpochDelta == nil || *e.RelayEpochDelta != 1 || e.RoutesRestored == nil || *e.RoutesRestored != uint64(tc.original) {
				t.Fatalf("rejoin evidence: unknown=%v evidence=%+v", c.g.unknown, e)
			}
		})
	}
}
