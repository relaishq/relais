package callharness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClockAcceptsSixteenThousandProbesThenFailsClosed(t *testing.T) {
	tracker, err := NewClockTracker(100)
	require.NoError(t, err)
	at := time.Now()
	probe := func(i int) ClockProbe {
		return probeAt(at.Add(time.Duration(i)*250*time.Millisecond), 0, time.Millisecond)
	}
	// Preload directly: adding each probe compares it with every earlier one.
	const limit = 16384
	for i := range limit - 1 {
		tracker.probes = append(tracker.probes, probe(i))
	}
	require.NoError(t, tracker.Add(probe(limit-1)))
	require.Len(t, tracker.Probes(), limit)
	_, err = tracker.Map(at.Add(time.Hour), OffsetInterval{})
	require.NoError(t, err)

	require.ErrorContains(t, tracker.Add(probe(limit)), "clock probe capacity exceeded")
	require.ErrorContains(t, tracker.Err(), "clock probe capacity exceeded")
	_, err = tracker.Map(at.Add(time.Hour), OffsetInterval{})
	require.ErrorContains(t, err, "clock probe capacity exceeded", "earlier events lose clock trust too")
	require.Len(t, tracker.Probes(), limit)
}

// burstProbes is a background cadence plus an event burst, with varied RTTs so
// the binding probes are not simply the nearest ones.
func burstProbes(start time.Time) []ClockProbe {
	var sends []time.Duration
	for at := -60 * time.Second; at <= 20*time.Second; at += 10 * time.Second {
		sends = append(sends, at)
	}
	for at := 2900 * time.Millisecond; at <= 4350*time.Millisecond; at += 250 * time.Millisecond {
		sends = append(sends, at)
	}
	slices.Sort(sends)
	probes := []ClockProbe{}
	for i, at := range sends {
		rtt := 800*time.Microsecond + time.Duration(i*37%7)*150*time.Microsecond
		skew := 2*time.Millisecond + at/100000
		probes = append(probes, probeAt(start.Add(at), skew, rtt))
	}
	return probes
}

// Identical probes must give byte-identical window classifications. The
// digest was recorded on main 0d50c6b, before event bursts and the 16384 cap.
func TestDistributedClassificationIdenticalForIdenticalProbes(t *testing.T) {
	measure := func() []byte {
		r := newRecorder()
		r.start = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
		r.hungUpAt = 12 * time.Second
		clock, err := NewClockTracker(100)
		require.NoError(t, err)
		for _, p := range burstProbes(r.start) {
			require.NoError(t, clock.Add(p))
		}
		r.distributed = &DistributedOptions{RunID: "run", Clock: clock, ControlOffset: OffsetInterval{Lower: -2 * time.Millisecond, Upper: 2 * time.Millisecond}, WorkerOffset: OffsetInterval{Lower: -3 * time.Millisecond, Upper: 3 * time.Millisecond}}
		move := DistributedEvent{Identity: OperationIdentity{RunID: "run", OperationID: "move", SessionID: "session", Worker: "0", Epoch: 1}, Kind: "move", Start: r.start.Add(3050 * time.Millisecond), End: r.start.Add(3250 * time.Millisecond)}
		r.moves = []moveRecord{{kind: "move", start: move.Start, end: move.End, event: &move, armedAt: r.start.Add(2900 * time.Millisecond), receivedAt: r.start.Add(3600 * time.Millisecond)}}
		arrivals := steadyArrivals()
		r.tracks = []*trackRecord{{kind: kindAudio, arrivals: arrivals, headers: make([]rtpMark, len(arrivals))}}
		for i := 0; i < 400; i++ {
			at := time.Duration(i) * 20 * time.Millisecond
			r.sentAudioUnits[fmt.Sprint(i)] = contentUnit{at: at, returnedAt: at + time.Millisecond, written: true, duration: 20 * time.Millisecond}
		}
		raw, err := json.Marshal(r.measureMoves(r.hungUpAt, 1))
		require.NoError(t, err)
		return raw
	}
	first := measure()
	require.Equal(t, first, measure())
	var moves []MoveReport
	require.NoError(t, json.Unmarshal(first, &moves))
	require.True(t, moves[0].Distributed.Mapped, moves[0].Distributed.Causes)
	require.Equal(t, "pass", moves[0].Distributed.Outcome, moves[0].Distributed.Causes)
	start := moves[0].Distributed.Timing.Start
	require.Equal(t, 7, start.LowerProbe, "a low-RTT burst probe binds, not the nearest one")
	sum := sha256.Sum256(first)
	require.Equal(t, "f40f27b28d09ed88d34bb1d70c42609bc335f9988cc6d490f191d6a78b91b27c", hex.EncodeToString(sum[:]))
}
