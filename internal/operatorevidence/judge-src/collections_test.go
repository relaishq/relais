package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/pkg/callharness"
)

func editPoolCalls(t *testing.T, input *previewPoolInput, change func(*callharness.Report)) {
	t.Helper()
	for i := range input.Phases {
		one := previewPoolInput{Phases: []previewMediaInput{input.Phases[i]}}
		replaceCall(t, &one, change)
		input.Phases[i] = one.Phases[0]
	}
}
func TestBaselinePopulationCompleteness(t *testing.T) {
	t.Parallel()
	control := rawPoolSample()
	if got := pooledPreviewMedia(&control); got.Status != "passed" {
		t.Fatal("passing control", got)
	}
	changes := map[string]func(*callharness.DistributedGap){
		"missing": func(g *callharness.DistributedGap) { g.BaselinePossible = nil },
		"truncated": func(g *callharness.DistributedGap) {
			g.BaselinePossible = g.BaselinePossible[:len(g.BaselinePossible)-1]
		},
		"non-containing": func(g *callharness.DistributedGap) {
			g.BaselinePossible = append([]time.Duration{}, g.BaselinePossible...)
			g.BaselinePossible[0]++
		},
		"missing-certain": func(g *callharness.DistributedGap) { g.BaselineCertain = nil },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := rawPoolSample()
			editPoolCalls(t, &input, func(c *callharness.Report) {
				for i := range c.Moves {
					for j := range c.Moves[i].Distributed.Gaps {
						change(&c.Moves[i].Distributed.Gaps[j])
					}
				}
			})
			if name == "missing" {
				for i := range input.Phases {
					input.Phases[i].CallerJSONL = strings.ReplaceAll(input.Phases[i].CallerJSONL, `"baseline_possible_ns":null,`, "")
				}
			}
			if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
				t.Fatal("MUTATION: baseline-population-complete", got)
			}
			for i := range input.Phases {
				if _, _, qualified := parsePreviewMedia(&input.Phases[i]); qualified {
					t.Fatal("incomplete population qualified")
				}
			}
			input.Phases[0].Receipts[0].Result.Lost = true
			if got := pooledPreviewMedia(&input); got.Status != "failed" {
				t.Fatal("independent receipt failure erased", got)
			}
			input.Phases[0].Receipts[0].Result.Lost = false
			replaceCall(t, &input, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 1 })
			if got := pooledPreviewMedia(&input); got.Status != "failed" {
				t.Fatal("independent counter failure erased", got)
			}
		})
	}
	// Duplicates in the certain multiset require the same multiplicity in possible.
	if baselinePopulation(callharness.DistributedGap{BaselineCertain: []time.Duration{1, 1}, BaselinePossible: []time.Duration{1, 2}}) {
		t.Fatal("multiset multiplicity ignored")
	}
}
func TestCallerCollectionCompleteness(t *testing.T) {
	t.Parallel()
	changes := map[string]func(*callharness.Report){
		"first-baselines":         func(c *callharness.Report) { c.Moves[0].Distributed.FirstBaselines = nil },
		"changed-first-baselines": func(c *callharness.Report) { c.Moves[1].Distributed.FirstBaselines[0].Audio.P95++ },
		"classification-count":    func(c *callharness.Report) { c.Moves[0].Distributed.Evaluations = 0 },
		"armed-extra": func(c *callharness.Report) {
			id := c.Distributed.Armed[0]
			id.OperationID = "unreported-operation"
			c.Distributed.Armed = append(c.Distributed.Armed, id)
		},
		"tracks":          func(c *callharness.Report) { c.Tracks = nil },
		"duplicate-track": func(c *callharness.Report) { c.Tracks[1].Kind = "audio" },
		"moves":           func(c *callharness.Report) { c.Moves = c.Moves[1:] },
		"armed":           func(c *callharness.Report) { c.Distributed.Armed = nil },
		"armed-epoch":     func(c *callharness.Report) { c.Distributed.Armed[0].Epoch++ },
		"duplicate-armed": func(c *callharness.Report) { c.Distributed.Armed = append(c.Distributed.Armed, c.Distributed.Armed[0]) },
		"probes":          func(c *callharness.Report) { c.Distributed.Probes = c.Distributed.Probes[:9] },
		"gaps":            func(c *callharness.Report) { c.Moves[0].Distributed.Gaps = nil },
		"gap-kind":        func(c *callharness.Report) { c.Moves[0].Distributed.Gaps = c.Moves[0].Distributed.Gaps[:1] },
		"duplicate-gap":   func(c *callharness.Report) { c.Moves[0].Distributed.Gaps[1].Kind = "audio" },
		"duration-map":    func(c *callharness.Report) { delete(c.Moves[0].Distributed.Durations, "resume") },
		"content-counts": func(c *callharness.Report) {
			c.Moves[0].Measurement.ContentLoss.Event.Audio = callharness.ContentLossCounts{Sent: 1, Lost: 2}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := rawPoolSample()
			replaceCall(t, &input, change)
			if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
				t.Fatal("MUTATION: caller-collections-complete", got)
			}
			input.Phases[0].Receipts[0].Result.Lost = true
			if got := pooledPreviewMedia(&input); got.Status != "failed" {
				t.Fatal("collection error erased loss", got)
			}
		})
	}
}
func TestPhaseCollectionJoins(t *testing.T) {
	t.Parallel()
	changes := map[string]func(*previewMediaInput){
		"receipt-epoch":          func(p *previewMediaInput) { p.Receipts[0].Identity.Epoch++ },
		"receipt-worker":         func(p *previewMediaInput) { p.Receipts[0].Identity.Worker = "other" },
		"receipt-session-result": func(p *previewMediaInput) { p.Receipts[0].Result.ID = "other-session" },
		"receipt-source-result":  func(p *previewMediaInput) { p.Receipts[0].Result.From = "other-worker" },
		"receipt-kind":           func(p *previewMediaInput) { p.Receipts[0].Kind = "takeover" },
		"event-sessions":         func(p *previewMediaInput) { p.Events[0].Sessions = nil },
		"event-count":            func(p *previewMediaInput) { p.Events[0].Affected++ },
		"event-epoch":            func(p *previewMediaInput) { p.Events[0].Identities[0].Epoch++ },
		"event-identities":       func(p *previewMediaInput) { p.Events[0].Identities = nil },
		"event-truncated":        func(p *previewMediaInput) { p.Events = p.Events[:len(p.Events)-1] },
		"receipt-truncated":      func(p *previewMediaInput) { p.Receipts = p.Receipts[:len(p.Receipts)-1] },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := rawPoolSample()
			change(&input.Phases[0])
			phase := &input.Phases[0]
			process, err := loadgen.ReadSoakProcess(strings.NewReader(phase.CallerJSONL), 42, len(phase.Sessions), loadgen.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			report := loadgen.RunReport{Callers: len(phase.Sessions), Events: phase.Events, Processes: []loadgen.ProcessReport{process}}
			if phaseCollections(phase, &report) == nil {
				t.Fatal("MUTATION: phase-collection-join")
			}
			if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
				t.Fatal("MUTATION: phase-collection-join", got)
			}
			replaceCall(t, &input, func(c *callharness.Report) { c.DecryptionFailures.AuthTag = 1 })
			if got := pooledPreviewMedia(&input); got.Status != "failed" {
				t.Fatal("join error erased failure", got)
			}
		})
	}
}
func TestCollectionPresenceBeforeJudgment(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	input.Phases[0].CallerJSONL = strings.Replace(input.Phases[0].CallerJSONL, `"Trusted":true,"Failed":false`, `"Trusted":true`, 1)
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal("MUTATION: verdict-presence", got)
	}
	input.Phases[0].Receipts[0].Result.Lost = true
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal(got)
	}
}
func TestGeneratorCollectionsMatchSource(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	r, _, qualified := parsePreviewMedia(&input.Phases[0])
	if !qualified {
		t.Fatal("control not qualified")
	}
	decoder := json.NewDecoder(strings.NewReader(input.Phases[0].CallerJSONL))
	var samples []json.RawMessage
	for decoder.More() {
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			t.Fatal("decode")
		}
		var m loadgen.Message
		if json.Unmarshal(raw, &m) != nil {
			t.Fatal("message")
		}
		if m.Stats != nil {
			samples = append(samples, raw)
		}
	}
	r.Processes[0].Samples[0].CPUPerGOMAXPROCS = 100
	if previewGeneratorGuards(r, input.Phases[0].Receipts, samples) {
		t.Fatal("MUTATION: generator-source-collections")
	}
}
func TestOptionalBaselinesCannotShrinkWindow(t *testing.T) {
	t.Parallel()
	c := previewSample().Processes[0].Calls[0].Call
	m := c.Moves[0]
	receipt := receipt{Start: c.StartedAt.Add(m.Start), End: c.StartedAt.Add(m.End)}
	m.Measurement.WindowStart, m.Measurement.WindowEnd = 3*time.Second, 4*time.Second
	m.Measurement.ContentLoss.Event = callharness.ContentLossWindow{Start: m.Measurement.WindowStart, End: m.Measurement.WindowEnd}
	m.Measurement.ContentLoss.Baselines = []callharness.ContentLossWindow{{Start: time.Second, End: 2 * time.Second}}
	first, last, err := callerEventWindow(receipt, c, m)
	if err != nil {
		t.Fatal(err)
	}
	m.Measurement.ContentLoss.Baselines = nil
	missingFirst, missingLast, err := callerEventWindow(receipt, c, m)
	if err != nil || missingFirst.After(first) || missingLast.Before(last) {
		t.Fatal("MUTATION: missing-baseline-window", first, missingFirst, last, missingLast, err)
	}
}

func TestEventMembershipCounts(t *testing.T) {
	t.Parallel()
	r := previewSample()
	r.Events[0].Affected++
	if reportCollections(&r) == nil {
		t.Fatal("MUTATION: event-membership-count")
	}
}
func TestIncompletePopulationCannotInventFailure(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	editPoolCalls(t, &input, func(c *callharness.Report) {
		for i := range c.Moves {
			for j := range c.Moves[i].Distributed.Gaps {
				g := &c.Moves[i].Distributed.Gaps[j]
				g.BaselinePossible = []time.Duration{}
				g.EventGap = callharness.DurationInterval{Lower: 100 * time.Millisecond, Upper: 100 * time.Millisecond}
			}
		}
	})
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal("MUTATION: incomplete-population-not-failure", got)
	}
}

func TestRequiredReceiptMembers(t *testing.T) {
	t.Parallel()
	for _, loss := range []bool{false, true} {
		sample := rawPoolSample()
		sample.Phases[0].Receipts[0].Result.Lost = loss
		phases := evidencePhases(t, sample)
		phases[0].Raw = []byte(strings.ReplaceAll(string(phases[0].Raw), `,"uncertain":false`, ""))
		pool, input := joinedPool(t, phases)
		want := "inconclusive"
		if loss {
			want = "failed"
		}
		if !pool.Missing || !input.Phases[0].Missing || pooledPreviewMedia(&input).Status != want {
			t.Fatal("MUTATION: receipt-member-presence", pool.Missing, pooledPreviewMedia(&input))
		}
		raw, _ := json.Marshal(sample)
		raw = []byte(strings.ReplaceAll(string(raw), `,"uncertain":false`, ""))
		decoded, err := decodePool(raw)
		if err != nil || !decoded.Missing || pooledPreviewMedia(&decoded).Status != want {
			t.Fatal("missing receipt member erased state", err)
		}
	}
}
func TestTakeoverBoundCollection(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	editPoolCalls(t, &input, func(c *callharness.Report) {
		for i := range c.Moves {
			if c.Moves[i].Kind == "takeover" {
				c.Moves[i].Distributed.KillToLive = nil
			}
		}
	})
	if got := pooledPreviewMedia(&input); got.Status != "inconclusive" {
		t.Fatal("MUTATION: takeover-bound-presence", got)
	}
	input.Phases[0].Receipts[0].Result.Lost = true
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal("takeover bound erased loss", got)
	}
}
