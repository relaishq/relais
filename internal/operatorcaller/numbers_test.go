package operatorcaller

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
)

func TestCallerNumericBoundaries(t *testing.T) {
	base := loadgen.Stats{Interval: time.Second, IntervalPacing: loadgen.Pacing{SentFrames: 100}}
	for _, change := range []func(*loadgen.Stats){func(s *loadgen.Stats) { s.IntervalLateness.P99 = -1 }, func(s *loadgen.Stats) { s.CPUPerGOMAXPROCS = -1 }, func(s *loadgen.Stats) {
		s.IntervalPacing.SentFrames = math.MaxUint64
		s.IntervalPacing.SkippedFrames = 1
	}} {
		sample := base
		change(&sample)
		raw, _ := json.Marshal(sample)
		w := SaturationWindow{PlannedSeconds: 60}
		aborted, published := false, false
		if err := RecordGeneratorSample(raw, &w, func() error { published = true; return nil }, func(string) error { return nil }, func() { aborted = true }); err == nil || !aborted || published {
			t.Fatal("MUTATION: caller-invalid-numbers-abort", err, aborted)
		}
	}
	w := SaturationWindow{PlannedSeconds: 60, FlaggedSeconds: float64(maxSafeInteger)}
	base.CPUPerGOMAXPROCS = 100
	raw, _ := json.Marshal(base)
	if stopping, err := w.Add(raw); !stopping || err == nil {
		t.Fatal("caller total overflow accepted")
	}
	m := fixtureManifest()
	clock := &testClock{time.UnixMilli(m.CreatedAt)}
	broker, _ := NewBroker(m, NewMailbox(&memoryBox{}), clock, &testFence{})
	broker.sequence = maxSafeInteger
	if _, err := broker.Feed(context.Background(), "operation-test", Payload{Kind: "finish"}, clock.at.Add(time.Minute)); err == nil {
		t.Fatal("exhausted sequence accepted")
	}
	mailbox := ProtocolMailbox{next: maxSafeInteger}
	if mailbox.Create(context.Background(), "scope/key", nil) != CreateUnknown {
		t.Fatal("exhausted request ID accepted")
	}
	burst := fixtureBurst(clock.at)
	burst.Probes[0].CallerReceive = clock.at.Add(200 * 24 * time.Hour)
	if burst.Validate() == nil {
		t.Fatal("unbounded probe span accepted")
	}
}
