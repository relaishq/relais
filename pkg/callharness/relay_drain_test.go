package callharness

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/sessionstore"
	"github.com/stretchr/testify/require"
)

func TestRelayDrainLive(t *testing.T) {
	forAgentStores(t, func(t *testing.T, store sessionstore.Store) {
		p, w, h, relays, _ := relayChangeTopology(t, store, false, false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var calls, other []*Call
		for len(calls) < 8 {
			require.Less(t, len(calls)+len(other), 16)
			c, e := h.Dial(ctx, CallOptions{Video: true, RelayChange: true})
			require.NoError(t, e)
			_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
			require.NoError(t, e)
			if a.Slot == "r1" {
				calls = append(calls, c)
			} else {
				other = append(other, c)
			}
		}
		for _, c := range other {
			_, e := c.Hangup(ctx)
			require.NoError(t, e)
		}
		watch := watchRelayAttempts(store, calls)
		sent := make(chan error, 8)
		for _, c := range calls {
			go func() { sent <- watch.sendMedia(ctx, c, 5*time.Second) }()
		}
		time.Sleep(time.Second)
		baselines := map[*Call]*Report{}
		for _, c := range calls {
			baselines[c] = c.rec.report()
		}
		start := time.Now()
		drain, ok := any(p).(interface {
			DrainRelay(context.Context, string) error
		})
		require.True(t, ok, "one drain must change eight calls")
		err := drain.DrainRelay(ctx, "r1")
		watch.stop()
		require.NoError(t, err, "drain evidence:\n%s", watch.evidence(calls, start))
		time.Sleep(time.Second)
		old := relays[0].Stats().WorkerPackets
		time.Sleep(100 * time.Millisecond)
		require.Equal(t, old, relays[0].Stats().WorkerPackets)
		for range calls {
			require.NoError(t, <-sent)
		}
		pauses := map[string][]time.Duration{}
		limits := map[string]time.Duration{}
		for _, c := range calls {
			_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
			require.NoError(t, e)
			require.Equal(t, sessionstore.RelayAssignment{Slot: "r2", Generation: 1}, a)
			failures, e := w.SessionDecryptFailures(c.SessionID())
			require.NoError(t, e)
			require.Zero(t, failures)
			report, e := c.Hangup(ctx)
			require.NoError(t, e)
			require.Zero(t, report.DecryptionFailures.Total())
			for _, track := range report.Tracks {
				require.Zero(t, track.DuplicatePackets)
				var gap time.Duration
				for j := 1; j < len(track.Arrivals); j++ {
					if c.rec.start.Add(track.Arrivals[j]).After(start) {
						gap = max(gap, track.Arrivals[j]-track.Arrivals[j-1])
					}
				}
				pauses[track.Kind] = append(pauses[track.Kind], gap)
			}
			for _, base := range baselines[c].Tracks {
				limit := base.MediaGap + 50*time.Millisecond
				if limits[base.Kind] == 0 || limit < limits[base.Kind] {
					limits[base.Kind] = limit
				}
			}
			connected := 0
			for _, state := range c.RelayDTLSStates() {
				if state.State == "connected" {
					connected++
				}
			}
			require.Equal(t, 1, connected)
		}
		for kind, gaps := range pauses {
			p95 := percentile(gaps, 95)
			t.Logf("RELAY_SAMPLES track=%s gaps=%v", kind, gaps)
			t.Logf("RELAY_DRAIN track=%s n=8 p95=%s baseline_limit=%s reconnects=0 auth=0 duplicates=0", kind, p95, limits[kind])
			require.LessOrEqual(t, p95, relayTimingLimit(min(150*time.Millisecond, limits[kind])))
		}
		c, e := h.Dial(ctx, CallOptions{RelayChange: true})
		require.NoError(t, e)
		_, a, e := store.(sessionstore.RelayAssignments).RelayOwner(ctx, c.SessionID())
		require.NoError(t, e)
		require.Equal(t, "r2", a.Slot, "draining relay must not receive new calls")
		_, e = c.Hangup(ctx)
		require.NoError(t, e)
	})
}

// relayAttemptWatch saves each call's durable relay attempts while a drain
// runs. A drain that fails starts recovery, which replaces the failed attempt
// in the store, so the saved copy is the only record of its evidence.
type relayAttemptWatch struct {
	mu       sync.Mutex
	attempts map[string]map[uint64]sessionstore.RelayAttempt
	sends    map[*Call]string
	cancel   context.CancelFunc
	done     chan struct{}
}

func watchRelayAttempts(store sessionstore.Store, calls []*Call) *relayAttemptWatch {
	ctx, cancel := context.WithCancel(context.Background())
	w := &relayAttemptWatch{attempts: map[string]map[uint64]sessionstore.RelayAttempt{}, sends: map[*Call]string{}, cancel: cancel, done: make(chan struct{})}
	confirmations := store.(sessionstore.RelayConfirmations)
	go func() {
		defer close(w.done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			for _, c := range calls {
				if _, rec, err := confirmations.RelayConfirmation(ctx, c.SessionID()); err == nil && rec.Attempt != nil {
					w.mu.Lock()
					if w.attempts[c.SessionID()] == nil {
						w.attempts[c.SessionID()] = map[uint64]sessionstore.RelayAttempt{}
					}
					w.attempts[c.SessionID()][rec.Attempt.Generation] = *rec.Attempt
					w.mu.Unlock()
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return w
}

func (w *relayAttemptWatch) stop() { w.cancel(); <-w.done }

// sendMedia runs c.SendMedia and saves its result for the evidence.
func (w *relayAttemptWatch) sendMedia(ctx context.Context, c *Call, d time.Duration) error {
	w.mu.Lock()
	w.sends[c] = "running"
	w.mu.Unlock()
	err := c.SendMedia(ctx, d)
	w.mu.Lock()
	w.sends[c] = fmt.Sprint(err)
	w.mu.Unlock()
	return err
}

// evidence names, per call, what the caller sent and received and what
// control saved: the caller's media send result, its packets per track after
// the attempt's observed switch commit, its selected relay address and
// generation, and each attempt's phase, required and delivered MIDs. Times
// are relative to the drain start.
func (w *relayAttemptWatch) evidence(calls []*Call, start time.Time) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var b strings.Builder
	for _, c := range calls {
		c.relayRestartMu.Lock()
		generation := c.relayGeneration
		c.relayRestartMu.Unlock()
		c.mu.Lock()
		restartErr := c.restartErr
		c.mu.Unlock()
		fmt.Fprintf(&b, "call=%s caller_generation=%d selected=%s restart_err=%v send=%s\n", c.SessionID(), generation, c.selectedRemoteAddr(), restartErr, w.sends[c])
		var commit time.Time
		for g := uint64(0); g <= generation+2; g++ {
			t, ok := w.attempts[c.SessionID()][g]
			if !ok {
				continue
			}
			if commit.IsZero() {
				commit = t.CommitAt
			}
			at := "none"
			if !t.CommitAt.IsZero() {
				at = t.CommitAt.Sub(start).Round(time.Millisecond).String()
			}
			fmt.Fprintf(&b, "  attempt generation=%d slot=%s phase=%s kind=%s commit_at=+%s required=%v delivered=%v decoded=%v\n", t.Generation, t.Slot, t.Phase, t.Kind, at, t.Required, t.Delivered, t.Decoded)
		}
		for _, track := range c.rec.report().Tracks {
			after := 0
			for _, arrival := range track.Arrivals {
				if !commit.IsZero() && c.rec.start.Add(arrival).After(commit) {
					after++
				}
			}
			fmt.Fprintf(&b, "  caller track=%s packets=%d after_commit=%d last_arrival=+%s\n", track.Kind, track.Packets, after, c.rec.start.Add(track.LastArrival).Sub(start).Round(time.Millisecond))
		}
	}
	return b.String()
}
