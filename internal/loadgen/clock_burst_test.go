package loadgen

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

// samplerTimes runs the sampler in a synctest bubble and returns each probe
// time since start. The script drives arms and receipts on the fake clock.
func samplerTimes(t *testing.T, cost time.Duration, script func(*eventBursts)) []time.Duration {
	var at []time.Duration
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.Mutex
		running, overlap := 0, false
		sample := func() error {
			mu.Lock()
			running++
			overlap = overlap || running > 1
			at = append(at, time.Since(start))
			mu.Unlock()
			time.Sleep(cost)
			mu.Lock()
			running--
			mu.Unlock()
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		bursts := newEventBursts()
		done := make(chan struct{})
		go func() {
			defer close(done)
			runClockSampler(ctx, sample, bursts)
		}()
		script(bursts)
		cancel()
		<-done
		require.False(t, overlap, "probes overlap")
	})
	return at
}

func burstID(op string) callharness.OperationIdentity {
	return callharness.OperationIdentity{RunID: "run", OperationID: op, SessionID: "session", Worker: "0", Epoch: 1}
}

func every(from, step time.Duration, count int) []time.Duration {
	out := make([]time.Duration, count)
	for i := range out {
		out[i] = from + time.Duration(i)*step
	}
	return out
}

func TestEventBurstProbesUntilReceiptThenThreeMore(t *testing.T) {
	at := samplerTimes(t, 0, func(b *eventBursts) {
		time.Sleep(time.Second)
		b.arm(burstID("move"))
		time.Sleep(1100 * time.Millisecond)
		b.received(burstID("move"))
		time.Sleep(10 * time.Second)
	})
	// Arm probes at once, then every 250 ms; three probes follow the receipt.
	// The 10 s background cadence continues.
	want := append(every(time.Second, burstProbeEvery, 5), every(2250*time.Millisecond, burstProbeEvery, 3)...)
	want = append(want, 10*time.Second)
	require.Equal(t, want, at)
}

func TestEventBurstStopsAtSixtyProbesPlusThree(t *testing.T) {
	at := samplerTimes(t, 0, func(b *eventBursts) {
		time.Sleep(100 * time.Millisecond)
		b.arm(burstID("stuck"))
		time.Sleep(30 * time.Second)
		// A receipt after the limit adds no probes.
		b.received(burstID("stuck"))
		time.Sleep(5 * time.Second)
	})
	burst := every(100*time.Millisecond, burstProbeEvery, 63)
	require.Equal(t, 15600*time.Millisecond, burst[62])
	want := append(append(append([]time.Duration{}, burst[:40]...), 10*time.Second), burst[40:]...)
	want = append(want, 20*time.Second, 30*time.Second)
	require.Equal(t, want, at)
}

func TestConcurrentEventBurstsShareOneCadence(t *testing.T) {
	at := samplerTimes(t, 0, func(b *eventBursts) {
		time.Sleep(time.Second)
		b.arm(burstID("first"))
		time.Sleep(600 * time.Millisecond)
		// The second event joins the running cadence; it adds no extra probe.
		b.arm(burstID("second"))
		b.arm(burstID("second"))
		time.Sleep(500 * time.Millisecond)
		b.received(burstID("first"))
		time.Sleep(time.Second)
		b.received(burstID("second"))
		time.Sleep(time.Second)
		// A repeated arm after the burst ended starts nothing.
		b.arm(burstID("first"))
		time.Sleep(6 * time.Second)
	})
	// One cadence from the first arm until three probes after the last receipt.
	require.Equal(t, append(every(time.Second, burstProbeEvery, 12), 10*time.Second), at)
}

func TestSlowProbesNeverOverlapOrQueue(t *testing.T) {
	at := samplerTimes(t, 600*time.Millisecond, func(b *eventBursts) {
		time.Sleep(time.Second)
		ops := []string{"a", "b", "c", "d"}
		for _, op := range ops {
			b.arm(burstID(op))
		}
		time.Sleep(1500 * time.Millisecond)
		for _, op := range ops {
			b.received(burstID(op))
		}
		time.Sleep(3500 * time.Millisecond)
	})
	// 600 ms probes run back to back while tickers drop missed ticks: never
	// concurrent (checked in samplerTimes) and no queued backlog afterwards.
	require.Equal(t, every(time.Second, 600*time.Millisecond, 6), at)
}

func TestEventBurstStopsOnCancel(t *testing.T) {
	at := samplerTimes(t, 0, func(b *eventBursts) {
		b.arm(burstID("cancelled"))
		time.Sleep(600 * time.Millisecond)
	})
	require.Equal(t, every(0, burstProbeEvery, 3), at)
}
