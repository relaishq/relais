package main

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

// A possible population must contain every guaranteed observation, including
// repeated values. Length alone cannot establish this invariant.
func baselinePopulation(g callharness.DistributedGap) bool {
	if len(g.BaselineCertain) == 0 || len(g.BaselinePossible) < len(g.BaselineCertain) {
		return false
	}
	counts := make(map[time.Duration]int, len(g.BaselinePossible))
	for _, n := range g.BaselinePossible {
		counts[n]++
	}
	for _, n := range g.BaselineCertain {
		if counts[n] == 0 {
			return false
		}
		counts[n]--
	}
	return true
}
func gapCollections(gaps []callharness.DistributedGap) bool {
	if len(gaps) == 0 {
		return false
	}
	kinds := map[string]bool{}
	for _, g := range gaps {
		if g.Kind != "audio" && g.Kind != "video" || kinds[g.Kind] || !baselinePopulation(g) {
			return false
		}
		kinds[g.Kind] = true
	}
	return true
}

// Product-owned collections define their own membership. Diagnostic arrays
// that are not consumed by either judge do not become qualification evidence.
func callerCollections(c *callharness.Report) error {
	if c == nil || c.Distributed == nil || len(c.Tracks) == 0 || len(c.Distributed.Probes) < 10 {
		return fmt.Errorf("missing caller collections")
	}
	if observe.Numbers(c.Distributed.Probes) != nil {
		return fmt.Errorf("invalid probe numbers")
	}
	for i, p := range c.Distributed.Probes {
		if _, err := p.Offset(); err != nil || i > 0 && !p.CallerSend.After(c.Distributed.Probes[i-1].CallerSend) {
			return fmt.Errorf("invalid probe population")
		}
	}
	tracks := map[string]bool{}
	for _, t := range c.Tracks {
		if t.Kind != "audio" && t.Kind != "video" || tracks[t.Kind] {
			return fmt.Errorf("ambiguous track inventory")
		}
		tracks[t.Kind] = true

	}
	if !tracks["audio"] || !tracks["video"] {
		return fmt.Errorf("incomplete track inventory")
	}
	return nil
}
func trialCollections(c *callharness.Report) error {
	if err := callerCollections(c); err != nil {
		return err
	}
	if len(c.Moves) == 0 {
		return fmt.Errorf("missing trials")
	}
	tracks := map[string]bool{}
	for _, t := range c.Tracks {
		tracks[t.Kind] = true
	}
	armed := map[callharness.OperationIdentity]bool{}
	logical := map[[3]string]bool{}
	for _, id := range c.Distributed.Armed {
		key := [3]string{id.RunID, id.OperationID, id.SessionID}
		if !id.Valid() || id.RunID != c.Distributed.RunID || armed[id] || logical[key] {
			return fmt.Errorf("invalid armed inventory")
		}
		armed[id], logical[key] = true, true
	}
	if len(armed) == 0 || len(c.Distributed.Missing) > 0 {
		return fmt.Errorf("incomplete armed inventory")
	}
	moves := map[callharness.OperationIdentity]bool{}
	for _, m := range c.Moves {
		if m.Identity == nil || !m.Identity.Valid() || !armed[*m.Identity] || moves[*m.Identity] || m.Kind != "move" && m.Kind != "takeover" {
			return fmt.Errorf("move differs from armed inventory")
		}
		moves[*m.Identity] = true
		d := m.Distributed
		if d == nil || !gapCollections(d.Gaps) || len(d.Gaps) != len(tracks) {
			return fmt.Errorf("incomplete event collections")
		}
		for _, g := range d.Gaps {
			if !tracks[g.Kind] {
				return fmt.Errorf("gap differs from track inventory")
			}
		}
		if m.Receipt != nil && (m.Receipt.Identity != *m.Identity || m.Receipt.Kind != m.Kind) {
			return fmt.Errorf("caller receipt identity mismatch")
		}

	}
	if len(moves) != len(armed) {
		return fmt.Errorf("armed event without move")
	}
	return nil
}
func callCollections(c *callharness.Report) error {
	for _, t := range c.Tracks {
		if t.Kind == "video" && t.Video == nil {
			return fmt.Errorf("missing video evidence")
		}
		if len(t.Arrivals) > t.Packets {
			return fmt.Errorf("arrival count exceeds packets")
		}
	}
	var first map[callharness.FirstBaselines]bool
	for _, m := range c.Moves {
		d := m.Distributed
		if d == nil || m.Measurement.ObservedUntil <= 0 || m.Measurement.ObservedUntil > c.HungUpAt || m.Kind == "takeover" && d.KillToLive == nil || !m.Measurement.VideoExpected || !d.Mapped || d.Start.Earliest.IsZero() || d.Start.Latest.IsZero() || d.End.Earliest.IsZero() || d.End.Latest.IsZero() || d.End.Latest.Before(d.Start.Earliest) || d.WindowOutcome != "pass" && d.WindowOutcome != "fail" && d.WindowOutcome != "inconclusive" {
			return fmt.Errorf("incomplete event mapping")
		}
		for _, name := range []string{"resume", "first_content", "first_new_content", "first_live"} {
			if _, ok := d.Durations[name]; !ok {
				return fmt.Errorf("missing event duration bound")
			}
		}
		if len(d.FirstBaselines) == 0 || d.Evaluations < len(d.FirstBaselines) {
			return fmt.Errorf("missing classification population")
		}
		refs := map[callharness.FirstBaselines]bool{}
		for _, ref := range d.FirstBaselines {
			if refs[ref] || ref.Audio.Samples <= 0 || ref.Video.Samples <= 0 || ref.Audio.Median > ref.Audio.P95 || ref.Video.Median > ref.Video.P95 {
				return fmt.Errorf("inconsistent first baselines")
			}
			refs[ref] = true
		}
		if first == nil {
			first = refs
		} else if !reflect.DeepEqual(first, refs) {
			return fmt.Errorf("first baseline population changed")
		}
		if err := contentCollections(m.Measurement.ContentLoss); err != nil {
			return err
		}
	}
	return nil
}

func contentCollections(e callharness.ContentLossEvidence) error {
	check := func(w callharness.ContentLossWindow) bool {
		if w.End < w.Start {
			return false
		}
		for _, c := range []callharness.ContentLossCounts{w.Audio, w.Video} {
			if c.Lost > c.Sent || c.LongestLostRun > c.Lost {
				return false
			}
		}
		return true
	}
	if !check(e.Event) {
		return fmt.Errorf("invalid content denominator")
	}
	var end time.Duration
	baselines := slices.Clone(e.Baselines)
	sort.Slice(baselines, func(i, j int) bool { return baselines[i].Start < baselines[j].Start })
	for i, b := range baselines {
		if !check(b) || b.End-b.Start != e.Event.End-e.Event.Start || b.End > e.Event.Start || i > 0 && b.Start < end {
			return fmt.Errorf("inconsistent content baseline windows")
		}
		end = b.End
	}
	return nil
}

func reportCollections(r *loadgen.RunReport) error {
	return reportCollectionsFor(r, false)
}
func reportCollectionsFor(r *loadgen.RunReport, relay bool) error {
	if len(r.Processes) == 0 || !relay && len(r.Events) == 0 || relay && (r.Callers != 1 || len(r.Events) != 0) {
		return fmt.Errorf("missing report collections")
	}
	calls := map[string]*callharness.Report{}
	run := ""
	moves := map[callharness.OperationIdentity]string{}
	for _, p := range r.Processes {
		if !p.Complete || len(p.Calls) == 0 || p.ExpectedCalls > 0 && len(p.Calls) != p.ExpectedCalls {
			return fmt.Errorf("incomplete process calls")
		}
		callIDs := map[int]string{}
		for _, m := range p.Calls {
			if m.CallID < 0 || m.CallID >= len(p.Calls) || callIDs[m.CallID] != "" || m.PID != p.PID {
				return fmt.Errorf("invalid caller message identity")
			}
			callIDs[m.CallID] = m.Session
			if m.Session == "" || calls[m.Session] != nil {
				return fmt.Errorf("ambiguous caller sessions")
			}
			if relay {
				if err := callerCollections(m.Call); err != nil {
					return err
				}
				if len(m.Call.Moves) != 0 || len(m.Call.Distributed.Armed) != 0 || len(m.Call.Distributed.Missing) != 0 || m.Call.Distributed.ClockError != "" {
					return fmt.Errorf("relay phase contains trials or incomplete clocks")
				}
			} else if err := trialCollections(m.Call); err != nil {
				return err
			}
			if run != "" && run != m.Call.Distributed.RunID {
				return fmt.Errorf("mixed report runs")
			}
			run = m.Call.Distributed.RunID
			calls[m.Session] = m.Call
			for _, move := range m.Call.Moves {
				id := *move.Identity
				if id.SessionID != m.Session || moves[id] != "" {
					return fmt.Errorf("move session mismatch")
				}
				moves[id] = move.Kind
			}
		}
		if len(p.Dials) > 0 {
			if len(p.Dials) != len(p.Calls) {
				return fmt.Errorf("incomplete dial inventory")
			}
			seen := map[int]bool{}
			for _, d := range p.Dials {
				if seen[d.CallID] || d.PID != p.PID || d.Dial == nil || d.Dial.Outcome != callharness.DialConnected || callIDs[d.CallID] != d.Session || d.Dial.Session != d.Session {
					return fmt.Errorf("dial and call inventories differ")
				}
				seen[d.CallID] = true
			}
		}
	}
	if len(calls) != r.Callers {
		return fmt.Errorf("caller total differs from inventory")
	}
	events := map[callharness.OperationIdentity]bool{}
	for _, e := range r.Events {
		if e.Affected <= 0 || e.Affected != len(e.Sessions) || len(e.Sessions) != len(e.Identities) {
			return fmt.Errorf("incomplete event membership")
		}
		sessions := map[string]bool{}
		for _, s := range e.Sessions {
			if s == "" || sessions[s] || calls[s] == nil {
				return fmt.Errorf("invalid event sessions")
			}
			sessions[s] = true
		}
		kind := "move"
		if e.Event.Kind == "kill" {
			kind = "takeover"
		} else if e.Event.Kind != "move" && e.Event.Kind != "drain" {
			return fmt.Errorf("invalid event kind")
		}
		seen := map[string]bool{}
		for _, id := range e.Identities {
			if !id.Valid() || !sessions[id.SessionID] || seen[id.SessionID] || events[id] || moves[id] != kind {
				return fmt.Errorf("event identity mismatch")
			}
			seen[id.SessionID], events[id] = true, true
		}
	}
	if len(events) != len(moves) {
		return fmt.Errorf("event and move inventories differ")
	}
	return nil
}
func phaseCollections(input *previewMediaInput, r *loadgen.RunReport) error {
	if input.Missing || input.RunID == "" || len(input.Sessions) < 1 || len(input.Sessions) > 2 || len(input.Probes) < 10 || len(input.Probes) > 32 {
		return fmt.Errorf("incomplete registration collections")
	}

	if err := reportCollectionsFor(r, input.Gate == "relay-deploy"); err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, s := range input.Sessions {
		if s == "" || expected[s] {
			return fmt.Errorf("invalid expected sessions")
		}
		expected[s] = true
	}
	receipts := map[callharness.OperationIdentity]receipt{}
	for _, v := range input.Receipts {
		if !v.Identity.Valid() || v.Identity.RunID != input.RunID || !expected[v.Identity.SessionID] || v.Result.ID != v.Identity.SessionID || v.Result.From != v.Identity.Worker {
			return fmt.Errorf("invalid receipt scope")
		}
		if _, ok := receipts[v.Identity]; ok {
			return fmt.Errorf("duplicate receipt")
		}
		receipts[v.Identity] = v
	}
	count := 0
	for _, p := range r.Processes {
		for _, message := range p.Calls {
			c := message.Call
			if !expected[message.Session] || c.Distributed.RunID != input.RunID || !matchesProbePrefix(input.Probes, c) {
				return fmt.Errorf("caller registration mismatch")
			}
			for _, m := range c.Moves {
				v, ok := receipts[*m.Identity]
				if !ok || v.Kind != m.Kind || v.Result.Kind != m.Kind {
					return fmt.Errorf("receipt and move inventories differ")
				}
				if m.Receipt != nil && (!m.Receipt.Start.Equal(v.Start) || !m.Receipt.End.Equal(v.End) || !reflect.DeepEqual(m.Receipt.Result, v.Result) || m.Receipt.Uncertain != v.Uncertain) {
					return fmt.Errorf("caller and server receipts differ")
				}
				count++
			}
		}
	}
	if count != len(receipts) {
		return fmt.Errorf("receipt without move")
	}
	return nil
}
