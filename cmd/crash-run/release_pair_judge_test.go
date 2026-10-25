package main

import (
	"testing"
	"time"

	"github.com/relaishq/relais/pkg/callharness"
	"github.com/stretchr/testify/require"
)

var pairStart = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func trustedPairVerdict() callharness.MetricVerdict { return callharness.MetricVerdict{Trusted: true} }

// pairMeasurement is a conclusive, lossless planned-move content record.
func pairMeasurement() callharness.EventMeasurement {
	m := callharness.EventMeasurement{SettleWindow: 500 * time.Millisecond}
	m.AudioLoss, m.VideoLoss = trustedPairVerdict(), trustedPairVerdict()
	m.Audio.Verdict, m.Video.Verdict = trustedPairVerdict(), trustedPairVerdict()
	m.ResumeVerdict, m.FirstContentVerdict, m.FirstNewContentVerdict, m.FirstLiveVerdict = trustedPairVerdict(), trustedPairVerdict(), trustedPairVerdict(), trustedPairVerdict()
	return m
}

func pairMove(at time.Duration, gap time.Duration) callharness.MoveReport {
	m := callharness.MoveReport{Kind: "move", From: 0, To: 1, Start: at, End: at + 20*time.Millisecond, Measurement: pairMeasurement()}
	// The yardstick's loss window: media resumes 10 ms after the move ends.
	m.Measurement.MediaResumedAt = at + 30*time.Millisecond
	m.Measurement.WindowStart, m.Measurement.WindowEnd = at-250*time.Millisecond, at+280*time.Millisecond
	for _, kind := range []string{"audio", "video"} {
		m.Tracks = append(m.Tracks, callharness.MoveTrackReport{Kind: kind, Gap: gap, BaselineGapP95: 21 * time.Millisecond, BaselineGapSamples: 40, PacketsAfter: 100})
	}
	return m
}

func cleanPairReport() *callharness.Report {
	r := &callharness.Report{StartedAt: pairStart, OfferAnswerExchanges: 1, RemoteAddr: "127.0.0.1:4000", ConnectionStates: []callharness.StateChange{{State: "connecting"}, {State: "connected"}}}
	r.Consent.ResponsesAfter = 3
	r.SentAudio.Frames, r.SentVideo.Frames = 3000, 1800
	r.Recording.Delivery = &callharness.WholeCallDelivery{Audio: callharness.ContentDelivery{Sent: 2990, Returned: 2990, Unsettled: 10}, Video: callharness.ContentDelivery{Sent: 1795, Returned: 1795, Unsettled: 5}}
	r.Tracks = []callharness.TrackReport{
		{Kind: "audio", Packets: 3000, LastArrival: 59*time.Second + 980*time.Millisecond, MediaGapsRecorded: true},
		{Kind: "video", Packets: 5000, LastArrival: 59*time.Second + 980*time.Millisecond, MediaGapsRecorded: true, Video: &callharness.VideoReport{Frames: 1800, OnlineDecode: callharness.OnlineDecodeReport{Every: 30, Sampled: 60, Decoded: 60, UniqueDecoded: 60}}},
	}
	r.Moves = []callharness.MoveReport{pairMove(10*time.Second, 25*time.Millisecond), pairMove(40*time.Second, 30*time.Millisecond)}
	return r
}

func cleanPairEvidence() pairEvidence {
	return pairEvidence{
		Reports: []*callharness.Report{cleanPairReport()}, ExpectedCalls: 1, RelayMedia: "127.0.0.1:4000",
		RelayWindows:  []pairWindow{{Kind: "relay", Start: pairStart.Add(20 * time.Second), End: pairStart.Add(20*time.Second + 90*time.Millisecond)}},
		Phases:        []pairWindow{{Kind: "upgrade", Start: pairStart, End: pairStart.Add(30 * time.Second)}, {Kind: "rollback", Start: pairStart.Add(30 * time.Second), End: pairStart.Add(time.Minute)}},
		ObservedUntil: pairStart.Add(time.Minute),
		Observations:  []pairObservation{{Label: "boot", Calls: 1}, {Label: "final", Calls: 1}},
	}
}

func requirePairOutcome(t *testing.T, e pairEvidence, outcome, reason string) pairJudgement {
	t.Helper()
	j := judgeReleasePair(e)
	require.Equal(t, outcome, j.Outcome, j.Summary())
	if reason != "" {
		require.Contains(t, j.Summary(), reason)
	}
	return j
}

func TestJudgeReleasePairPassesCleanEvidence(t *testing.T) {
	e := cleanPairEvidence()
	// A relay-window gap below the 1 s handover limit is accepted.
	e.Reports[0].Tracks[0].MediaGaps = []callharness.MediaGap{{EndedAt: 20*time.Second + 300*time.Millisecond, Duration: 400 * time.Millisecond}}
	// A gap between the last packet before a move and the first after it is the move metric's.
	e.Reports[0].Tracks[1].MediaGaps = []callharness.MediaGap{{EndedAt: 10*time.Second + 25*time.Millisecond, Duration: 200 * time.Millisecond}}
	// Relay-handover loss is reported, as the relay buffer is no durable log.
	d := e.Reports[0].Recording.Delivery
	d.Audio.Returned, d.Audio.Lost, d.Audio.LostAt = d.Audio.Returned-2, 2, []time.Duration{20 * time.Second, 20*time.Second + 20*time.Millisecond}
	j := requirePairOutcome(t, e, "pass", "")
	require.Equal(t, 400*time.Millisecond, j.RelayGapMax)
	require.Equal(t, 2, j.Phases[0].RelayLostAudio)
	require.Equal(t, 25*time.Millisecond, j.MoveGaps[0].P95Gap)
	require.Equal(t, "rollback", j.MoveGaps[2].Phase)
	require.Equal(t, 30*time.Millisecond, j.MoveGaps[2].P95Gap)
}

func TestJudgeReleasePairFailsTrustedFaults(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*pairEvidence)
		reason string
	}{
		"decryption":  {func(e *pairEvidence) { e.Reports[0].DecryptionFailures.AuthTag = 1 }, "decryption failures"},
		"ICE restart": {func(e *pairEvidence) { e.Reports[0].ICERestarts = 1 }, "ICE restarts=1"},
		"left connected": {func(e *pairEvidence) {
			e.Reports[0].ConnectionStates = append(e.Reports[0].ConnectionStates, callharness.StateChange{State: "disconnected"})
		}, "left connected"},
		"wrong remote":  {func(e *pairEvidence) { e.Reports[0].RemoteAddr = "127.0.0.1:5000" }, "is not the relay"},
		"no consent":    {func(e *pairEvidence) { e.Reports[0].Consent.ResponsesAfter = 0 }, "consent"},
		"media stopped": {func(e *pairEvidence) { e.Reports[0].Tracks[0].LastArrival = 50 * time.Second }, "media stopped"},
		"missing video": {func(e *pairEvidence) { e.Reports[0].Tracks = e.Reports[0].Tracks[:1] }, "no echoed video"},
		"gap outside": {func(e *pairEvidence) {
			e.Reports[0].Tracks[0].MediaGaps = []callharness.MediaGap{{EndedAt: 30 * time.Second, Duration: 200 * time.Millisecond}}
		}, "outside every scheduled rollout event"},
		// The move ends at 10.020 s; packets at 10.030 s and 10.330 s bound a
		// gap the move metric never measures. It is not exempt.
		"gap after move end": {func(e *pairEvidence) {
			e.Reports[0].Tracks[0].MediaGaps = []callharness.MediaGap{{EndedAt: 10*time.Second + 330*time.Millisecond, Duration: 300 * time.Millisecond}}
		}, "outside every scheduled rollout event"},
		"gap before move start": {func(e *pairEvidence) {
			e.Reports[0].Tracks[0].MediaGaps = []callharness.MediaGap{{EndedAt: 9*time.Second + 990*time.Millisecond, Duration: 300 * time.Millisecond}}
		}, "outside every scheduled rollout event"},
		"gap after relay window": {func(e *pairEvidence) {
			e.Reports[0].Tracks[0].MediaGaps = []callharness.MediaGap{{EndedAt: 20*time.Second + 420*time.Millisecond, Duration: 300 * time.Millisecond}}
		}, "outside every scheduled rollout event"},
		"long relay gap": {func(e *pairEvidence) {
			e.Reports[0].Tracks[0].MediaGaps = []callharness.MediaGap{{EndedAt: 21 * time.Second, Duration: time.Second}}
		}, "relay handover gap"},
		"move p95":           {func(e *pairEvidence) { e.Reports[0].Moves[1] = pairMove(40*time.Second, 200*time.Millisecond) }, "rollback: audio planned-move p95"},
		"move excess":        {func(e *pairEvidence) { e.Reports[0].Moves[1] = pairMove(40*time.Second, 80*time.Millisecond) }, "rollback: audio planned-move p95"},
		"move error":         {func(e *pairEvidence) { e.Reports[0].Moves[0].Error = "export failed" }, "export failed"},
		"move lost":          {func(e *pairEvidence) { e.Reports[0].Moves[0].Lost = true }, "lost=true"},
		"unplanned move":     {func(e *pairEvidence) { e.Reports[0].Moves[0].Kind = "takeover" }, "unplanned takeover"},
		"no packets after":   {func(e *pairEvidence) { e.Reports[0].Moves[0].Tracks[0].PacketsAfter = 0 }, "no audio packets after"},
		"move audio lost":    {func(e *pairEvidence) { e.Reports[0].Moves[0].Measurement.AudioLoss.Failed = true }, "audio content lost"},
		"move audio missing": {func(e *pairEvidence) { e.Reports[0].Moves[0].Measurement.LostAudio = 20 * time.Millisecond }, "audio content lost"},
		"move video missing": {func(e *pairEvidence) { e.Reports[0].Moves[1].Measurement.LostVideoFrames = 1 }, "video content lost"},
		"content recovery":   {func(e *pairEvidence) { e.Reports[0].Moves[0].Measurement.FirstLiveVerdict.Failed = true }, "content recovery failed"},
		"unmatched audio":    {func(e *pairEvidence) { e.Reports[0].Tracks[0].UnmatchedPayloads = 1 }, "unmatched audio payloads"},
		"unmatched video":    {func(e *pairEvidence) { e.Reports[0].Tracks[1].Video.UnmatchedFrames = 1 }, "invalid video content"},
		"online decode":      {func(e *pairEvidence) { e.Reports[0].Tracks[1].Video.OnlineDecode.Errors = 1 }, "online video decode errors"},
		"full decode": {func(e *pairEvidence) {
			v := e.Reports[0].Tracks[1].Video
			v.OnlineDecode = callharness.OnlineDecodeReport{}
			v.FullDecode.FramesIn, v.FullDecode.FramesDecoded = 100, 99
		}, "full video decode 99/100"},
		"loss outside events": {func(e *pairEvidence) {
			d := e.Reports[0].Recording.Delivery
			d.Audio.Returned, d.Audio.Lost, d.Audio.LostAt = d.Audio.Returned-1, 1, []time.Duration{30 * time.Second}
		}, "lost outside every scheduled rollout event"},
		// The move at 10 s is measured through 10.280 s; a unit sent at
		// 10.400 s is outside it, so whole-call accounting must judge it.
		"loss after move window": {func(e *pairEvidence) {
			d := e.Reports[0].Recording.Delivery
			d.Audio.Returned, d.Audio.Lost, d.Audio.LostAt = d.Audio.Returned-1, 1, []time.Duration{10*time.Second + 400*time.Millisecond}
		}, "lost outside every scheduled rollout event"},
		// The relay window ends at 20.090 s, so its loss window ends at 20.340 s.
		"loss after relay window": {func(e *pairEvidence) {
			d := e.Reports[0].Recording.Delivery
			d.Video.Returned, d.Video.Lost, d.Video.LostAt = d.Video.Returned-1, 1, []time.Duration{20*time.Second + 400*time.Millisecond}
		}, "lost outside every scheduled rollout event"},
		"lost overflow": {func(e *pairEvidence) {
			d := e.Reports[0].Recording.Delivery
			d.Video.Returned, d.Video.Lost, d.Video.LostOverflow = d.Video.Returned-1, 1, 1
		}, "inventory overflow"},
		"lost call":       {func(e *pairEvidence) { e.Observations[1].LostCount = 1 }, "lost calls"},
		"crash takeover":  {func(e *pairEvidence) { e.Observations[1].CrashTakeovers = 1 }, "crash takeovers"},
		"missing call":    {func(e *pairEvidence) { e.Observations[1].Calls = 0 }, "owns 0 of 1"},
		"unexpected exit": {func(e *pairEvidence) { e.UnexpectedExits = []string{"signal (0.3.0, exit status 1)"} }, "signal (0.3.0, exit status 1) exited unexpectedly"},
		// An unknown elsewhere must never hide a trusted failure.
		"fail beats unknown": {func(e *pairEvidence) {
			e.Reports[0].ICERestarts = 1
			e.Observations[0].Err = "control down"
		}, "ICE restarts=1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := cleanPairEvidence()
			tc.mutate(&e)
			requirePairOutcome(t, e, "fail", tc.reason)
		})
	}
}

// Nineteen good upgrade moves would hide one failing rollback move in a pooled
// p95; each phase is gated on its own.
func TestJudgeReleasePairGatesEachPhaseSeparately(t *testing.T) {
	e := cleanPairEvidence()
	r := e.Reports[0]
	r.Moves = nil
	for i := range 19 {
		r.Moves = append(r.Moves, pairMove(time.Second+time.Duration(i)*time.Second, 25*time.Millisecond))
	}
	r.Moves = append(r.Moves, pairMove(40*time.Second, 200*time.Millisecond))
	var pooled []time.Duration
	for _, m := range r.Moves {
		pooled = append(pooled, m.Tracks[0].Gap)
	}
	require.Equal(t, 25*time.Millisecond, pairP95(pooled), "a whole-run p95 would pass")
	requirePairOutcome(t, e, "fail", "rollback: audio planned-move p95 gap 200ms")
}

func TestJudgeReleasePairTreatsMissingEvidenceAsInconclusive(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*pairEvidence)
		reason string
	}{
		"no moves":          {func(e *pairEvidence) { e.Reports[0].Moves = nil }, "upgrade: no planned-move evidence"},
		"no rollback moves": {func(e *pairEvidence) { e.Reports[0].Moves = e.Reports[0].Moves[:1] }, "rollback: no planned-move evidence"},
		"no baselines":      {func(e *pairEvidence) { e.Reports[0].Moves[0].Tracks[0].BaselineGapSamples = 3 }, "upgrade: audio planned-move coverage 0/1"},
		"no measurement":    {func(e *pairEvidence) { e.Reports[0].Moves[0].Measurement = callharness.EventMeasurement{} }, "event measurement not available"},
		"untrusted loss":    {func(e *pairEvidence) { e.Reports[0].Moves[1].Measurement.VideoLoss.Trusted = false }, "video loss inconclusive"},
		"untrusted fresh":   {func(e *pairEvidence) { e.Reports[0].Moves[1].Measurement.Audio.Verdict.Trusted = false }, "rollback: move measurement coverage 0/1"},
		"decode sparse":     {func(e *pairEvidence) { e.Reports[0].Tracks[1].Video.OnlineDecode.UniqueDecoded = 1 }, "online video decode inconclusive"},
		"decode absent": {func(e *pairEvidence) {
			e.Reports[0].Tracks[1].Video.OnlineDecode = callharness.OnlineDecodeReport{}
		}, "video decode unavailable"},
		"no delivery": {func(e *pairEvidence) { e.Reports[0].Recording.Delivery = nil }, "whole-call delivery unavailable"},
		"delivery accounting": {func(e *pairEvidence) {
			e.Reports[0].Recording.Delivery.Audio.Unsettled = 0
		}, "audio delivery accounting incomplete"},
		"gap inventory":     {func(e *pairEvidence) { e.Reports[0].Tracks[0].MediaGapOverflow = 1 }, "gap inventory incomplete"},
		"gaps not recorded": {func(e *pairEvidence) { e.Reports[0].Tracks[1].MediaGapsRecorded = false }, "gap inventory incomplete"},
		"missing report":    {func(e *pairEvidence) { e.ExpectedCalls = 2; e.Observations = nil }, "caller reports 1 of 2"},
		"no relay handover": {func(e *pairEvidence) { e.RelayWindows = nil }, "no relay standby handover"},
		"no phases":         {func(e *pairEvidence) { e.Phases = nil }, "no rollout phase windows"},
		"move outside":      {func(e *pairEvidence) { e.Phases[1].End = pairStart.Add(35 * time.Second) }, "outside every rollout phase"},
		"status error":      {func(e *pairEvidence) { e.Observations[0] = pairObservation{Label: "boot", Err: "refused"} }, "status unavailable"},
		"no observations":   {func(e *pairEvidence) { e.Observations = nil }, "no control-plane ownership"},
	} {
		t.Run(name, func(t *testing.T) {
			e := cleanPairEvidence()
			tc.mutate(&e)
			requirePairOutcome(t, e, "inconclusive", tc.reason)
		})
	}
}
