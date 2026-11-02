// Compiled in a temporary snapshot of a published release by judgePreviewMedia.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

type previewMediaSummary struct {
	CompleteMedia      bool              `json:"completeMedia"`
	LostCalls          int               `json:"lostCalls"`
	UndecidedMetrics   []undecidedMetric `json:"undecidedMetrics"`
	IncludedPhases     int               `json:"includedPhases"`
	DroppedPhases      int               `json:"droppedPhases"`
	ExcludedEvents     int               `json:"excludedEvents"`
	SaturationOverlaps int               `json:"saturationOverlaps"`
	DuplicatePackets   uint64            `json:"duplicatePackets"`
	PhaseDiagnostics   []phaseDiagnostic `json:"phaseDiagnostics"`
	exclusionCause     string
	issue              observe.EvidenceIssue

	Status              string `json:"status"`
	QualifiedMoves      int    `json:"qualifiedMoves"`
	QualifiedTakeovers  int    `json:"qualifiedTakeovers"`
	ExpectedCalls       int    `json:"expectedCalls"`
	CompletedCalls      int    `json:"completedCalls"`
	RelayCalls          int    `json:"relayCalls"`
	EventCount          int    `json:"eventCount"`
	MetricCount         int    `json:"metricCount"`
	FailedMetrics       int    `json:"failedMetrics"`
	InconclusiveMetrics int    `json:"inconclusiveMetrics"`
	MaxMediaGapNs       int64  `json:"maxMediaGapNs"`
	DecryptionFailures  int    `json:"decryptionFailures"`
	ICERestarts         int    `json:"iceRestarts"`
}

type phaseDiagnostic struct {
	Gate               string                `json:"gate"`
	EventCount         int                   `json:"eventCount"`
	Phase              int                   `json:"phase"`
	Included           bool                  `json:"included"`
	Cause              string                `json:"cause"`
	ExcludedEvents     int                   `json:"excludedEvents"`
	SaturationOverlaps int                   `json:"saturationOverlaps"`
	Issue              observe.EvidenceIssue `json:"issue"`
}

// Rejudge raw reports, never trust a serialized outcome. This returns MEDIA
// evidence only; no host procedure, durability or capacity pass is implied.
func previewMedia(r *loadgen.RunReport) previewMediaSummary {
	return previewMediaFor(r, false)
}
func previewMediaFor(r *loadgen.RunReport, relay bool) previewMediaSummary {
	knownFailure := reportFailure(r)
	validNumbers := observe.Numbers(r) == nil
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			validNumbers = validNumbers && callNumbers(c.Call) == nil
		}
	}
	if !validNumbers || reportCollectionsFor(r, relay) != nil {
		status := "inconclusive"
		if knownFailure {
			status = "failed"
		}
		return previewMediaSummary{Status: status}
	}
	if relay {
		// Relay calls supply no trial metrics. Their completion and generator
		// checks still apply; do not invoke the distributed trial judge.
		r.Outcome, r.Yardstick, r.Distributed = "valid", nil, nil
		for _, p := range r.Processes {
			if p.Saturated || len(p.Causes) > 0 {
				r.Outcome = "inconclusive"
			}
		}
	} else {
		r.Judge()
	}
	completeMedia := true
	knownLoss := knownFailure
	out := previewMediaSummary{Status: "inconclusive", ExpectedCalls: r.Callers, EventCount: len(r.Events), MetricCount: len(r.Yardstick)}
	add := func(target *int, n int) {
		sum, ok := observe.AddCount(int64(*target), int64(n))
		if !ok {
			completeMedia = false
			return
		}
		*target = int(sum)
	}
	for _, p := range r.Processes {
		add(&out.CompletedCalls, len(p.Calls))
		for _, c := range p.Calls {
			if c.Call == nil {
				completeMedia = false
				continue
			}
			for _, n := range []int{c.Call.DecryptionFailures.AuthTag, c.Call.DecryptionFailures.Replay, c.Call.DecryptionFailures.Other} {
				add(&out.DecryptionFailures, n)
			}
			add(&out.ICERestarts, c.Call.ICERestarts)
			audio, video := false, false
			for _, t := range c.Call.Tracks {
				out.DuplicatePackets += uint64(t.DuplicatePackets)
				if t.Video != nil && t.Video.OnlineDecode.Errors > 0 {
					completeMedia = false
				}
				audio = audio || t.Kind == "audio" && t.Packets > 0
				video = video || t.Kind == "video" && t.Packets > 0
				out.MaxMediaGapNs = max(out.MaxMediaGapNs, int64(t.MediaGap))
			}
			if !audio || !video {
				completeMedia = false
			}
			for _, m := range c.Call.Moves {
				if m.Distributed != nil && m.Distributed.WindowOutcome == "pass" {
					if m.Kind == "move" {
						add(&out.QualifiedMoves, 1)
					}
					if m.Kind == "takeover" {
						add(&out.QualifiedTakeovers, 1)
					}
				}
				knownLoss = knownLoss || m.Lost || m.DecryptionFailuresAfterResume > 0
				if !m.Measurement.VideoExpected {
					completeMedia = false
				}
			}
		}
	}
	for _, row := range r.Yardstick {
		if row.Outcome == "fail" {
			add(&out.FailedMetrics, 1)
		}
		if row.Outcome == "inconclusive" {
			add(&out.InconclusiveMetrics, 1)
		}
	}
	out.CompleteMedia = completeMedia
	for _, p := range r.Processes {
		out.CompleteMedia = out.CompleteMedia && p.Complete && len(p.Causes) == 0
	}
	out.UndecidedMetrics = undecidedRows(r)
	if relay {
		out.RelayCalls = out.CompletedCalls
	}
	trialsPass := r.Distributed != nil && r.Distributed.Outcome == "pass" && out.EventCount > 0 && out.MetricCount > 0
	if completeMedia && r.Outcome == "valid" && (relay || trialsPass) && out.ExpectedCalls > 0 && out.InconclusiveMetrics == 0 {
		out.Status = "passed"
	}
	// Public judge's trusted failures survive invalid/missing generator evidence.
	if knownLoss || out.FailedMetrics > 0 || r.Distributed != nil && r.Distributed.Outcome == "fail" || out.DecryptionFailures > 0 || out.ICERestarts > 0 {
		out.Status = "failed"
	}
	return out
}

type previewMediaInput struct {
	Gate        string                `json:"gate,omitempty"`
	RunID       string                `json:"runId"`
	Sessions    []string              `json:"sessions"`
	Probes      []registrationProbe   `json:"probes"`
	CallerJSONL string                `json:"callerJsonl"`
	Events      []loadgen.EventResult `json:"events"`
	Receipts    []previewReceipt      `json:"receipts"`
	Missing     bool                  `json:"missing,omitempty"`
	issue       observe.EvidenceIssue
}
type previewReceipt = receipt

func previewGeneratorGuards(r *loadgen.RunReport, receipts []receipt, samples []json.RawMessage) bool {
	return previewGeneratorDetails(r, receipts, samples, nil)
}

func previewGeneratorDetails(r *loadgen.RunReport, receipts []receipt, samples []json.RawMessage, details *previewMediaSummary) bool {
	dir, err := collectGeneratorSamples(samples)
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	if len(r.Processes) != 1 {
		return false
	}
	budgets, excluded := applySaturation(dir, r, receipts, []string{"caller-1"})
	passed := len(budgets) == 1 && budgets[0].Outcome == "pass" && len(excluded) == 0
	if details != nil {
		details.ExcludedEvents = len(excluded)
		if !passed {
			details.exclusionCause = "generator-guard"
		}
		for _, e := range excluded {
			if e.Cause == "generator saturation overlaps event window" {
				details.SaturationOverlaps++
			}
		}
		if details.SaturationOverlaps > 0 {
			details.exclusionCause = "generator saturation overlaps event window"
		}
	}
	return passed
}

// Both entry points use the same collected sample boundary, including hashes.
func collectGeneratorSamples(samples []json.RawMessage) (string, error) {
	dir, err := os.MkdirTemp("", "preview-generator-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	job := filepath.Join(dir, "caller-1")
	if os.MkdirAll(filepath.Join(job, "stats"), 0700) != nil {
		return "", fmt.Errorf("generator stats directory unavailable")
	}
	var manifest []map[string]any
	for i, raw := range samples {
		name := fmt.Sprintf("%05d.json", i)
		sum := sha256.Sum256(raw)
		if os.WriteFile(filepath.Join(job, "stats", name), raw, 0600) != nil {
			return "", fmt.Errorf("generator sample publication failed")
		}
		manifest = append(manifest, map[string]any{"name": name, "sha256": fmt.Sprintf("%x", sum), "bytes": len(raw)})
	}
	data, _ := json.Marshal(manifest)
	if os.WriteFile(filepath.Join(job, "stats-manifest.json"), data, 0600) != nil {
		return "", fmt.Errorf("generator manifest publication failed")
	}
	ok = true
	return dir, nil
}

func parsePreviewMedia(input *previewMediaInput) (*loadgen.RunReport, previewMediaSummary, bool) {
	return parsePreviewMediaFor(input)
}

func parsePreviewMediaFor(input *previewMediaInput) (*loadgen.RunReport, previewMediaSummary, bool) {
	r := &loadgen.RunReport{Callers: len(input.Sessions), Events: input.Events, Limits: loadgen.DefaultLimits()}
	var numericFailure bool
	trials := failureInput(input)
	var sampleDuration, sampleRequests int64
	var first loadgen.Message
	var parseErr error
	issue := input.issue
	reject := func(err error, path string) {
		if parseErr == nil && issue.Path == "" {
			issue = phaseIssue(err, path)
		}
		parseErr = err
	}
	if err := observe.Numbers(input.Probes); err != nil {
		reject(err, "$.probes")
	}
	validEvents := make([]loadgen.EventResult, 0, len(input.Events))
	for index, event := range input.Events {
		if err := observe.Numbers(event); err != nil {
			reject(err, fmt.Sprintf("$.events[%d]", index))
		} else {
			validEvents = append(validEvents, event)
		}
	}
	r.Events = validEvents
	validReceipts := make([]receipt, 0, len(input.Receipts))
	for index, receipt := range input.Receipts {
		if err := observe.Numbers(receipt); err != nil {
			reject(err, fmt.Sprintf("$.receipts[%d]", index))
		} else {
			validReceipts = append(validReceipts, receipt)
		}
	}
	var checked strings.Builder
	// Reuse collected readSample's explicit-field checks. A missing zero-valued
	// CPU/pacing field cannot silently become a clean generator sample.
	decoder := json.NewDecoder(strings.NewReader(input.CallerJSONL))
	var samples []json.RawMessage
	for row := 0; ; row++ {
		path := fmt.Sprintf("$.callerJsonl[%d]", row)
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			reject(err, path)
			break
		}
		var m loadgen.Message
		raw, err = decodeCallerMessage(raw, &m, input.Gate == "relay-deploy")
		if m.Call != nil && (m.Call.Distributed == nil || m.Call.Distributed.RunID == input.RunID) {
			for _, session := range input.Sessions {
				if m.Session == session && callFailure(m.Call) {
					numericFailure = true
				}
			}
		}
		if err != nil {
			reject(err, path)
			continue
		}
		if err = observe.Numbers(m); err == nil {
			err = callNumbers(m.Call)
		}
		if err != nil {
			reject(err, path)
			continue
		}
		if m.Type == "stats" {
			sample, err := sampleFromMessage(m, first.PID)
			if err != nil {
				reject(err, path)
				continue
			}
			duration, dok := observe.AddCount(sampleDuration, int64(sample.Interval))
			requests, rok := observe.AddCount(sampleRequests, int64(sample.IntervalStatusRequests))
			if !dok || !rok {
				reject(fmt.Errorf("generator totals overflow"), path+".stats")
				continue
			}
			sampleDuration, sampleRequests = duration, requests
			samples = append(samples, raw)
		}
		if first.PID == 0 {
			first = m
		}
		checked.Write(raw)
		checked.WriteByte('\n')
	}
	if first.PID > 0 {
		p, err := loadgen.ReadSoakProcess(strings.NewReader(checked.String()), first.PID, len(input.Sessions), r.Limits)
		if err != nil {
			reject(err, "$.callerJsonl")
		}
		r.Processes = []loadgen.ProcessReport{p}
	}
	collectionsErr := phaseCollections(input, r)
	if collectionsErr != nil {
		reject(collectionsErr, collectionPath(collectionsErr))
	}
	lost := lostCallCount(input, r)
	rawVerdict := previewMediaSummary{Status: "inconclusive"}
	out := rawVerdict
	guards := false
	if collectionsErr == nil && parseErr == nil {
		rawVerdict = previewMediaFor(r, input.Gate == "relay-deploy")
		guards = previewGeneratorDetails(r, validReceipts, samples, &out)
		judged := previewMediaFor(r, input.Gate == "relay-deploy")
		judged.ExcludedEvents = out.ExcludedEvents
		judged.SaturationOverlaps = out.SaturationOverlaps
		judged.exclusionCause = out.exclusionCause
		out = judged
	}
	if rawVerdict.Status == "failed" || numericFailure || trialsFailed(trials) {
		out.Status = "failed"
	}
	qualified := parseErr == nil && guards
	if input.Gate == "relay-deploy" && out.Status != "passed" {
		qualified = false
		out.exclusionCause = "incomplete-relay-call"
	}
	if !qualified && out.exclusionCause == "" {
		out.exclusionCause = "invalid-phase-evidence"

	}
	if !qualified && out.Status != "failed" {
		out.Status = "inconclusive"
	}
	expected := map[string]bool{}
	for _, session := range input.Sessions {
		expected[session] = true
	}
	// Server loss remains conclusive even when the caller's report was lost.
	for _, r := range input.Receipts {
		if !r.Identity.Valid() || r.Identity.RunID != input.RunID || !expected[r.Identity.SessionID] {
			continue
		}
		if uncertainReceipt(r) && out.Status != "failed" {
			if out.exclusionCause == "" {
				out.exclusionCause = "uncertain-receipt"
			}
			out.Status = "inconclusive"
			qualified = false
		}
		if r.Result.Lost {
			out.Status = "failed"
		}
	}
	out.LostCalls = lost
	out.issue = publicPhaseIssue(issue)
	if !qualified && out.exclusionCause == "invalid-phase-evidence" && out.issue.Path == "" {
		out.issue = observe.EvidenceIssue{Path: "$", Reason: "qualification-incomplete"}
	}
	return r, out, qualified
}
func joinedPreviewMedia(input *previewMediaInput) previewMediaSummary {
	_, out, _ := parsePreviewMedia(input)
	return out
}

type previewPoolInput struct {
	RunID   string              `json:"runId"`
	Phases  []previewMediaInput `json:"phases"`
	Missing bool                `json:"missing"`
}

// Assemble RAW processes and receipts first; invoke the unchanged public policy
// once for trial phases. Relay calls have no trials and only add completed calls.
func pooledPreviewMedia(input *previewPoolInput) previewMediaSummary {
	r := &loadgen.RunReport{Limits: loadgen.DefaultLimits()}
	valid, failed := input.RunID != "" && len(input.Phases) > 0 && !input.Missing, poolFailure(input)
	diagnostics := []phaseDiagnostic{}
	included, dropped, excluded, overlaps := 0, 0, 0, 0
	var duplicates uint64
	completeMedia, lost := true, 0
	type eventPosition struct{ phase, event int }
	var positions []eventPosition
	var relayCalls int
	var relayGap int64
	sessions, operations := map[string]bool{}, map[string]bool{}
	for i := range input.Phases {
		phase := &input.Phases[i]
		part, verdict, qualified := parsePreviewMedia(phase)
		valid = valid && qualified && phase.RunID == input.RunID
		failed = failed || phase.RunID == input.RunID && verdict.Status == "failed"
		for _, session := range phase.Sessions {
			if sessions[session] {
				valid = false
			}
			sessions[session] = true
		}
		lost += verdict.LostCalls
		completeMedia = completeMedia && verdict.CompleteMedia
		duplicates += verdict.DuplicatePackets
		excluded += verdict.ExcludedEvents
		overlaps += verdict.SaturationOverlaps
		diag := phaseDiagnostic{Gate: phase.Gate, EventCount: len(part.Events), Phase: i, ExcludedEvents: verdict.ExcludedEvents, SaturationOverlaps: verdict.SaturationOverlaps, Cause: verdict.exclusionCause, Issue: verdict.issue}
		// Replayed identities never increase the acceptance sample. Reject the whole
		// ambiguous phase as evidence, retaining its known failures independently.
		duplicate := false
		for _, event := range part.Events {
			for _, id := range event.Identities {
				key := id.RunID + "/" + id.OperationID + "/" + id.SessionID
				if operations[key] {
					duplicate = true
				}
				operations[key] = true
			}
		}
		if duplicate {
			valid = false
			dropped++
			diag.Cause = "duplicate-operation-identity"
			diagnostics = append(diagnostics, diag)
			continue
		}
		if !qualified || phase.RunID != input.RunID {
			dropped++
			if phase.RunID != input.RunID {
				diag.Cause = "phase-run-differs"
			}
			diagnostics = append(diagnostics, diag)
			continue
		}
		count, ok := observe.AddCount(int64(r.Callers), int64(part.Callers))
		if !ok {
			valid = false
			dropped++
			diag.Cause = "caller-count-overflow"
			diagnostics = append(diagnostics, diag)
			continue
		}
		included++
		diag.Included = true
		diag.Cause = ""
		diagnostics = append(diagnostics, diag)
		if phase.Gate == "relay-deploy" {
			relayCalls += verdict.RelayCalls
			relayGap = max(relayGap, verdict.MaxMediaGapNs)
			continue
		}
		r.Callers = int(count)
		r.Processes = append(r.Processes, part.Processes...)
		for j := range part.Events {
			positions = append(positions, eventPosition{i, j})
		}
		r.Events = append(r.Events, part.Events...)
	}
	out := previewMediaSummary{Status: "inconclusive"}
	if r.Callers > 0 {
		out = previewMedia(r)
	} else if relayCalls > 0 {
		out.Status = "passed"
	}
	out.ExpectedCalls += relayCalls
	out.CompletedCalls += relayCalls
	out.RelayCalls = relayCalls
	out.MaxMediaGapNs = max(out.MaxMediaGapNs, relayGap)
	if !valid && out.Status != "failed" {
		out.Status = "inconclusive"
	}
	if failed {
		out.Status = "failed"
	}
	out.IncludedPhases = included
	out.DroppedPhases = dropped
	out.ExcludedEvents = excluded
	out.SaturationOverlaps = overlaps
	out.DuplicatePackets = duplicates
	out.PhaseDiagnostics = diagnostics
	out.CompleteMedia = out.CompleteMedia && completeMedia && valid
	if r.Callers == 0 {
		out.CompleteMedia = completeMedia && valid
	}
	out.LostCalls = lost
	if out.UndecidedMetrics == nil {
		out.UndecidedMetrics = []undecidedMetric{}
	}
	for j := range out.UndecidedMetrics {
		row := &out.UndecidedMetrics[j]
		if row.Event < len(positions) {
			pos := positions[row.Event]
			row.Phase, row.Event = pos.phase, pos.event
		}
	}
	return out
}
func decodePool(data []byte) (input previewPoolInput, err error) {
	wire, err := decodePoolEnvelope(data)
	if err != nil {
		return input, err
	}

	input.RunID, input.Missing = wire.RunID, wire.Missing
	input.Phases = make([]previewMediaInput, len(wire.Phases))
	for i, raw := range wire.Phases {
		phase := &input.Phases[i]
		if problem := observe.DecodeEvidenceLimit(raw, phase, 64<<20); problem != nil {
			input.Missing = true
			phase.Missing = true
			phase.issue = observe.DescribeEvidence(problem)
		}
	}
	return
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--calibration" {
		data, err := os.ReadFile(os.Args[2])
		if err != nil || len(data) > 64<<20 {
			os.Exit(1)
		}
		if json.NewEncoder(os.Stdout).Encode(calibrateGenerator(string(data))) != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "preview media input required")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "preview media input unavailable")
		os.Exit(1)
	}
	input, err := decodePool(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "preview media input invalid")
		os.Exit(1)
	}
	// No arbitrary strings, session identities, server addresses or causes leave
	// the collected raw input. Only the numeric measurements above are emitted.
	if err := json.NewEncoder(os.Stdout).Encode(pooledPreviewMedia(&input)); err != nil {
		os.Exit(1)
	}
}
