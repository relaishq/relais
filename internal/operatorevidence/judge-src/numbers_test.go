package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/loadgen"
	"github.com/relaishq/relais/internal/operatorevidence"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/callharness"
)

func replaceCall(t *testing.T, input *previewPoolInput, change func(*callharness.Report)) {
	t.Helper()
	var stream strings.Builder
	decoder := json.NewDecoder(strings.NewReader(input.Phases[0].CallerJSONL))
	encoder := json.NewEncoder(&stream)
	for decoder.More() {
		var message loadgen.Message
		if err := decoder.Decode(&message); err != nil {
			t.Fatal(err)
		}
		if message.Call != nil {
			change(message.Call)
		}
		if err := encoder.Encode(message); err != nil {
			t.Fatal(err)
		}
	}
	input.Phases[0].CallerJSONL = stream.String()
}
func TestPassingPoolRejectsCounterCancellation(t *testing.T) {
	t.Parallel()
	passing := rawPoolSample()
	if got := pooledPreviewMedia(&passing); got.Status != "passed" {
		t.Fatal("passing control", got)
	}
	for _, counters := range []callharness.DecryptionFailures{{AuthTag: 1, Replay: -1}, {AuthTag: math.MaxInt64, Replay: 1}, {AuthTag: math.MaxInt64, Replay: math.MaxInt64}} {
		input := rawPoolSample()
		replaceCall(t, &input, func(c *callharness.Report) { c.DecryptionFailures = counters })
		if got := pooledPreviewMedia(&input); got.Status != "failed" {
			t.Fatal("MUTATION: positive-counter-independent", counters, got)
		}
		_, _, qualified := parsePreviewMedia(&input.Phases[0])
		if qualified {
			t.Fatal("invalid counters qualified", counters)
		}
	}
	missing := rawPoolSample()
	replaceCall(t, &missing, func(c *callharness.Report) {
		c.DecryptionFailures = callharness.DecryptionFailures{AuthTag: 1, Replay: -1}
		c.Distributed = nil
	})
	if got := pooledPreviewMedia(&missing); got.Status != "failed" {
		t.Fatal("missing mapping erased counter failure", got)
	}
	input := rawPoolSample()
	replaceCall(t, &input, func(c *callharness.Report) {
		c.DecryptionFailures = callharness.DecryptionFailures{AuthTag: int(observe.MaxInteger), Replay: 1}
	})
	_, summary, qualified := parsePreviewMedia(&input.Phases[0])
	if summary.Status != "failed" || qualified {
		t.Fatal("MUTATION: numeric-overflow-qualification", summary, qualified)
	}
}
func TestPassingPoolRejectsInvalidMeasurements(t *testing.T) {
	t.Parallel()
	changes := []func(*callharness.Report){
		func(c *callharness.Report) { c.DecryptionFailures.AuthTag = -1 },
		func(c *callharness.Report) { c.Moves[0].Distributed.Gaps[0].EventGap.Lower = -1 },
		func(c *callharness.Report) { c.Tracks[0].DuplicatePackets = -1 },
		func(c *callharness.Report) { c.Tracks[0].Packets = -1 },
		func(c *callharness.Report) { c.Tracks[0].MediaGap = -time.Second },
		func(c *callharness.Report) { c.Moves[0].Distributed.Gaps[0].BaselineCertain[0] = -time.Millisecond },
		func(c *callharness.Report) {
			c.Moves[0].Distributed.Gaps[0].EventGap.Upper = time.Duration(math.MaxInt64)
		},
		func(c *callharness.Report) {
			c.Moves[0].Measurement.Audio.RecoveryLimit = time.Duration(observe.MaxInteger)
			c.Moves[0].Measurement.Audio.StabilityWindow = time.Duration(observe.MaxInteger)
		},
	}
	for _, change := range changes {
		input := rawPoolSample()
		replaceCall(t, &input, change)
		if got := pooledPreviewMedia(&input); got.Status == "passed" {
			t.Fatal("MUTATION: invalid-media-number", got)
		}
		input.Phases[0].Receipts[0].Result.Lost = true
		if got := pooledPreviewMedia(&input); got.Status != "failed" {
			t.Fatal("independent receipt erased", got)
		}
	}
	input := rawPoolSample()
	replaceCall(t, &input, func(c *callharness.Report) {
		c.Tracks[0].Packets = -1
		c.Moves[0].Distributed.Gaps[0].EventGap = callharness.DurationInterval{Lower: 2 * time.Second, Upper: 2 * time.Second}
	})
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal("validated gap failure erased", got)
	}
	input = rawPoolSample()
	replaceCall(t, &input, func(c *callharness.Report) { c.ICERestarts = 1; c.Tracks[0].Packets = -1 })
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal("positive ICE restart erased", got)
	}
}
func TestGapFailureSurvivesInvalidDiagnostics(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	replaceCall(t, &input, func(c *callharness.Report) {
		c.Tracks[0].Packets = -1
		for i := range c.Moves {
			for j := range c.Moves[i].Distributed.Gaps {
				c.Moves[i].Distributed.Gaps[j].EventGap = callharness.DurationInterval{Lower: 180 * time.Millisecond, Upper: 180 * time.Millisecond}
			}
		}
	})
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal("MUTATION: gap-failure-independent", got)
	}
	// Removing malformed low gaps must not turn the remaining high observation
	// into the entire p95 population. No absolute 2s failure is present here.
	input = rawPoolSample()
	replaceCall(t, &input, func(c *callharness.Report) {
		c.Tracks[0].Packets = -1
		c.Moves[0].Distributed.Gaps[0].EventGap = callharness.DurationInterval{Lower: 180 * time.Millisecond, Upper: 180 * time.Millisecond}
		c.Moves[1].Distributed.Gaps[0].BaselineCertain[0] = -1
	})
	if got := pooledPreviewMedia(&input); got.Status == "failed" {
		t.Fatal("malformed quantile population fabricated failure", got)
	}
}

func TestGeneratorNumbersAndWindowArithmetic(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	decoder := json.NewDecoder(strings.NewReader(input.Phases[0].CallerJSONL))
	var message loadgen.Message
	if err := decoder.Decode(&message); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*loadgen.Stats){func(s *loadgen.Stats) { s.CPUPerGOMAXPROCS = -1 }, func(s *loadgen.Stats) { s.IntervalLateness.P99 = -1 }, func(s *loadgen.Stats) { s.IntervalPacing.SentFrames = math.MaxUint64 }, func(s *loadgen.Stats) {
		s.IntervalPacing.SentFrames = uint64(observe.MaxInteger)
		s.IntervalPacing.SkippedFrames = 1
	}} {
		copy := message
		stats := *message.Stats
		change(&stats)
		copy.Stats = &stats
		raw, _ := json.Marshal(copy)
		if _, err := readSample(raw, copy.PID); err == nil {
			t.Fatal("MUTATION: generator-numeric-guard")
		}
	}
	sample := *message.Stats
	sample.Interval = time.Duration(math.MaxInt64)
	if statsClockExcursion([]loadgen.Stats{sample, sample}) <= statsClockTolerance {
		t.Fatal("invalid excursion accepted")
	}
	c := previewSample().Processes[0].Calls[0].Call
	m := c.Moves[0]
	m.Measurement.Audio.RecoveryLimit = time.Duration(observe.MaxInteger)
	m.Measurement.Audio.StabilityWindow = time.Duration(observe.MaxInteger)
	if _, _, err := callerEventWindow(receipt{}, c, m); err == nil {
		t.Fatal("MUTATION: exclusion-window-overflow")
	}
}

func TestGeneratorRequestTotalOverflow(t *testing.T) {
	t.Parallel()
	input := rawPoolSample()
	decoder := json.NewDecoder(strings.NewReader(input.Phases[0].CallerJSONL))
	var stream strings.Builder
	encoder := json.NewEncoder(&stream)
	changed := 0
	for decoder.More() {
		var m loadgen.Message
		if err := decoder.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Stats != nil && changed < 2 {
			m.Stats.IntervalStatusRequests = uint64(observe.MaxInteger)
			changed++
		}
		if err := encoder.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	input.Phases[0].CallerJSONL = stream.String()
	if _, _, qualified := parsePreviewMedia(&input.Phases[0]); qualified {
		t.Fatal("MUTATION: generator-total-overflow")
	}
	input.Phases[0].Receipts[0].Result.Lost = true
	if got := pooledPreviewMedia(&input); got.Status != "failed" {
		t.Fatal("generator total erased receipt failure", got)
	}
}

func TestPoolNumericDecodeKeepsLoss(t *testing.T) {
	t.Parallel()
	sample := rawPoolSample()
	sample.Phases[0].Receipts[0].Result.Lost = true
	phases := evidencePhases(t, sample)
	phases[0].Raw = []byte(strings.Replace(string(phases[0].Raw), `"affected":1`, `"affected":9223372036854775808`, 1))
	pool, _, err := operatorevidence.ReadPool(sample.RunID, phases, false)
	if err != nil || !pool.Missing {
		t.Fatal("MUTATION: numeric-decode-keeps-loss", pool.Missing, err)
	}
	raw, _ := json.Marshal(pool)
	input, err := decodePool(raw)
	if err != nil || pooledPreviewMedia(&input).Status != "failed" {
		t.Fatal("numeric decoding erased loss", err)
	}
	raw, _ = json.Marshal(sample)
	raw = []byte(strings.Replace(string(raw), `"affected":1`, `"affected":9223372036854775808`, 1))
	input, err = decodePool(raw)
	if err != nil || !input.Missing || pooledPreviewMedia(&input).Status != "failed" {
		t.Fatal("numeric pool decoding erased loss", err)
	}
}
