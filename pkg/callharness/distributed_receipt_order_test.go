package callharness

import (
	"cmp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Reversed-receipt scenario: 50Hz identified audio, a 25ms
// clock probe and a brief media outage. Receipt delivery is independent of
// event order, and all assertions use the production report snapshot path.
func receiptOrderCall(t *testing.T, takeover bool) (*recorder, *Call) {
	t.Helper()
	r := newRecorder()
	r.start = time.Now().Add(-30 * time.Second)
	r.hungUpAt = 30 * time.Second
	r.recording.History = 180 * time.Second
	outage, resume := 12*time.Second, 12040*time.Millisecond
	if takeover {
		outage, resume = 5*time.Second, 5600*time.Millisecond
	}
	for i, at := 0, time.Duration(0); at < r.hungUpAt; i, at = i+1, at+20*time.Millisecond {
		returned := at + 30*time.Millisecond + densityJitter(i)
		if at >= outage && at < resume {
			returned = resume + (at-outage)/10
		}
		key := "a" + strconv.Itoa(i)
		r.sentAudioUnits[key] = contentUnit{at: at, returnedAt: returned, duration: 20 * time.Millisecond, written: true}
		r.audioSends = append(r.audioSends, evidenceRef{at: at, audio: key})
		r.audioReturns = append(r.audioReturns, evidenceRef{at: returned, audio: key})
	}
	slices.SortFunc(r.audioReturns, func(a, b evidenceRef) int { return cmp.Compare(a.at, b.at) })
	audio := &trackRecord{kind: kindAudio}
	seq := uint16(0)
	jumped := false
	for at := 30 * time.Millisecond; at < r.hungUpAt; at += 20 * time.Millisecond {
		if at > outage && at < resume {
			continue
		}
		if takeover && !jumped && at >= resume {
			seq += 8192
			jumped = true
		}
		audio.arrivals = append(audio.arrivals, at)
		audio.headers = append(audio.headers, rtpMark{seq: seq})
		seq++
	}
	r.tracks = []*trackRecord{audio}
	clock, err := NewClockTracker(0)
	require.NoError(t, err)
	mid := r.start.Round(0).Add(12500 * time.Microsecond)
	require.NoError(t, clock.Add(ClockProbe{CallerSend: r.start, CallerReceive: r.start.Add(25 * time.Millisecond), ServerReceive: mid, ServerSend: mid}))
	b := OffsetInterval{Lower: -time.Millisecond, Upper: time.Millisecond}
	r.distributed = &DistributedOptions{RunID: "run", Clock: clock, ControlOffset: b, WorkerOffset: b}
	return r, &Call{rec: r, remoteUfrag: "session"}
}

func TestDistributedReversedReceipts(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		kind := "move"
		end := 5030 * time.Millisecond
		if takeover {
			kind, end = "takeover", 5500*time.Millisecond
		}
		t.Run(kind, func(t *testing.T) {
			for _, reversed := range []bool{false, true} {
				r, c := receiptOrderCall(t, takeover)
				order := []string{"earlier", "later"}
				if reversed {
					slices.Reverse(order)
				}
				for _, op := range order {
					if op == "earlier" {
						recordDenseEvent(t, r, c, op, kind, 5*time.Second, end)
					} else {
						recordDenseEvent(t, r, c, op, "move", 12*time.Second, 12030*time.Millisecond)
					}
				}
				r.hungUp = true
				rep := r.report()
				require.Len(t, rep.Moves, 2)
				for _, m := range rep.Moves {
					t.Logf("reversed=%t op=%s resume=%s window=%s", reversed, m.Identity.OperationID, m.Measurement.ResumeVerdict.Summary(), m.Distributed.WindowOutcome)
					require.True(t, m.Measurement.ResumeVerdict.Trusted, "%s reversed=%t", m.Identity.OperationID, reversed)
					require.False(t, m.Measurement.ResumeVerdict.Failed, "%s reversed=%t: %v", m.Identity.OperationID, reversed, m.Measurement.ResumeVerdict.Reasons)
					require.False(t, m.Measurement.Failed, "%s reversed=%t: %v", m.Identity.OperationID, reversed, m.Measurement.Reasons)
				}
				require.NotEqual(t, "fail", rep.Distributed.Outcome)
			}
		})
	}
}

func TestDistributedUnmappedReceiptDoesNotTruncateMappedEvent(t *testing.T) {
	for _, armed := range []time.Duration{2 * time.Second, 8 * time.Second} {
		t.Run(armed.String(), func(t *testing.T) {
			r, c := receiptOrderCall(t, false)
			recordDenseEvent(t, r, c, "earlier", "move", 5*time.Second, 5030*time.Millisecond)
			if armed == 2*time.Second {
				// An earlier arm's receipt arrives last, after both mapped events.
				recordDenseEvent(t, r, c, "later", "move", 12*time.Second, 12030*time.Millisecond)
			}
			id := OperationIdentity{RunID: "run", OperationID: "unmapped", SessionID: "session", Worker: "0", Epoch: 1}
			r.armed[id] = r.start.Add(armed)
			require.NoError(t, c.RecordDistributedEvent(DistributedEvent{Identity: id, Kind: "move", Uncertain: true}))
			if armed == 8*time.Second {
				// The unknown intervening event cannot truncate the earlier one.
				recordDenseEvent(t, r, c, "later", "move", 12*time.Second, 12030*time.Millisecond)
			}
			r.hungUp = true
			rep := r.report()
			require.Len(t, rep.Moves, 3)
			for _, m := range rep.Moves {
				if m.Identity.OperationID == "unmapped" {
					continue
				}
				if armed == 2*time.Second || m.Identity.OperationID == "earlier" {
					require.True(t, m.Measurement.ResumeVerdict.Trusted)
				}
				require.False(t, m.Measurement.ResumeVerdict.Failed, "%s: %v", m.Identity.OperationID, m.Measurement.ResumeVerdict.Reasons)
				if m.Identity.OperationID == "earlier" {
					require.Greater(t, m.Measurement.Audio.EvaluationEnd, 9*time.Second, "the unmapped arm must not truncate the freshness window")
				}
			}
		})
	}
}

func TestDistributedInvalidReceiptOrderingDoesNotTruncateMappedEvent(t *testing.T) {
	r, c := receiptOrderCall(t, false)
	recordDenseEvent(t, r, c, "earlier", "move", 5*time.Second, 5030*time.Millisecond)
	recordDenseEvent(t, r, c, "invalid", "move", 8*time.Second, 8030*time.Millisecond)
	// Its timestamps can be transformed, but precede the arm acknowledgment.
	// The real judgment therefore considers this receipt unmapped.
	r.moves[1].armedAt = r.start.Add(9 * time.Second)
	r.hungUp = true
	rep := r.report()
	require.Len(t, rep.Moves, 2)
	require.False(t, rep.Moves[1].Distributed.Mapped)
	require.Greater(t, rep.Moves[0].Measurement.Audio.EvaluationEnd, 9*time.Second)
	require.True(t, rep.Moves[0].Measurement.ResumeVerdict.Trusted)
	require.False(t, rep.Moves[0].Measurement.ResumeVerdict.Failed)
}

// #115: maintenance saves a later event before an earlier event's receipt
// arrives. The earlier window still ends at the saved event's mapped start,
// exactly as it does when both receipts are pending.
func TestDistributedCompactedLaterEventEndsEarlierWindow(t *testing.T) {
	measure := func(compactLater bool) (earlier, later MoveReport) {
		r, c := receiptOrderCall(t, false)
		r.recording.History = 20 * time.Second
		recordDenseEvent(t, r, c, "later", "move", 12*time.Second, 12030*time.Millisecond)
		if compactLater {
			r.compactRecording(29 * time.Second)
			require.Len(t, r.savedMoves, 1)
			require.Empty(t, r.moves)
		}
		recordDenseEvent(t, r, c, "earlier", "move", 10500*time.Millisecond, 10530*time.Millisecond)
		r.hungUp = true
		rep := r.report()
		require.Len(t, rep.Moves, 2)
		for _, m := range rep.Moves {
			require.True(t, m.Distributed.Mapped, m.Identity.OperationID)
			if m.Identity.OperationID == "earlier" {
				earlier = m
			} else {
				later = m
			}
		}
		return earlier, later
	}
	pending, _ := measure(false)
	earlier, later := measure(true)
	require.Less(t, later.Start, earlier.Start+FreshnessPolicy{}.EvaluationWindow(), "the saved event starts inside the earlier evaluation window")
	require.Equal(t, later.Start, earlier.Measurement.Audio.EvaluationEnd)
	require.Equal(t, pending.Measurement.Audio, earlier.Measurement.Audio)
	require.Equal(t, pending.Measurement.ResumeVerdict, earlier.Measurement.ResumeVerdict)
	require.Equal(t, pending.Distributed.Durations, earlier.Distributed.Durations)
}
