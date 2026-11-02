package operatorgates

import (
	"context"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

type latencyBroker struct {
	GateBroker
	clock     *FakeClock
	latency   time.Duration
	exchanges int
}

func (b *latencyBroker) Arm(ctx context.Context, id string, ids []Identity) error {
	b.clock.Advance(2 * b.latency)
	b.exchanges += 2
	return b.GateBroker.Arm(ctx, id, ids)
}
func (b *latencyBroker) Feed(ctx context.Context, id string, r []Receipt) error {
	b.clock.Advance(2 * b.latency)
	b.exchanges += 2
	return b.GateBroker.Feed(ctx, id, r)
}

// Remote latency advances the same elapsed clock as warmup, consent, preflight
// freshness and admission. One successful mailbox create/read pair acknowledges
// each arm/feed; extra readiness and mailbox polling are outside this model.
func TestReleaseBatchLeavesFloorReserveAndMargin(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "serial", true: "batch"}[batch], func(t *testing.T) {
			w, p := newRoundTripWorld(t, batch)
			peer := w.runtime.Peer.(*observationPeer)
			respond := peer.respond
			peer.respond = func(req operator.Request) operator.Reply {
				w.clock.Advance(780 * time.Millisecond)
				return respond(req)
			}
			broker := &latencyBroker{GateBroker: p.Broker, clock: w.clock, latency: 780 * time.Millisecond}
			p.Broker = broker
			end := GateMaximum
			if batch {
				end -= callerCollectionReserve + callerStopReserve
			}
			out := RunReleaseGate(context.Background(), w.runtime.Config, p, end)
			if !out.Completed {
				t.Fatalf("MUTATION: release-latency-budget incomplete %+v counts=%v", out, w.counts)
			}
			if batch && w.clock.Elapsed()+operator.HostSubmissionMinimum+RestoreReserve+60*time.Second > end {
				t.Fatalf("MUTATION: release-latency-budget elapsed=%s end=%s", w.clock.Elapsed(), end)
			}
			total := broker.exchanges
			for _, n := range w.counts {
				total += n
			}
			t.Logf("elapsed=%s requests=%d mailbox=%d host=%v", w.clock.Elapsed(), total, broker.exchanges, w.counts)
		})
	}
}
