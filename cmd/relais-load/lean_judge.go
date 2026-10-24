package main

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/capacity"
	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

func judgeLean(m leanManifest) leanReport {
	out := leanReport{Policy: "lean-distributed-v1", RunID: m.RunID, leanVerdict: leanVerdict{Outcome: "pass"}, SurvivorSafeCalls: 1, Schedule: leanPlan(), Evidence: m}
	out.CloudQualification = leanVerdict{Outcome: "pass"}
	if m.Topology == "local" {
		out.CloudQualification = leanVerdict{Outcome: "NOT-RUN", Causes: []string{"local topology declares no cloud qualification; cloud ENA behavior was not exercised"}}
	}
	if err := validateLeanManifest(m); err != nil {
		out.unknown(err.Error())
		out.CloudQualification = leanVerdict{Outcome: "NOT-RUN", Causes: []string{"invalid manifest"}}
		out.Cost = leanCost{Status: "NOT-RUN(invalid manifest)"}
		return out
	}
	var trials []loadgen.RunReport
	counts := map[string]int{}
	identities := map[callharness.OperationIdentity]bool{}
	sessions := map[string]bool{}
	for index, p := range m.Phases {
		v := judgeLeanPhase(m, p)
		if index > 0 {
			previous := m.Phases[index-1].Restored
			for slot, w := range p.Before.Workers {
				if slot >= len(previous.Workers) || w.Name != previous.Workers[slot].Name || w.Generation != previous.Workers[slot].Generation {
					v.unknown("next phase lacks restored worker-generation continuity")
				}
			}
			if p.Before.At.Before(m.Phases[index-1].CompletedAt) {
				v.unknown("serial phase observations overlap or run backward")
			}
		}
		for _, id := range leanSessions(p.Before) {
			if sessions[id] {
				v.unknown("lean phases require fresh session identities across process generations")
			}
			sessions[id] = true
		}
		counts[p.Kind]++
		if p.Kind != "boundary" {
			trials = append(trials, p.Run)
		}
		for _, e := range p.Run.Events {
			for _, id := range e.Identities {
				if identities[id] {
					v.unknown("duplicate operation identity across phases")
				}
				identities[id] = true
			}
		}
		out.Phases = append(out.Phases, v)
		out.merge(p.ID+": ", v.leanVerdict)
	}
	if counts["move"] < 20 || counts["kill"] < 10 || counts["drain"] < 2 || counts["boundary"] != 1 {
		out.unknown("need >=20 move, >=10 takeover, >=2 drain phases and exactly one admission boundary")
	}
	out.Schedule.Moves, out.Schedule.Takeovers, out.Schedule.Drains = counts["move"], counts["kill"], counts["drain"]
	out.Schedule.ExpectedSeconds = out.Schedule.SetupCleanupSeconds
	for _, p := range m.Phases {
		budget := max(p.Run.Duration, 30*time.Second)
		if p.Kind == "boundary" {
			budget = max(budget, 120*time.Second)
		}
		if p.Kind == "kill" || p.Kind == "drain" {
			budget = max(budget, time.Duration(out.Schedule.FaultTrialSeconds)*time.Second)
		}
		out.Schedule.ExpectedSeconds += int(math.Ceil(budget.Seconds()))
	}
	if out.Schedule.ExpectedSeconds > 3600 {
		out.Schedule.Note += " Submitted schedule exceeds about 60 minutes; use the minimum 20 moves, 10 takeovers, two drains and one boundary before budgeting extra trials."
	}
	pooled := loadgen.PoolDistributedReports(trials)
	out.Trials = pooled.Distributed
	judgeLeanStallTrials(trials, &out.leanVerdict)
	if pooled.Distributed == nil {
		out.unknown("no pooled distributed trial evidence")
	} else if pooled.Distributed.Outcome == "fail" {
		out.fail("distributed trials: " + strings.Join(pooled.Distributed.Causes, "; "))
	} else if pooled.Distributed.Outcome != "pass" {
		out.unknown("distributed trials: " + strings.Join(pooled.Distributed.Causes, "; "))
	}
	for _, row := range pooled.Yardstick {
		if row.Outcome == "fail" {
			out.fail(row.Scenario + " " + row.Metric)
		} else if row.Outcome != "pass" {
			out.unknown(row.Scenario + " " + row.Metric + " incomplete")
		}
	}
	for _, e := range pooled.Events {
		if e.Recorded != e.Affected {
			out.unknown("pooled affected-call coverage incomplete")
		}
	}
	if !m.Cleanup.Checked || len(m.Cleanup.Errors) > 0 || m.Cleanup.RelayWait < soakRelayWait || m.Cleanup.RelayObservedAt.IsZero() {
		out.unknown("external cleanup observation incomplete")
	}
	for _, key := range m.Cleanup.Keys {
		if key.Error != "" {
			out.unknown("cleanup key retention unavailable: " + key.Key + ": " + key.Error)
		}
	}
	for _, c := range leakCauses(m.Cleanup) {
		out.fail("cleanup: " + c)
	}
	if len(m.Cleanup.StoreKeys) != len(m.Cleanup.Keys) || !m.Cleanup.ScanControls["session_epoch"] || !m.Cleanup.ScanControls["relay-fencing-hash"] {
		out.unknown("cleanup lacks retention inventory or scan positive controls")
	}
	if len(m.Phases) > 0 {
		out.ObservedWallSeconds = m.Phases[len(m.Phases)-1].CompletedAt.Sub(m.Phases[0].Before.At).Seconds()
	}
	if out.ObservedWallSeconds > float64(out.Schedule.GateWindowSeconds) {
		out.unknown("observed schedule exceeded private 120-minute gate window")
	}
	if m.Topology != "local" {
		out.CloudQualification = out.leanVerdict
	}
	out.Cost = calculateLeanCost(m, out)
	return out
}

func leanPoolCheck(p leanPool, calls, ready int) leanVerdict {
	v := leanVerdict{Outcome: "pass"}
	if p.At.IsZero() || p.Stale || !finiteNonnegative(p.AgeSeconds) || p.AgeSeconds > 1 || p.Cap != 2 || len(p.Workers) != 2 {
		v.unknown("missing/fresh pool observation with two workers and cap=2 required")
	}
	names, generations, sessions := map[string]bool{}, map[string]bool{}, map[string]bool{}
	n, r := 0, 0
	for _, w := range p.Workers {
		if w.Name == "" || w.Generation == "" || names[w.Name] || generations[w.Generation] {
			v.unknown("worker identity/generation missing or duplicated")
		}
		names[w.Name], generations[w.Generation] = true, true
		if w.Reservations < 0 {
			v.unknown("negative reservation count")
		}
		if len(w.Sessions)+w.Reservations > 2 {
			v.fail("worker exceeds cap=2 including incoming reservations")
		}
		if w.Ready {
			r++
		}
		if !w.Ready && len(w.Sessions) > 0 {
			v.unknown("non-ready worker still owns calls")
		}
		n += len(w.Sessions)
		for _, s := range w.Sessions {
			if s == "" || sessions[s] {
				v.unknown("empty/duplicate pool session")
			}
			sessions[s] = true
		}
	}
	if n != calls || r != ready {
		v.unknown(fmt.Sprintf("pool has %d calls/%d ready workers; expected %d/%d", n, r, calls, ready))
	}
	for _, w := range p.Workers {
		if w.Reservations != 0 {
			v.unknown("unsettled pool reservations")
		}
	}
	return v
}
func leanSessions(p leanPool) []string {
	var ids []string
	for _, w := range p.Workers {
		ids = append(ids, w.Sessions...)
	}
	slices.Sort(ids)
	return ids
}
func leanWorkerFor(p leanPool, session string) *leanWorker {
	for i := range p.Workers {
		if slices.Contains(p.Workers[i].Sessions, session) {
			return &p.Workers[i]
		}
	}
	return nil
}
func judgeLeanPhase(m leanManifest, p leanPhase) leanPhaseResult {
	v := leanPhaseResult{ID: p.ID, leanVerdict: leanVerdict{Outcome: "pass"}}
	// Lean phases are capped at three minutes. The shared slope judge needs
	// five minutes after warm-up (ten minutes for worker incarnations).
	v.MemoryGate = leanVerdict{Outcome: "not-applicable", Causes: []string{"lean phases are at most three minutes; memory growth needs five minutes after warm-up and ten minutes per worker incarnation; insufficient-data is excluded from the lean verdict, not a memory pass"}}
	expected := 1
	if p.Kind == "boundary" {
		expected = 4
	}
	if p.Run.Callers != expected {
		v.unknown("phase concurrent caller count violates lean policy")
	}
	v.merge("before: ", leanPoolCheck(p.Before, expected, 2))
	ready := 2
	if p.Kind == "kill" || p.Kind == "drain" {
		ready = 1
	}
	v.merge("after: ", leanPoolCheck(p.After, expected, ready))
	v.merge("restored: ", leanPoolCheck(p.Restored, expected, 2))
	if p.CompletedAt.IsZero() || p.CompletedAt.Before(p.Restored.At) {
		v.unknown("phase completion observation missing or before restore")
	}
	if p.After.At.Before(p.Before.At) || p.Restored.At.Before(p.After.At) || p.Restored.At.Sub(p.After.At) > 90*time.Second {
		v.unknown("pool restore/rejoin exceeded 90s budget or observation order invalid")
	}
	if !slices.Equal(leanSessions(p.Before), leanSessions(p.After)) || !slices.Equal(leanSessions(p.Before), leanSessions(p.Restored)) {
		v.fail("phase did not preserve exact accepted sessions")
	}
	if p.Run.StartedAt.IsZero() || len(p.Run.Processes) == 0 {
		v.unknown("missing media window/processes")
	}
	sessions := []string{}
	for _, proc := range p.Run.Processes {
		judgeLeanGenerator(p.Run, proc, &v.leanVerdict)
		for _, msg := range proc.Calls {
			sessions = append(sessions, msg.Session)
			judgeLeanCall(m, p, proc, msg, &v.leanVerdict)
		}
	}
	slices.Sort(sessions)
	if !slices.Equal(sessions, leanSessions(p.Before)) {
		v.unknown("caller reports do not match observed ownership")
	}
	if p.Kind == "boundary" {
		judgeLeanAdmission(p, &v.leanVerdict)
		if len(p.Run.Events) != 0 {
			v.unknown("admission boundary must not run faults")
		}
	} else {
		if len(p.Run.Events) != 1 || len(p.Timings) != 1 {
			v.unknown("recovery phase needs exactly one scheduled event/timing")
		}
		for _, e := range p.Run.Events {
			if e.Event.Kind != p.Kind || e.Affected != 1 || len(e.Identities) != 1 || len(e.Sessions) != 1 || e.Event.After < 3*time.Second || e.Event.After+7*time.Second >= p.Run.Duration {
				v.unknown("invalid scheduled lean event or recovery tail")
			}
			if e.Failed || e.Error != "" {
				v.fail("scheduled event failed: " + e.Error)
			}
			if len(e.Identities) == 1 && len(e.Sessions) == 1 {
				owner := leanWorkerFor(p.Before, e.Sessions[0])
				target := leanWorkerFor(p.After, e.Sessions[0])
				if owner == nil || target == nil || owner.Name != e.Identities[0].Worker || owner.Name == target.Name {
					v.unknown("event source/target does not match pool ownership")
				}
				if p.Kind == "kill" && owner != nil {
					for _, w := range p.Restored.Workers {
						if w.Name == owner.Name && w.Generation == owner.Generation {
							v.unknown("killed worker generation was not replaced")
						}
					}
				}
			}
		}
	}
	if p.Kind == "drain" {
		e := p.DrainEmpty
		if e == nil || e.At.IsZero() || e.Worker == "" {
			v.unknown("missing independent drained-worker empty proof")
		} else {
			if e.LocalSessions != 0 || e.Owners != 0 || e.Reservations != 0 || e.Transfers != 0 {
				v.fail("drained worker not empty")
			}
			if len(p.Run.Events) == 1 && len(p.Run.Events[0].Identities) == 1 && e.Worker != p.Run.Events[0].Identities[0].Worker {
				v.unknown("empty proof names wrong worker")
			}
			matched := false
			for _, proc := range p.Run.Processes {
				for _, msg := range proc.Calls {
					if msg.Call == nil {
						continue
					}
					for _, move := range msg.Call.Moves {
						receipt := move.Receipt
						owner := leanWorkerFor(p.Before, msg.Session)
						if receipt == nil || owner == nil || receipt.Identity.Worker != e.Worker || owner.Name != e.Worker || owner.Generation != e.Generation || e.Generation == "" || receipt.End.IsZero() || e.At.Before(receipt.End) || e.At.After(p.Restored.At) {
							continue
						}
						for _, restored := range p.Restored.Workers {
							matched = matched || (restored.Name == e.Worker && restored.Generation == e.Generation)
						}
					}
				}
			}
			if !matched {
				v.unknown("drain empty proof lacks matching generation or receipt-to-restore time bound")
			}
		}
	}
	judgeLeanTelemetry(m, p, &v)
	judgeLeanAllowances(m, p, &v.leanVerdict)
	return v
}

func judgeLeanAdmission(p leanPhase, v *leanVerdict) {
	a := p.Admission
	if a == nil {
		v.unknown("missing admission-boundary transcript")
		return
	}
	v.merge("admission before: ", leanPoolCheck(a.Before, 4, 2))
	v.merge("admission after: ", leanPoolCheck(a.After, 4, 2))
	if a.Before.At.After(a.After.At) {
		v.unknown("admission observations run backward")
	}
	if a.Status != 503 || a.Code != "at_capacity" || a.RetryAfter != "1" {
		v.fail("fifth call was not cleanly refused with 503 at_capacity and Retry-After=1")
	}
	if !reflect.DeepEqual(a.Before.Workers, a.After.Workers) || !slices.Equal(leanSessions(a.Before), leanSessions(p.Before)) {
		v.fail("refused create changed ownership/reservations or accepted sessions")
	}
	if a.RefusalsAfter < a.RefusalsBefore || a.RefusalsAfter != a.RefusalsBefore+1 {
		v.unknown("admission refusal counter does not confirm one refusal")
	}
}

func judgeLeanGenerator(r loadgen.RunReport, p loadgen.ProcessReport, v *leanVerdict) {
	if !p.Complete || len(p.Calls) != p.ExpectedCalls || p.ExpectedCalls < 1 || p.ExpectedCalls > 4 || p.SamplesDropped > 0 || len(p.Samples) == 0 {
		v.unknown("incomplete caller process or missing generator history")
	}
	// Causes other than the reader's peak lateness/skip causes remain blocking.
	peak := loadgen.Stats{Lateness: loadgen.Lateness{Packets: 1}, IntervalLateness: loadgen.Lateness{P99: p.PeakIntervalP99}}
	for _, s := range p.Samples {
		if s.IntervalPacing.SkippedPercent() > peak.IntervalPacing.SkippedPercent() {
			peak.IntervalPacing = s.IntervalPacing
		}
	}
	for _, c := range p.Causes {
		if !slices.Contains(r.Limits.Causes(peak), c) {
			v.unknown("generator: " + c)
		}
	}
	var end time.Time
	for i, s := range p.Samples {
		if (s.Interval < 900*time.Millisecond || s.Interval > 1100*time.Millisecond) || s.At.IsZero() || (!end.IsZero() && (s.At.Add(-s.Interval).Sub(end) < -time.Millisecond || s.At.Add(-s.Interval).Sub(end) > time.Millisecond)) || !finiteNonnegative(s.CPUPerGOMAXPROCS) || s.IntervalLateness.Packets == 0 || s.IntervalLateness.Max < s.IntervalLateness.P99 {
			v.unknown("missing/invalid one-second generator samples")
		}
		if sampleSaturated(p.Samples, i, r.Limits, r.StartedAt, mediaEnd(r, p)) {
			v.unknown("generator saturation exceeds isolated-slip policy")
		}
		end = s.At
	}
	if len(p.Samples) > 0 && (p.Samples[0].At.After(r.StartedAt.Add(1100*time.Millisecond)) || end.Before(mediaEnd(r, p).Add(-time.Millisecond))) {
		v.unknown("generator samples do not cover media window")
	}
}

func judgeLeanCall(m leanManifest, p leanPhase, proc loadgen.ProcessReport, msg loadgen.Message, v *leanVerdict) {
	c := msg.Call
	if c == nil {
		v.unknown("missing caller report")
		return
	}
	if c.RemoteAddr != m.Relay {
		v.unknown("selected caller media endpoint differs from external pool relay")
	}
	if msg.Error != "" {
		v.unknown("caller execution error: " + msg.Error)
	}
	if c.DecryptionFailures.Total() != 0 || c.ICERestarts != 0 || c.Renegotiations != 0 {
		v.fail("caller decryption failure or reconnect")
	}
	for _, s := range c.ConnectionStates {
		if s.State == "failed" || s.State == "disconnected" {
			v.fail("caller connection " + s.State)
		}
	}
	if c.OfferAnswerExchanges != 1 || c.ConnectedAt <= 0 {
		v.unknown("caller connection setup not proven")
	}
	if c.StartedAt.IsZero() || c.HungUpAt <= 0 || c.StartedAt.Add(c.HungUpAt).Before(p.Run.StartedAt.Add(p.Run.Duration)) {
		v.fail("caller ended before media window")
	}
	for _, track := range c.Tracks {
		if track.UnmatchedPayloads > 0 || track.DuplicatePackets > 0 || (track.Video != nil && (track.Video.UnmatchedFrames > 0 || track.Video.KeyframeDecodeErrors > 0)) {
			v.fail("independent caller content corruption")
		}
	}
	for _, move := range c.Moves {
		if move.Lost || move.LossCause != "" || move.Error != "" || move.Measurement.Failed || (move.Distributed != nil && move.Distributed.WindowOutcome == "fail") || (move.Receipt != nil && move.Receipt.Result.Lost) {
			v.fail("recorded operation lost call or failed")
		}
	}
	judgeLeanLocalContinuity(p, c, v)
	recording := c.Distributed
	if recording == nil || recording.RunID != m.RunID || len(recording.Probes) == 0 || recording.ClockError != "" || len(recording.Missing) > 0 || c.Recording.History < 180*time.Second {
		v.unknown("missing or invalid distributed clock/receipt history")
		return
	}
	if recording.ControlOffset != (callharness.OffsetInterval{Lower: -m.Clock.ControlBound, Upper: m.Clock.ControlBound}) || recording.WorkerOffset != (callharness.OffsetInterval{Lower: -m.Clock.WorkerBound, Upper: m.Clock.WorkerBound}) || recording.DriftPPM != m.Clock.DriftPPM {
		v.unknown("recorded clock bounds differ from independently measured inputs")
	}
	tracker, err := callharness.NewClockTracker(recording.DriftPPM)
	if err != nil {
		v.unknown(err.Error())
		return
	}
	for _, probe := range recording.Probes {
		if err := tracker.Add(probe); err != nil {
			v.unknown("clock probe: " + err.Error())
		}
	}
	before, beforeErr := tracker.Map(p.Before.At, recording.ControlOffset)
	complete, completeErr := tracker.Map(p.CompletedAt, recording.ControlOffset)
	if beforeErr != nil || completeErr != nil || before.Latest.After(p.Run.StartedAt) || complete.Earliest.Before(c.StartedAt.Add(c.HungUpAt)) {
		v.unknown("phase media lifetime is not bracketed across control/caller clock bounds")
	}
	restored, restoreErr := tracker.Map(p.Restored.At, recording.ControlOffset)
	if restoreErr != nil || restored.Latest.After(c.StartedAt.Add(c.HungUpAt)) {
		v.unknown("pool restore not proven before caller ended")
	}
	if p.Kind == "boundary" && p.Admission != nil {
		before, err1 := tracker.Map(p.Admission.Before.At, recording.ControlOffset)
		after, err2 := tracker.Map(p.Admission.After.At, recording.ControlOffset)
		start := maxTime(p.Run.StartedAt, c.StartedAt.Add(c.ConnectedAt))
		end := minTime(p.Run.StartedAt.Add(p.Run.Duration), c.StartedAt.Add(c.HungUpAt))
		if err1 != nil || err2 != nil || before.Earliest.Before(start) || before.Latest.After(end) || after.Earliest.Before(start) || after.Latest.After(end) {
			v.unknown("admission observations not proven inside live media window across clock bounds")
		}
	}
	expectedMoves := 0
	if p.Kind != "boundary" {
		expectedMoves = 1
	}
	if len(c.Moves) != expectedMoves || len(recording.Armed) != expectedMoves {
		v.unknown("unexpected/missing/duplicate move receipts or arms")
	}
	for _, move := range c.Moves {
		judgeLeanMove(m, p, proc, msg, move, tracker, v)
	}
	// Whole-call loss is classified using guaranteed/possible interval windows.
	judgeLeanDelivery(p, proc, c, v)
	kinds := map[string]bool{}
	for _, t := range c.Tracks {
		kinds[t.Kind] = true
		if t.Packets == 0 || t.UnmatchedPayloads > 0 || t.DuplicatePackets > 0 {
			v.fail("missing/corrupt/duplicate " + t.Kind + " media")
		}
		if t.Video != nil && (t.Video.UnmatchedFrames > 0 || t.Video.KeyframeDecodeErrors > 0 || t.Video.NonMonotonicTimestamps > 0) {
			v.fail("invalid video content")
		}
		judgeLeanGaps(p, proc, c, t, v)
	}
	if !kinds["audio"] || (c.SentVideo.Frames > 0 && !kinds["video"]) {
		v.unknown("missing expected media tracks")
	}
}

func judgeLeanMove(m leanManifest, p leanPhase, proc loadgen.ProcessReport, msg loadgen.Message, move callharness.MoveReport, tracker *callharness.ClockTracker, v *leanVerdict) {
	if move.Error != "" || move.Lost || move.LossCause != "" || move.Measurement.Failed || move.DecryptionFailuresAfterResume > 0 {
		v.fail("move error, loss, content failure or decryption rejection")
	}
	d, e := move.Distributed, move.Receipt
	if d == nil || e == nil || move.Identity == nil || !move.Identity.Valid() || *move.Identity != e.Identity || e.Identity.RunID != m.RunID || e.Identity.SessionID != msg.Session || e.Kind != move.Kind {
		v.unknown("missing/mismatched durable distributed receipt")
		return
	}
	if e.Result.Lost || e.Result.LossCause != "" {
		v.fail("server reports lost call: " + e.Result.LossCause)
	}
	if e.Uncertain || e.Result.Error != "" {
		v.unknown("operation result uncertain or failed")
	}
	target := leanWorkerFor(p.After, msg.Session)
	if target == nil || target.Name != e.Result.To {
		v.unknown("receipt destination differs from observed owner")
	}
	if e.Result.Kind != e.Kind || e.Result.ID != msg.Session || e.Result.From != e.Identity.Worker || e.Result.To == "" || e.Result.To == e.Result.From {
		v.unknown("receipt result does not match affected ownership")
	}
	if len(p.Run.Events) != 1 || !loadgen.MatchesEvent(p.Run.Events[0], msg.Call, move) {
		v.unknown("receipt does not match scheduled identity")
	}
	if !slices.Contains(msg.Call.Distributed.Armed, *move.Identity) {
		v.unknown("receipt identity was not armed")
	}
	host := msg.Call.Distributed.ControlOffset
	if move.Kind == "takeover" {
		host = msg.Call.Distributed.WorkerOffset
	}
	start, err1 := tracker.Map(e.Start, host)
	end, err2 := tracker.Map(e.End, msg.Call.Distributed.ControlOffset)
	if err1 != nil || err2 != nil || !d.Mapped || (!start.Earliest.Equal(d.Start.Earliest) || !start.Latest.Equal(d.Start.Latest)) || (!end.Earliest.Equal(d.End.Earliest) || !end.Latest.Equal(d.End.Latest)) || !start.Earliest.Before(end.Latest) {
		v.unknown("distributed interval does not match clock probes and receipt")
	}
	bracket := false
	for _, t := range p.Timings {
		if t.Identity == e.Identity {
			bracket = !t.ArmAcknowledgedAt.IsZero() && !t.ReceiptSentAt.IsZero() && !d.Start.Earliest.Before(t.ArmAcknowledgedAt) && !d.End.Latest.After(t.ReceiptSentAt)
		}
	}
	if d.Start.Earliest.Before(p.Run.StartedAt.Add(3*time.Second)) || !d.End.Latest.Before(p.Run.StartedAt.Add(p.Run.Duration-7*time.Second)) {
		v.unknown("actual distributed event lacks warmup or recovery tail")
	}
	if p.After.At.Before(e.End) {
		v.unknown("after-pool observation precedes completed operation")
	}
	if !bracket {
		v.unknown("mapped event lacks caller-local arm/receipt bracket")
	}
	if d.WindowOutcome == "fail" || d.AudioLoss.Failed || d.VideoLoss.Failed {
		v.fail("distributed window content failure")
	} else if d.WindowOutcome != "pass" || !d.AudioLoss.Trusted || (msg.Call.SentVideo.Frames > 0 && !d.VideoLoss.Trusted) || move.Measurement.Inconclusive {
		v.unknown("distributed event classification inconclusive")
	}
	if len(d.Gaps) == 0 || len(d.Durations) == 0 {
		v.unknown("missing distributed gap/duration evidence")
	}
	// All timing correlation and stall overlap stays on this caller's clock.
	bound := stallBound(proc.Samples, p.Run.Limits.P99, d.Start.Earliest.Add(-time.Second), d.End.Latest.Add(2*time.Second))
	for _, g := range d.Gaps {
		if g.EventGap.Lower < 0 || g.EventGap.Upper < g.EventGap.Lower || len(g.BaselineCertain) < 10 {
			v.unknown("invalid distributed gap/baseline")
		}
		if g.EventGap.Lower >= 2*time.Second {
			v.fail(g.Kind + " pause >=2s")
		} else if g.EventGap.Upper+bound >= 2*time.Second {
			v.unknown(g.Kind + " pause may reach 2s with clock/stall bound")
		}
	}
	for name, duration := range d.Durations {
		if duration.Upper < duration.Lower {
			v.unknown("invalid " + name + " duration interval")
		}
	}
	if move.Kind == "takeover" {
		if d.KillToLive == nil {
			v.unknown("actual-kill-to-live interval missing")
		} else if d.KillToLive.Lower >= 2*time.Second {
			v.fail("actual kill to live reaches 2s")
		} else if d.KillToLive.Upper+bound >= 2*time.Second {
			v.unknown("actual kill to live may reach 2s")
		}
	}
	gapKinds := map[string]bool{}
	for _, gap := range d.Gaps {
		if gapKinds[gap.Kind] {
			v.unknown("duplicate distributed track gap")
		}
		gapKinds[gap.Kind] = true
	}
	if !gapKinds["audio"] || (msg.Call.SentVideo.Frames > 0 && !gapKinds["video"]) {
		v.unknown("missing expected distributed track gaps")
	}
	if _, ok := d.Durations["audio_freshness"]; !ok {
		v.unknown("missing audio freshness duration interval")
	}
	// Pooled p95 is rejudged with caller-stall upper bounds separately.
}

// leanGapWindow returns certain and possible scheduled windows on the caller
// clock. Scalar Start/End/MediaResumedAt fields are deliberately unused.
func leanGapWindow(c *callharness.Report, m callharness.MoveReport) (time.Duration, time.Duration, time.Duration, time.Duration) {
	d := m.Distributed
	finishLo, finishHi := d.End.Earliest.Sub(c.StartedAt), d.End.Latest.Sub(c.StartedAt)
	startLo, startHi := d.Start.Earliest.Sub(c.StartedAt), d.Start.Latest.Sub(c.StartedAt)
	if m.Kind == "takeover" {
		finishLo = max(finishLo, startLo+2*time.Second)
		finishHi = max(finishHi, startHi+2*time.Second)
	} else if resume, ok := d.Durations["resume"]; ok {
		finishLo = max(finishLo, startLo+resume.Lower)
		finishHi = max(finishHi, startHi+resume.Upper)
	}
	return startHi - 150*time.Millisecond, finishLo + 150*time.Millisecond, startLo - 150*time.Millisecond, finishHi + 150*time.Millisecond
}
func leanGapExemption(c *callharness.Report, start, end time.Duration) (bool, bool) {
	possible := false
	for _, m := range c.Moves {
		if m.Distributed == nil || !m.Distributed.Mapped {
			continue
		}
		a, b, x, y := leanGapWindow(c, m)
		if start >= a && end <= b {
			return true, true
		}
		possible = possible || (start >= x && end <= y)
	}
	return false, possible
}
func leanTrackGaps(t callharness.TrackReport) []callharness.MediaGap {
	gaps := append([]callharness.MediaGap{{EndedAt: t.MediaGapEndedAt, Duration: t.MediaGap}}, t.MediaGaps...)
	for i := 1; i < len(t.Arrivals); i++ {
		gaps = append(gaps, callharness.MediaGap{EndedAt: t.Arrivals[i], Duration: t.Arrivals[i] - t.Arrivals[i-1]})
	}
	return gaps
}

// These observations use only the caller's monotonic timeline. Bad host
// clocks cannot turn a definite continuity failure into inconclusive evidence.
func judgeLeanLocalContinuity(p leanPhase, c *callharness.Report, v *leanVerdict) {
	for _, track := range c.Tracks {
		if !track.MediaGapsRecorded {
			v.unknown(track.Kind + " whole-call gap inventory unavailable")
		}
		if track.MediaGapOverflow > 0 {
			v.fail(track.Kind + " whole-call gap inventory overflow")
		}
		if reason := gapInventoryFullCause(track); reason != "" {
			v.unknown(reason)
		}
		for _, gap := range leanTrackGaps(track) {
			if gap.Duration > 2*time.Second {
				v.fail(track.Kind + " caller-local gap exceeds 2s")
			}
		}
	}
	d := c.Recording.Delivery
	if d == nil {
		v.unknown("whole-call delivery unavailable")
		return
	}
	for _, k := range []struct {
		kind    string
		units   callharness.ContentDelivery
		written int
	}{{"audio", d.Audio, c.SentAudio.Frames}, {"video", d.Video, c.SentVideo.Frames}} {
		u := k.units
		if u.Sent < 0 || u.Returned < 0 || u.Lost < 0 || u.Unsettled < 0 || u.Sent+u.Unsettled != k.written || u.Returned+u.Lost != u.Sent || len(u.LostAt)+u.LostOverflow != u.Lost {
			v.unknown(k.kind + " delivery accounting incomplete")
		}
		if u.LostOverflow > 0 {
			v.fail("lost-unit inventory overflow")
		}
		for _, at := range u.LostAt {
			inside, bracket := false, false
			for _, timing := range p.Timings {
				if timing.ArmAcknowledgedAt.IsZero() || timing.ReceiptSentAt.IsZero() || timing.ReceiptSentAt.Before(timing.ArmAcknowledgedAt) {
					continue
				}
				for _, move := range c.Moves {
					if move.Receipt == nil || move.Identity == nil || !timing.Identity.Valid() || *move.Identity != timing.Identity || move.Receipt.Identity != timing.Identity || c.Distributed == nil || !slices.Contains(c.Distributed.Armed, timing.Identity) {
						continue
					}
					bracket = true
					instant := c.StartedAt.Add(at)
					inside = inside || (!instant.Before(timing.ArmAcknowledgedAt) && !instant.After(timing.ReceiptSentAt.Add(soakDeliverySettle)))
				}
			}
			if !inside && (bracket || p.Kind == "boundary") {
				v.fail(k.kind + " loss outside caller-local scheduled event arm/receipt bracket")
			} else if !bracket {
				v.unknown(k.kind + " loss lacks caller-local arm/receipt bracket")
			}
		}
	}
}

func judgeLeanGaps(p leanPhase, proc loadgen.ProcessReport, c *callharness.Report, t callharness.TrackReport, v *leanVerdict) {
	for _, g := range leanTrackGaps(t) {
		if g.Duration < 0 || g.EndedAt < g.Duration {
			v.unknown("invalid caller-local media gap")
			continue
		}
		certain, possible := leanGapExemption(c, g.EndedAt-g.Duration, g.EndedAt)
		if certain {
			continue
		}
		bound := stallBound(proc.Samples, p.Run.Limits.P99, c.StartedAt.Add(g.EndedAt-g.Duration-time.Second), c.StartedAt.Add(g.EndedAt))
		if g.Duration > 150*time.Millisecond {
			if possible || proc.Lateness.Max >= g.Duration-150*time.Millisecond {
				v.unknown("whole-call gap ambiguous within clock or caller-stall bound")
			} else {
				v.fail("unscheduled " + t.Kind + " gap exceeds 150ms")
			}
		} else if g.Duration+bound > 150*time.Millisecond {
			v.unknown("unscheduled gap plus caller-stall bound exceeds 150ms")
		}
	}
}
func judgeLeanDelivery(p leanPhase, proc loadgen.ProcessReport, c *callharness.Report, v *leanVerdict) {
	d := c.Recording.Delivery
	if d == nil {
		return
	}
	for _, k := range []struct {
		kind  string
		units callharness.ContentDelivery
	}{{"audio", d.Audio}, {"video", d.Video}} {
		u := k.units
		for _, at := range u.LostAt {
			certain, possible := false, false
			for _, move := range c.Moves {
				d := move.Distributed
				if d == nil || !d.Mapped {
					continue
				}
				lo, hi := d.Start.Earliest.Sub(c.StartedAt), d.Start.Latest.Sub(c.StartedAt)
				endLo, endHi := d.End.Earliest.Sub(c.StartedAt)+soakDeliverySettle, d.End.Latest.Sub(c.StartedAt)+soakDeliverySettle
				certain = certain || (at >= hi && at < endLo)
				possible = possible || (at >= lo && at < endHi)
			}
			if certain {
				continue
			}
			saturated := false
			for i, s := range proc.Samples {
				if sampleSaturated(proc.Samples, i, p.Run.Limits, p.Run.StartedAt, mediaEnd(p.Run, proc)) && s.At.After(c.StartedAt.Add(at)) && s.At.Add(-s.Interval).Before(c.StartedAt.Add(at+soakDeliverySettle)) {
					saturated = true
				}
			}
			if possible || saturated {
				v.unknown(k.kind + " loss lies in uncertain event/stall margin")
			} else {
				v.fail(k.kind + " loss outside scheduled event")
			}
		}
	}
}
func judgeLeanTelemetry(m leanManifest, p leanPhase, v *leanPhaseResult) {
	t := p.Run.Telemetry
	if t.WindowSeconds <= 0 || math.Abs(t.WindowSeconds-p.Run.Duration.Seconds()) > 2*p.WindowError.Seconds()+.001 || len(t.Errors) > 0 {
		v.unknown("missing/alignment-uncertain service telemetry window")
	}
	roles := map[string]int{}
	names := map[string]bool{}
	takeovers := 0.0
	takeoverSeen := false
	for _, process := range t.Processes {
		role := process.Name
		if strings.HasPrefix(role, "store-") {
			role = "store"
		}
		if strings.HasPrefix(role, "worker-") {
			role = "worker"
		}
		roles[role]++
		if names[process.Name] {
			v.unknown("duplicate service incarnation")
		}
		names[process.Name] = true
		if process.UnexpectedExit {
			v.fail(process.Name + " unexpected service exit")
		}
		if process.Samples < 2 || (!finiteNonnegative(process.ObservedSeconds) || process.ObservedSeconds <= 0) || !finiteNonnegative(process.CPUSeconds) || process.RSSUnavailable || process.PeakRSSBytes <= 0 || len(process.Errors) > 0 {
			v.unknown(process.Name + " resource observations incomplete")
		}
		delta := func(name string, required bool) float64 {
			first, a := process.First[name]
			last, b := process.Last[name]
			if !a || !b || !finiteNonnegative(first) || !finiteNonnegative(last) || last < first {
				if required || a || b {
					v.unknown(process.Name + " missing/reset counter " + name)
				}
				return 0
			}
			return last - first
		}
		denom := float64(p.Run.Callers) * (p.Run.Duration - 2*p.WindowError).Seconds()
		if denom <= 0 {
			v.unknown("invalid call-second denominator")
			continue
		}
		commands, wire := 0.0, 0.0
		commandEvidence, wireEvidence := false, false
		for name := range process.First {
			if strings.Contains(name, "_total") {
				if _, ok := process.Last[name]; !ok {
					v.unknown(process.Name + " counter missing from final sample: " + name)
				}
			}
		}
		for name := range process.Last {
			if strings.HasPrefix(name, "relais_store_commands_total") {
				commands += delta(name, true)
				commandEvidence = true
			}
			if strings.HasPrefix(name, "relais_transport_bytes_total{") && strings.Contains(name, `transport="redis"`) {
				wire += delta(name, true)
				wireEvidence = true
			}
			if strings.HasPrefix(name, "relais_control_losses_total") && delta(name, true) > 0 {
				v.fail("control reports lost calls")
			}
		}
		if role != "store" && (!commandEvidence || !wireEvidence) {
			v.unknown(process.Name + " missing application store counters")
		}
		v.CommandsLowerPerCallSecond += commands / (float64(p.Run.Callers) * (p.Run.Duration + 2*p.WindowError).Seconds())
		v.BytesLowerPerCallSecond += wire / (float64(p.Run.Callers) * (p.Run.Duration + 2*p.WindowError).Seconds())
		v.CommandsPerCallSecond += commands / denom
		v.BytesPerCallSecond += wire / denom
		if role == "relay" {
			value, ok := process.Last["relais_relay_buffer_enabled"]
			if !ok || (value != 0 && value != 1) || (value == 1) != *m.RelayBufferEnabled {
				v.unknown("running relay buffer setting missing/mismatched")
			}
			packets := delta("relais_relay_worker_packets_total", true)
			callers := delta("relais_relay_caller_packets_total", true)
			if packets <= 0 || callers <= 0 {
				v.unknown("missing relay packet evidence")
			}
			bytes := delta("relais_relay_public_udp_bytes_out_total", true) + 28*packets
			measuredDenominator := float64(p.Run.Callers) * t.WindowSeconds
			if measuredDenominator > 0 && finiteNonnegative(bytes/measuredDenominator) {
				v.PublicBytesPerCallSecond += bytes / measuredDenominator
			} else {
				v.unknown("public egress rate unavailable or overflowed")
			}
		}
		if role == "control" {
			_, takeoverSeen = process.First["relais_control_takeovers_total"]
			takeovers += delta("relais_control_takeovers_total", true)
			if process.Last["relais_control_worker_call_cap"] != 2 {
				v.unknown("running control cap=2 not confirmed")
			}
			for _, cause := range []string{"ownership_expired", "checkpoint_unsafe", "checkpoint_missing", "recovery_failed"} {
				delta(`relais_control_losses_total{cause="`+cause+`"}`, true)
			}
		}
		if role == "worker" {
			peak, ok := process.Peaks["relais_worker_active_sessions"]
			if !ok || !finiteNonnegative(peak) {
				v.unknown("missing worker peak occupancy")
			} else if peak > 2 || (p.Kind != "boundary" && peak > 1) {
				v.fail("worker exceeded validated phase occupancy")
			}
			if delta("relais_worker_decryption_failures_total", true) > 0 || delta("relais_worker_replay_rejections_total", true) > 0 {
				v.fail("worker decryption/replay rejection")
			}
		}
		for _, metric := range []string{"rss", "heap-in-use"} {
			if role == "store" && metric == "heap-in-use" {
				continue
			}
			memory := judgeMemory(process.Name, process.PID, metric, process.Memory, p.Run.StartedAt, p.Run.Duration, role == "worker")
			v.Memory = append(v.Memory, memory)
			if memory.Outcome == "fail" {
				v.fail(process.Name + " memory growth")
			}
		}
	}
	for _, role := range []string{"relay", "control", "store", "worker"} {
		if roles[role] == 0 {
			v.unknown("missing " + role + " resource evidence")
		}
	}
	for _, w := range p.Before.Workers {
		if !names["worker-"+w.Name+"@"+w.Generation] {
			v.unknown("missing worker generation telemetry")
		}
	}
	want := 0.0
	if p.Kind == "kill" {
		want = 1
	}
	if !takeoverSeen {
		v.unknown("missing takeover count window")
	} else if takeovers != want {
		v.fail("takeover counter differs from scheduled affected calls")
	}
	if !finiteNonnegative(v.CommandsLowerPerCallSecond) {
		v.unknown("store command lower rate overflow")
		v.CommandsLowerPerCallSecond = 0
	}
	if !finiteNonnegative(v.BytesLowerPerCallSecond) {
		v.unknown("store byte lower rate overflow")
		v.BytesLowerPerCallSecond = 0
	}
	if !finiteNonnegative(v.CommandsPerCallSecond) {
		v.unknown("store command rate overflow")
		v.CommandsPerCallSecond = 0
	}
	if !finiteNonnegative(v.BytesPerCallSecond) {
		v.unknown("store byte rate overflow")
		v.BytesPerCallSecond = 0
	}
	if !finiteNonnegative(v.PublicBytesPerCallSecond) {
		v.unknown("public egress rate overflow")
		v.PublicBytesPerCallSecond = 0
	}
	if v.CommandsPerCallSecond <= 0 || v.BytesPerCallSecond <= 0 || v.PublicBytesPerCallSecond <= 0 {
		v.unknown("missing store/egress evidence")
	}
	if v.CommandsLowerPerCallSecond > 100 || v.BytesLowerPerCallSecond > 128<<10 {
		v.fail("store command/byte budget exceeded across alignment bounds")
	} else if v.CommandsPerCallSecond > 100 || v.BytesPerCallSecond > 128<<10 {
		v.unknown("store command/byte budget not proven across alignment bounds")
	}
	judgeLeanAges(p, v)
}
func judgeLeanAges(p leanPhase, v *leanPhaseResult) {
	a := p.Run.Telemetry.Agents
	if a.IntervalMS != 100 || len(a.Errors) > 0 {
		v.unknown("missing/error 100ms checkpoint/snapshot samples")
	}
	v.Ages.Samples = len(a.Samples)
	ages := map[string][]capacity.AgentSample{}
	seen := map[string]bool{}
	for _, s := range a.Samples {
		if !s.Status.CheckpointKnown {
			continue
		}
		key := fmt.Sprint(s.Status.ID, "/", s.At.UnixNano())
		if seen[key] {
			v.unknown("duplicate age sample")
		}
		seen[key] = true
		if s.At.Before(p.Run.StartedAt) || s.At.After(p.Run.StartedAt.Add(p.Run.Duration)) || !finiteNonnegative(s.Status.CheckpointAgeMS) || !finiteNonnegative(s.Status.SnapshotAgeMS) {
			v.unknown("invalid/out-of-window age sample")
			continue
		}
		ages[s.Status.ID] = append(ages[s.Status.ID], s)
	}
	ticks := p.Run.Duration.Seconds() * 10
	if a.Missing > uint64(math.Ceil(float64(p.Run.Callers)*ticks*.1)) {
		v.unknown("more than 10% age samples missing")
	}
	for _, id := range leanSessions(p.Before) {
		samples := ages[id]
		if len(samples) < int(math.Floor(ticks*.9)) {
			v.unknown("incomplete age coverage for " + id)
			continue
		}
		slices.SortFunc(samples, func(a, b capacity.AgentSample) int { return a.At.Compare(b.At) })
		if samples[len(samples)-1].At.Sub(samples[0].At) < p.Run.Duration*9/10 {
			v.unknown("age samples do not span phase")
		}
		for index := 1; index < len(samples); index++ {
			if samples[index].At.Sub(samples[index-1].At) < 50*time.Millisecond {
				v.unknown("age samples are clustered rather than 100ms observations")
				break
			}
		}
		var checkpoint, snapshot []float64
		for _, s := range samples {
			v.Ages.CheckpointMaxMS = max(v.Ages.CheckpointMaxMS, s.Status.CheckpointAgeMS)
			v.Ages.SnapshotMaxMS = max(v.Ages.SnapshotMaxMS, s.Status.SnapshotAgeMS)
			checkpoint = append(checkpoint, s.Status.CheckpointAgeMS)
			snapshot = append(snapshot, s.Status.SnapshotAgeMS)
		}
		slices.Sort(checkpoint)
		slices.Sort(snapshot)
		idx := int(math.Ceil(float64(len(samples))*.99)) - 1
		v.Ages.CheckpointP99MS = max(v.Ages.CheckpointP99MS, checkpoint[idx])
		v.Ages.SnapshotP99MS = max(v.Ages.SnapshotP99MS, snapshot[idx])
		if checkpoint[idx] > 550 || snapshot[idx] > 550 {
			v.fail("checkpoint/snapshot age p99 exceeds 550ms")
		}
	}
	if len(ages) != p.Run.Callers {
		v.unknown("age samples do not match session inventory")
	}
}

var leanENACounters = []string{"pps_allowance_exceeded", "conntrack_allowance_exceeded", "bw_in_allowance_exceeded", "bw_out_allowance_exceeded"}

func judgeLeanAllowances(m leanManifest, p leanPhase, v *leanVerdict) {
	hosts := map[string]bool{}
	for _, a := range p.Allowances {
		key := a.Host + "@" + a.Generation
		if a.Host == "" || a.Generation == "" || hosts[key] {
			v.unknown("missing/duplicate ENA host generation")
		}
		hosts[key] = true
		for _, name := range leanENACounters {
			x, ok := a.First[name]
			y, last := a.Last[name]
			if !ok || !last || !finiteNonnegative(x) || !finiteNonnegative(y) || y < x {
				v.unknown("missing/reset ENA allowance " + name)
			} else if y > x {
				v.fail("ENA allowance exceeded: " + name)
			}
		}
		if a.ConntrackRemaining == nil || !finiteNonnegative(*a.ConntrackRemaining) {
			v.unknown("missing conntrack headroom")
		}
	}
	if m.Topology != "local" || m.ENARequired {
		for _, name := range []string{"relay", "control"} {
			found := false
			for _, a := range p.Allowances {
				found = found || a.Host == name
			}
			if !found {
				v.unknown("required ENA counters unavailable for " + name)
			}
		}
		for _, w := range p.Before.Workers {
			if !hosts[w.Name+"@"+w.Generation] {
				v.unknown("required worker ENA counters unavailable")
			}
		}
	}
}

func judgeLeanStallTrials(phases []loadgen.RunReport, v *leanVerdict) {
	var raw, worst []callharness.MoveReport
	for _, p := range phases {
		for _, proc := range p.Processes {
			for _, msg := range proc.Calls {
				if msg.Call == nil {
					continue
				}
				for _, move := range msg.Call.Moves {
					if move.Distributed == nil {
						continue
					}
					raw = append(raw, move)
					d := *move.Distributed
					d.Gaps = slices.Clone(d.Gaps)
					bound := stallBound(proc.Samples, p.Limits.P99, d.Start.Earliest.Add(-time.Second), d.End.Latest.Add(2*time.Second))
					for i := range d.Gaps {
						d.Gaps[i].EventGap.Upper += bound
					}
					move.Distributed = &d
					worst = append(worst, move)
				}
			}
		}
	}
	measured, _ := callharness.JudgeDistributedTrials(raw)
	guarded, _ := callharness.JudgeDistributedTrials(worst)
	if measured == "pass" && guarded != "pass" {
		v.unknown("distributed pooled p95 may breach move thresholds with caller-stall bounds")
	}
}
