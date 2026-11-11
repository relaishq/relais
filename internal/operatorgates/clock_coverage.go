package operatorgates

import (
	"context"
	"encoding/json"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// ClockCoverageReader is an isolated, read-only host adapter. It must not use
// Session.Exchange: diagnostic transport failures cannot latch the live session.
// The returned string is the existing packed host clock output.
type ClockCoverageReader func(context.Context, string, string) (string, error)

func (s *Session) retainClockAttempt(e clocks.Evidence)      { s.retainCoverageAttempt(e, false) }
func (s *Session) retainDrainClockAttempt(e clocks.Evidence) { s.retainCoverageAttempt(e, true) }
func (s *Session) retainCoverageAttempt(e clocks.Evidence, drain bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clockCoverageFalsified = s.clockCoverageFalsified || clocks.HasFalsification(e)
	history, omitted, limit := &s.clockAttempts, &s.clockAttemptsOmitted, clocks.CoverageAttemptLimit
	if drain {
		history, omitted, limit = &s.clockDrainAttempts, &s.clockDrainAttemptsOmitted, clocks.CoverageDrainAttemptLimit
	}
	if len(*history) >= limit {
		(*omitted)++
		s.noteClockLoss(e)
		return
	}
	*history = append(*history, clocks.PublicEvidence(e))
}

// The retained prefix stays usable. A bounded per-slot loss interval prevents a
// later drain from joining across rows acknowledged but omitted from history.
// Caller holds s.mu. Failed reads without rows/breaks are not invented gaps.
func (s *Session) noteClockLoss(e clocks.Evidence) {
	for slot, sample := range e.Clocks {
		if sample.TrackingLog == nil {
			continue
		}
		knownRows := map[observe.CoverageRow]bool{}
		knownBreaks := map[observe.CoverageBreak]bool{}
		for _, history := range [][]clocks.Evidence{s.clockAttempts, s.clockDrainAttempts} {
			for _, attempt := range history {
				if l := attempt.Clocks[slot].TrackingLog; l != nil {
					for _, r := range l.Rows {
						knownRows[observe.CoverageRow{UnixSeconds: r.UnixSeconds, MaxErrorSeconds: r.MaxErrorSeconds, LeapStatus: r.LeapStatus}] = true
					}
					for _, b := range l.Breaks {
						knownBreaks[observe.CoverageBreak{FromUnixSeconds: b.FromUnixSeconds, UnixSeconds: b.UnixSeconds, Kind: b.Kind}] = true
					}
				}
			}
		}
		from, to, known := int64(0), int64(0), false
		add := func(a, b int64) {
			if !known {
				from, to, known = a, b, true
			}
			from = min(from, a)
			to = max(to, b)
		}
		for _, r := range sample.TrackingLog.Rows {
			row := observe.CoverageRow{UnixSeconds: r.UnixSeconds, MaxErrorSeconds: r.MaxErrorSeconds, LeapStatus: r.LeapStatus}
			if knownRows[row] {
				continue
			}
			previous, havePrevious := int64(0), false
			for old := range knownRows {
				if old.UnixSeconds < r.UnixSeconds && (!havePrevious || old.UnixSeconds > previous) {
					previous, havePrevious = old.UnixSeconds, true
				}
			}
			if !havePrevious {
				previous = r.UnixSeconds
			}
			add(previous, r.UnixSeconds)
		}
		for _, b := range sample.TrackingLog.Breaks {
			br := observe.CoverageBreak{FromUnixSeconds: b.FromUnixSeconds, UnixSeconds: b.UnixSeconds, Kind: b.Kind}
			if !knownBreaks[br] {
				add(br.FromUnixSeconds, br.UnixSeconds)
			}
		}
		if !known {
			continue
		}
		if s.clockMissingIntervals == nil {
			s.clockMissingIntervals = map[string]observe.CoverageBreak{}
		}
		if old, ok := s.clockMissingIntervals[slot]; ok {
			from = min(from, old.FromUnixSeconds)
			to = max(to, old.UnixSeconds)
		}
		s.clockMissingIntervals[slot] = observe.CoverageBreak{FromUnixSeconds: from, UnixSeconds: to, Kind: "log-loss"}
	}
}

func (s *Session) coverageSnapshot(_ clocks.Bounds) *clocks.CoverageEvidence {
	if s.clockBinding == nil {
		return &clocks.CoverageEvidence{Binding: &observe.ClockBinding{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := inventory.ComponentSlot(s.inventoryComponents, "signal")
	c, _ := inventory.ComponentSlot(s.inventoryComponents, "control")
	missing := map[string]observe.CoverageBreak{}
	for slot, b := range s.clockMissingIntervals {
		missing[slot] = b
	}
	return &clocks.CoverageEvidence{Falsified: s.clockCoverageFalsified, Version: clocks.EvidenceVersion, FrontendSlot: f, ControlSlot: c, Bounds: s.clockBinding.Binding.Bounds, Attempts: boundCoverageAttempts(s.clockAttempts), DrainAttempts: boundCoverageAttempts(s.clockDrainAttempts), OmittedAttempts: s.clockAttemptsOmitted, OmittedDrainAttempts: s.clockDrainAttemptsOmitted, OmittedBrackets: s.clockBracketsOmitted, MissingIntervals: missing, KillBrackets: append([]observe.CoverageBracket{}, s.clockBrackets...), ClockReceiptSHA256: s.clockBinding.Binding.ReceiptSHA256, Binding: s.clockBinding}
}
func (s *Session) retainClockBracket(worker, before, after, midpoint string, diagnostics *observe.SignalDiagnostics) {
	if midpoint == "" {
		midpoint = "unavailable"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clockBrackets) >= 4096 {
		s.clockBracketsOmitted++
		return
	}
	s.clockBrackets = append(s.clockBrackets, observe.CoverageBracket{Diagnostics: diagnostics, Worker: worker, BeforeUnixNS: before, AfterUnixNS: after, MidpointUnixNS: midpoint})
}

// collectCoverage bypasses the policy cache and retains every diagnostic read.
// A missing isolated adapter is explicit incomplete evidence, never a media error.
func (s *Session) collectCoverage(ctx context.Context, bounds clocks.Bounds, windows []observe.CoverageWindow, read ClockCoverageReader, pause func(context.Context, time.Duration) error, cutoff time.Duration) *clocks.CoverageEvidence {
	e := s.coverageSnapshot(bounds)
	e.Windows = windows
	e.Evaluate()
	if e.DrainComplete || read == nil || ctx.Err() != nil {
		return e
	}
	// Drainage consumes the original collection reserve; it never extends it.
	limit := s.timing.drainLimit()
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, max(0, time.Until(deadline)))
	}
	end := min(s.Budget.end, s.clock.Elapsed()+limit, cutoff)
	bounded, cancel := context.WithTimeout(ctx, max(0, end-s.clock.Elapsed()))
	defer cancel()
	s.mu.Lock()
	cursors := map[string]json.RawMessage{}
	for slot, c := range s.clockCursors {
		cursors[slot] = append(json.RawMessage{}, c...)
	}
	s.mu.Unlock()
	for !e.DrainComplete && bounded.Err() == nil && s.clock.Elapsed() < end && len(e.DrainAttempts) < clocks.CoverageDrainAttemptLimit {
		anyLog := false
		attempt := clocks.Evidence{Version: clocks.EvidenceVersion, At: s.clock.Wall(), Clocks: map[string]clocks.Sample{}}
		for _, slot := range inventory.Slots {
			if bounded.Err() != nil || s.clock.Elapsed() >= end {
				break
			}
			script := host.ClockWithLease(cursors[slot], s.Budget.armed, "", nil)
			stdout, err := read(bounded, slot, script)
			if err != nil {
				continue
			}
			raw, err := host.Unpack(stdout, 256000)
			if err != nil {
				continue
			}
			sample, err := clocks.Parse(raw)
			if err != nil {
				continue
			}
			attempt.Clocks[slot] = sample
			if sample.TrackingLog != nil {
				anyLog = true
				cursors[slot] = sample.TrackingLog.Cursor
			}
		}
		s.retainDrainClockAttempt(attempt)
		e = s.coverageSnapshot(bounds)
		e.Windows = windows
		e.Evaluate()
		// An adapter returning only the legacy scalar shape cannot close a
		// tracking timeline; repeating that unsupported read cannot help.
		if !anyLog && len(attempt.Clocks) == len(inventory.Slots) {
			break
		}
		if !e.DrainComplete {
			if err := pause(bounded, min(time.Second, max(0, end-s.clock.Elapsed()))); err != nil {
				break
			}
		}
	}
	return e
}

func (s *Session) ClockAttempts() []clocks.Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]clocks.Evidence{}, s.clockAttempts...)
}

// SeedClockCoverageFalsification carries a prior sequence's run-wide trust loss
// into this Session. It never clears a falsification observed in this sequence.
func (s *Session) SeedClockCoverageFalsification(falsified bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clockCoverageFalsified = s.clockCoverageFalsified || falsified
}
func (s *Session) ClockCoverageFalsified() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clockCoverageFalsified
}

func boundCoverageAttempts(attempts []clocks.Evidence) []clocks.CoverageAttempt {
	out := []clocks.CoverageAttempt{}
	for _, a := range attempts {
		out = append(out, clocks.NewBoundCoverageAttempt(a))
	}
	return out
}
