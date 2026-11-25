package relayjudge

import (
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/internal/relayloss"
	"github.com/relaishq/relais/pkg/operator"
)

// DecodeInput rejects ambiguous members without extending the shared report
// decoder's exception ledger. Each caller record is checked separately below.
func DecodeInput(raw []byte) (Input, error) {
	var in Input
	err := observe.DecodeClosedLimit(raw, &in, observe.Contract{"version", "runId", "scenario", "pid", "sessions?", "preflight?", "preflight[].session", "preflight[].slot", "preflight[].generation", "targetSlot", "survivorEndpoint", "restore", "targetInsufficient", "lostSessions?", "callerJsonl", "samples?"}, operator.MaxBlobBytes)
	return in, err
}
func (r *CallResult) add(outcome, cause string) {
	if !slices.Contains(r.Causes, cause) {
		r.Causes = append(r.Causes, cause)
	}
	if outcome == "fail" || r.Outcome != "fail" && outcome == "inconclusive" {
		r.Outcome = outcome
	}
}
func duration(v *time.Duration) bool { return v != nil && *v >= 0 && int64(*v) <= observe.MaxInteger }
func count(v *int) bool              { return v != nil && *v >= 0 && int64(*v) <= observe.MaxInteger }
func positive(v *int) bool           { return count(v) && *v > 0 }
func validSlot(s string) bool        { return s == "relay" || s == "relay-two" }

func Judge(in Input) Result {
	out := Result{RunID: in.RunID, Outcome: "pass", Causes: []string{}, Calls: make([]CallResult, 4)}
	assignments := map[string]Assignment{}
	counts := map[string]int{}
	valid := in.Version == 1 && in.RunID != "" && in.PID > 0 && int64(in.PID) <= observe.MaxInteger && len(in.Sessions) == 4 && len(in.Preflight) == 4 && validSlot(in.TargetSlot)
	if endpoint, err := netip.ParseAddrPort(in.SurvivorEndpoint); err != nil || endpoint.Port() == 0 {
		valid = false
	}
	s, err := operator.BuildScenarioIdentity(in.Scenario, in.Scenario.ClockReceiptSHA256)
	valid = valid && err == nil && s.Digest == in.Scenario.Digest && in.Scenario.RelayTwo && slices.Equal(in.Scenario.Gates, []operator.Gate{operator.RelayHostLoss}) && in.Scenario.Caller.SourceCommit != "" && in.Scenario.Judge.SourceCommit == in.Scenario.Caller.SourceCommit
	for _, a := range in.Preflight {
		if a.Session == "" || assignments[a.Session].Session != "" || !validSlot(a.Slot) || a.Generation >= uint64(observe.MaxInteger) {
			valid = false
		}
		assignments[a.Session] = a
		counts[a.Slot]++
	}
	seen := map[string]bool{}
	for i := range out.Calls {
		r := &out.Calls[i]
		*r = CallResult{Ordinal: i, Role: "unknown", Outcome: "pass", Causes: []string{}}
		if i < len(in.Sessions) {
			r.Session = in.Sessions[i]
		}
		if r.Session == "" || seen[r.Session] || assignments[r.Session].Session == "" {
			valid = false
		}
		seen[r.Session] = true
	}
	lost := map[string]bool{}
	for _, session := range in.LostSessions {
		if !seen[session] || lost[session] {
			valid = false
		}
		lost[session] = true
	}
	if counts[in.TargetSlot] < 2 || counts[in.TargetSlot] < counts["relay"] || counts[in.TargetSlot] < counts["relay-two"] {
		in.TargetInsufficient = true
	}
	messages, streamOK := readMessages(in)
	generatorOK := qualifyGenerator(in, messages)
	for i := range out.Calls {
		r := &out.Calls[i]
		if valid {
			r.Role = "unaffected"
			if assignments[r.Session].Slot == in.TargetSlot {
				r.Role = "affected"
			}
		}
		if !valid || !streamOK {
			r.add("inconclusive", "relay-loss-missing")
		}
		if !generatorOK {
			r.add("inconclusive", "relay-loss-generator")
		}
		if in.TargetInsufficient {
			r.add("inconclusive", "relay-loss-target-insufficient")
		}
		if in.Restore == "failed" {
			r.add("fail", "relay-loss-restore-failed")
		} else if in.Restore != "restored" {
			r.add("inconclusive", "relay-loss-missing")
		}
		if slices.Contains(in.LostSessions, r.Session) {
			r.add("fail", "relay-loss-call-lost")
		}
		m, ok := messages[i]
		if !ok || m.Session != r.Session {
			r.add("inconclusive", "relay-loss-missing")
		} else {
			evaluateCall(r, m, assignments[r.Session], in.SurvivorEndpoint, in.TargetInsufficient)
		}
		if r.Outcome == "fail" {
			out.Outcome = "fail"
		} else if r.Outcome == "inconclusive" && out.Outcome != "fail" {
			out.Outcome = "inconclusive"
		}
		for _, c := range r.Causes {
			if !slices.Contains(out.Causes, c) {
				out.Causes = append(out.Causes, c)
			}
		}
	}
	return out
}

func readMessages(in Input) (map[int]message, bool) {
	calls := map[int]message{}
	dialed := map[int]bool{}
	ok, done := true, false
	decoder := json.NewDecoder(strings.NewReader(in.CallerJSONL))
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			ok = false
			break
		}
		var m message
		err = observe.DecodeLimit(raw, &m, nil, operator.MaxBlobBytes)
		// Number overflow leaves independent siblings readable; ambiguity never does.
		var typeErr *json.UnmarshalTypeError
		var timeErr *time.ParseError
		if err != nil && !errors.As(err, &typeErr) && !errors.As(err, &timeErr) {
			ok = false
			continue
		}
		m.invalid = err != nil
		if m.invalid {
			m.recoveryInvalid = !recoveryFieldsValid(raw, &m)
		}
		if done {
			ok = false
		}
		if m.PID != in.PID || observe.Require(raw, observe.Contract{"type", "pid", "~call_id"}) != nil {
			ok = false
			continue
		}
		switch m.Type {
		case "call":
			if m.CallID < 0 || m.CallID >= 4 {
				ok = false
				continue
			}
			if first, exists := calls[m.CallID]; exists {
				ok = false
				first.extra = append(first.extra, m)
				calls[m.CallID] = first
				continue
			}
			calls[m.CallID] = m
		case "dial":
			if m.CallID < 0 || m.CallID >= 4 || dialed[m.CallID] || m.Dial == nil || m.Dial.Outcome != "connected" || m.Error != "" || m.CallID >= len(in.Sessions) || m.Session != in.Sessions[m.CallID] || m.Dial.Session != m.Session {
				ok = false
			}
			dialed[m.CallID] = true
		case "stats":
		case "done":
			done = true
		default:
			ok = false
		}
	}
	// An absent dial population is allowed by the existing judge. A partial one
	// is not. Independently collected stats must exactly match the inline stream.
	if len(dialed) != 0 && len(dialed) != 4 {
		ok = false
	}
	return calls, ok && done && len(calls) == 4
}

func evaluateCall(row *CallResult, m message, a Assignment, survivor string, noFault bool) {
	missing := func() { row.add("inconclusive", "relay-loss-missing") }
	for _, duplicate := range m.extra {
		if duplicate.Session == row.Session {
			evaluateCall(row, duplicate, a, survivor, noFault)
		}
	}
	c := m.Call
	if c == nil {
		missing()
		return
	}
	if m.Error != "" || m.invalid {
		missing()
	}
	d := c.DecryptionFailures
	if d == nil {
		missing()
	} else {
		if positive(d.AuthTag) || positive(d.Replay) || positive(d.Other) {
			row.add("fail", "relay-loss-decryption")
		}
		if !count(d.AuthTag) || !count(d.Replay) || !count(d.Other) {
			missing()
		}
	}
	for _, move := range c.Moves {
		if move.Lost {
			row.add("fail", "relay-loss-call-lost")
		}
		if move.DecryptionFailuresAfterResume > 0 {
			row.add("fail", "relay-loss-decryption")
		}
		v := move.Measurement
		facts := []struct{ trusted, failed bool }{
			{v.AudioLoss.Trusted, v.AudioLoss.Failed},
			{v.Audio.Verdict.Trusted, v.Audio.Verdict.Failed},
			{v.ResumeVerdict.Trusted, v.ResumeVerdict.Failed},
			{v.VideoLoss.Trusted, v.VideoLoss.Failed},
			{v.Video.Verdict.Trusted, v.Video.Verdict.Failed},
			{v.FirstContentVerdict.Trusted, v.FirstContentVerdict.Failed},
			{v.FirstNewContentVerdict.Trusted, v.FirstNewContentVerdict.Failed},
			{v.FirstLiveVerdict.Trusted, v.FirstLiveVerdict.Failed},
		}
		for _, fact := range facts {
			if fact.trusted && fact.failed {
				row.add("fail", "relay-loss-call-lost")
			}
		}
		if move.Distributed != nil && move.Distributed.WindowOutcome == "fail" {
			row.add("fail", "relay-loss-call-lost")
		}
	}
	if len(c.Moves) != 0 {
		missing()
	} // R has no move or takeover plan.
	complete := d != nil && count(d.AuthTag) && count(d.Replay) && count(d.Other) &&
		len(c.Moves) == 0 && !m.invalid && m.Error == "" && c.StartedAt != nil && !c.StartedAt.IsZero() &&
		duration(c.FirstVideoSentAt) && *c.FirstVideoSentAt > 0 && c.SentVideo != nil && positive(c.SentVideo.Frames) &&
		duration(c.HungUpAt) && *c.HungUpAt > *c.FirstVideoSentAt && count(c.ICERestarts) && len(c.Tracks) == 2
	tracks := map[string]track{}
	for _, t := range c.Tracks {
		if t.Kind != "audio" && t.Kind != "video" || tracks[t.Kind].Kind != "" || t.SSRC == nil {
			complete = false
		}
		tracks[t.Kind] = t
		if positive(t.DuplicatePackets) {
			row.add("fail", "relay-loss-duplicate")
		}
		if !count(t.Packets) || !positive(t.Packets) || !count(t.DuplicatePackets) || !duration(t.MediaGap) || !duration(t.LastArrival) {
			complete = false
		}
		if t.Kind == "video" && (t.Video == nil || t.Video.OnlineDecode == nil || !count(t.Video.OnlineDecode.Errors) || *t.Video.OnlineDecode.Errors != 0) {
			complete = false
		}
		if duration(t.MediaGap) {
			row.Gap = max(row.Gap, *t.MediaGap)
		}
		if duration(c.HungUpAt) && duration(t.LastArrival) {
			if *c.HungUpAt >= *t.LastArrival {
				row.Gap = max(row.Gap, *c.HungUpAt-*t.LastArrival)
			} else {
				complete = false
			}
		}
	}
	if tracks["audio"].Kind == "" || tracks["video"].Kind == "" {
		complete = false
	}
	if !complete {
		missing()
	}
	if row.Role == "unaffected" || noFault {
		if len(m.Episodes) != 0 || count(c.ICERestarts) && *c.ICERestarts != 0 {
			row.add("inconclusive", "relay-loss-unaffected-restart")
		}
	} else if row.Role == "affected" {
		if len(m.Episodes) != 1 {
			row.add("inconclusive", "relay-loss-episode-count")
		} else {
			e := m.Episodes[0]
			anchors := duration(e.Last) && duration(e.Start) && duration(e.Applied) && duration(e.Video) && duration(e.Audio) && *e.Last > 0 && *e.Last < *e.Start
			binding := e.Generation != nil && *e.Generation == a.Generation+1 && e.Candidate != nil && *e.Candidate == survivor
			if e.Generation == nil || *e.Generation != a.Generation+1 {
				row.add("inconclusive", "relay-loss-generation-mismatch")
			}
			if e.Candidate == nil || *e.Candidate != survivor {
				row.add("inconclusive", "relay-loss-candidate-mismatch")
			}
			if !positive(c.ICERestarts) {
				row.add("inconclusive", "relay-loss-episode-count")
				binding = false
			}
			ordered := anchors && *e.Applied >= *e.Start
			recovered := ordered && *e.Video >= *e.Applied && *e.Audio >= *e.Applied && *e.Video > 0 && *e.Audio > 0
			if recovered && (!duration(tracks["video"].LastArrival) || !duration(tracks["audio"].LastArrival) || *e.Video > *tracks["video"].LastArrival || *e.Audio > *tracks["audio"].LastArrival) {
				recovered = false
				missing()
			}
			if recovered && duration(c.HungUpAt) && (max(*e.Video, *e.Audio) > *c.HungUpAt || *e.Start > *c.HungUpAt) {
				recovered = false
				ordered = false
			}
			if !ordered || anchors && (*e.Video > 0 && *e.Video < *e.Applied || *e.Audio > 0 && *e.Audio < *e.Applied) {
				row.add("inconclusive", "relay-loss-anchor-order")
			}
			countOK := !m.recoveryInvalid && len(e.Counts) == len(c.Tracks) && len(c.Tracks) == 2
			mediaOrderViolated := false
			counted := map[string]bool{}
			for _, n := range e.Counts {
				t := tracks[n.Kind]
				if counted[n.Kind] || t.Kind == "" || n.SSRC == nil || t.SSRC == nil || *n.SSRC != *t.SSRC || !count(n.Count) {
					countOK = false
				}
				counted[n.Kind] = true
				if positive(n.Count) {
					row.add("inconclusive", "relay-loss-media-order-violated")
					mediaOrderViolated = true
				}
			}
			if !countOK {
				missing()
			}
			countOK = countOK && !mediaOrderViolated
			// A complete unanswered attempt can prove non-recovery without an
			// answer binding. A partial answered attempt still needs that binding
			// and the arrival counts; contradictory anchors cannot prove absence.
			unanswered := e.Generation != nil && *e.Generation == 0 && e.Candidate != nil && *e.Candidate == ""
			consistent := anchors && duration(c.HungUpAt) && *e.Start <= *c.HungUpAt &&
				(*e.Applied == 0 && unanswered && *e.Video == 0 && *e.Audio == 0 ||
					*e.Applied >= *e.Start && *e.Applied <= *c.HungUpAt &&
						(*e.Video == 0 || *e.Video >= *e.Applied && *e.Video <= *c.HungUpAt) &&
						(*e.Audio == 0 || *e.Audio >= *e.Applied && *e.Audio <= *c.HungUpAt))
			unrecovered := complete && consistent && countOK && (*e.Applied == 0 || binding) &&
				(*e.Video == 0 || *e.Audio == 0) && !slices.Contains(row.Causes, "relay-loss-media-order-violated") &&
				*c.HungUpAt >= *e.Last &&
				*c.HungUpAt-*e.Last >= 2*relayloss.Budget
			// Only a bound, ordered, zero-early-media episode is a recovery measure.
			// Other independent gap failures still survive missing episode evidence.
			if recovered && binding && countOK {
				row.Gap = max(row.Gap, max(*e.Video, *e.Audio)-*e.Last)
			} else if unrecovered {
				row.add("fail", "relay-loss-unrecovered")
			} else if !mediaOrderViolated && !slices.Contains(row.Causes, "relay-loss-episode-count") {
				missing()
			}
		}
	}
	if row.Gap > relayloss.Budget {
		row.add("fail", "relay-loss-slow")
	}
}

// A malformed snapshot sibling cannot erase an otherwise valid recovery gap.
// Recheck the episode and track identities separately: a malformed arrival
// count or SSRC must never become a fabricated clean zero after Unmarshal.
func recoveryFieldsValid(raw []byte, m *message) bool {
	var parts struct {
		Episodes json.RawMessage `json:"relayLossEpisodes"`
		Call     struct{ Tracks json.RawMessage }
	}
	if json.Unmarshal(raw, &parts) != nil {
		return false
	}
	var episodes []episode
	var inventory []struct {
		Kind string
		SSRC *uint32
	}
	if observe.DecodeLimit(parts.Episodes, &episodes, nil, operator.MaxBlobBytes) != nil ||
		observe.DecodeLimit(parts.Call.Tracks, &inventory, nil, operator.MaxBlobBytes) != nil {
		return false
	}
	m.Episodes = episodes
	return true
}
