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
	Status              string `json:"status"`
	QualifiedMoves      int    `json:"qualifiedMoves"`
	QualifiedTakeovers  int    `json:"qualifiedTakeovers"`
	ExpectedCalls       int    `json:"expectedCalls"`
	CompletedCalls      int    `json:"completedCalls"`
	EventCount          int    `json:"eventCount"`
	MetricCount         int    `json:"metricCount"`
	FailedMetrics       int    `json:"failedMetrics"`
	InconclusiveMetrics int    `json:"inconclusiveMetrics"`
	MaxMediaGapNs       int64  `json:"maxMediaGapNs"`
	DecryptionFailures  int    `json:"decryptionFailures"`
	ICERestarts         int    `json:"iceRestarts"`
}

// Rejudge raw reports, never trust a serialized outcome. This returns MEDIA
// evidence only; no host procedure, durability or capacity pass is implied.
func previewMedia(r *loadgen.RunReport) previewMediaSummary {
	knownFailure := reportFailure(r)
	validNumbers := observe.Numbers(r) == nil
	for _, p := range r.Processes {
		for _, c := range p.Calls {
			validNumbers = validNumbers && callNumbers(c.Call) == nil
		}
	}
	if !validNumbers || reportCollections(r) != nil {
		status := "inconclusive"
		if knownFailure {
			status = "failed"
		}
		return previewMediaSummary{Status: status}
	}
	r.Judge()
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
	if completeMedia && r.Outcome == "valid" && r.Distributed != nil && r.Distributed.Outcome == "pass" && out.ExpectedCalls > 0 && out.EventCount > 0 && out.MetricCount > 0 && out.InconclusiveMetrics == 0 {
		out.Status = "passed"
	}
	// Public judge's trusted failures survive invalid/missing generator evidence.
	if knownLoss || out.FailedMetrics > 0 || r.Distributed != nil && r.Distributed.Outcome == "fail" || out.DecryptionFailures > 0 || out.ICERestarts > 0 {
		out.Status = "failed"
	}
	return out
}

type previewMediaInput struct {
	RunID       string                `json:"runId"`
	Sessions    []string              `json:"sessions"`
	Probes      []registrationProbe   `json:"probes"`
	CallerJSONL string                `json:"callerJsonl"`
	Events      []loadgen.EventResult `json:"events"`
	Receipts    []previewReceipt      `json:"receipts"`
	Missing     bool                  `json:"missing,omitempty"`
}
type previewReceipt = receipt

func previewGeneratorGuards(r *loadgen.RunReport, receipts []receipt, samples []json.RawMessage) bool {
	dir, err := os.MkdirTemp("", "preview-generator-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	job := filepath.Join(dir, "caller-1")
	if os.MkdirAll(filepath.Join(job, "stats"), 0700) != nil {
		return false
	}
	var manifest []map[string]any
	for i, raw := range samples {
		name := fmt.Sprintf("%05d.json", i)
		sum := sha256.Sum256(raw)
		if os.WriteFile(filepath.Join(job, "stats", name), raw, 0600) != nil {
			return false
		}
		manifest = append(manifest, map[string]any{"name": name, "sha256": fmt.Sprintf("%x", sum), "bytes": len(raw)})
	}
	data, _ := json.Marshal(manifest)
	if os.WriteFile(filepath.Join(job, "stats-manifest.json"), data, 0600) != nil {
		return false
	}
	if len(r.Processes) != 1 {
		return false
	}
	budgets, excluded := applySaturation(dir, r, receipts, []string{"caller-1"})
	return len(budgets) == 1 && budgets[0].Outcome == "pass" && len(excluded) == 0
}

func parsePreviewMedia(input *previewMediaInput) (*loadgen.RunReport, previewMediaSummary, bool) {
	return parsePreviewMediaFor(input, true)
}

func parsePreviewMediaFor(input *previewMediaInput, qualify bool) (*loadgen.RunReport, previewMediaSummary, bool) {
	r := &loadgen.RunReport{Callers: len(input.Sessions), Events: input.Events, Limits: loadgen.DefaultLimits()}
	var numericFailure bool
	trials := failureInput(input)
	var sampleDuration, sampleRequests int64
	var first loadgen.Message
	var parseErr error
	if observe.Numbers(input.Probes) != nil {
		parseErr = fmt.Errorf("invalid probe numbers")
	}
	validEvents := make([]loadgen.EventResult, 0, len(input.Events))
	for _, event := range input.Events {
		if observe.Numbers(event) != nil {
			parseErr = fmt.Errorf("invalid event numbers")
		} else {
			validEvents = append(validEvents, event)
		}
	}
	r.Events = validEvents
	validReceipts := make([]receipt, 0, len(input.Receipts))
	for _, receipt := range input.Receipts {
		if observe.Numbers(receipt) != nil {
			parseErr = fmt.Errorf("invalid receipt numbers")
		} else {
			validReceipts = append(validReceipts, receipt)
		}
	}
	var checked strings.Builder
	// Reuse collected readSample's explicit-field checks. A missing zero-valued
	// CPU/pacing field cannot silently become a clean generator sample.
	decoder := json.NewDecoder(strings.NewReader(input.CallerJSONL))
	var samples []json.RawMessage
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			parseErr = err
			break
		}
		var m loadgen.Message
		err = observe.DecodeEvidenceLimit(raw, &m, 64<<20)
		if m.Call != nil && (m.Call.Distributed == nil || m.Call.Distributed.RunID == input.RunID) {
			for _, session := range input.Sessions {
				if m.Session == session && callFailure(m.Call) {
					numericFailure = true
				}
			}
		}
		if err != nil {
			parseErr = err
			continue
		}
		if err = observe.Numbers(m); err == nil {
			err = callNumbers(m.Call)
		}
		if err != nil {
			parseErr = err
			continue
		}
		if m.Type == "stats" {
			sample, err := sampleFromMessage(m, first.PID)
			if err != nil {
				parseErr = err
				continue
			}
			duration, dok := observe.AddCount(sampleDuration, int64(sample.Interval))
			requests, rok := observe.AddCount(sampleRequests, int64(sample.IntervalStatusRequests))
			if !dok || !rok {
				parseErr = fmt.Errorf("generator totals overflow")
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
			parseErr = err
		}
		r.Processes = []loadgen.ProcessReport{p}
	}
	collectionsErr := phaseCollections(input, r)
	if collectionsErr != nil {
		parseErr = collectionsErr
	}
	rawVerdict := previewMediaSummary{Status: "inconclusive"}
	out := rawVerdict
	guards := false
	if collectionsErr == nil && parseErr == nil {
		rawVerdict = previewMedia(r)
		if qualify {
			guards = previewGeneratorGuards(r, validReceipts, samples)
		}
		out = previewMedia(r)
	}
	if rawVerdict.Status == "failed" || numericFailure || trialsFailed(trials) {
		out.Status = "failed"
	}
	qualified := parseErr == nil && guards
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
			out.Status = "inconclusive"
			qualified = false
		}
		if r.Result.Lost {
			out.Status = "failed"
		}
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
// once. Adding per-gate verdicts or sample counters cannot qualify a campaign.
func pooledPreviewMedia(input *previewPoolInput) previewMediaSummary {
	r := &loadgen.RunReport{Limits: loadgen.DefaultLimits()}
	valid, failed := input.RunID != "" && len(input.Phases) > 0 && !input.Missing, poolFailure(input)
	sessions, operations := map[string]bool{}, map[string]bool{}
	for i := range input.Phases {
		phase := &input.Phases[i]
		part, verdict, qualified := parsePreviewMediaFor(phase, valid)
		valid = valid && qualified && phase.RunID == input.RunID
		failed = failed || phase.RunID == input.RunID && verdict.Status == "failed"
		for _, session := range phase.Sessions {
			if sessions[session] {
				valid = false
			}
			sessions[session] = true
		}
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
			continue
		}
		if !qualified || phase.RunID != input.RunID {
			continue
		}
		count, ok := observe.AddCount(int64(r.Callers), int64(part.Callers))
		if !ok {
			valid = false
			continue
		}
		r.Callers = int(count)
		r.Processes = append(r.Processes, part.Processes...)
		r.Events = append(r.Events, part.Events...)
	}
	out := previewMediaSummary{Status: "inconclusive"}
	if valid {
		out = previewMedia(r)
	}
	if !valid && out.Status != "failed" {
		out.Status = "inconclusive"
	}
	if failed {
		out.Status = "failed"
	}
	return out
}
func decodePool(data []byte) (input previewPoolInput, err error) {
	err = observe.DecodeEvidenceLimit(data, &input, 64<<20)
	if observe.IsIncompleteEvidence(err) {
		input.Missing = true
		for i := range input.Phases {
			input.Phases[i].Missing = true
		}
		err = nil
	}

	return
}
func main() {
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
