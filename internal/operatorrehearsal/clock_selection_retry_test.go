package operatorrehearsal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/stretchr/testify/require"
)

// The first admitting read starts a full 243 s after helper setup returns.
func TestRehearsalClockSelectionSetupOpening(t *testing.T) {
	p := convergenceFixture(t, 0)
	p.converge = true
	var setupDone, first time.Time
	p.start = func(ctx context.Context, _ time.Time) error {
		err := p.pause(ctx, 9*time.Second)
		setupDone = p.now()
		return err
	}
	read := p.read
	p.read = func(ctx context.Context, slot, script string) (string, error) {
		if slot == "shared" && first.IsZero() && !setupDone.IsZero() {
			first = p.now()
		}
		return read(ctx, slot, script)
	}
	r := collectClockSelection(context.Background(), "setup-test", "shared", "shared", p)
	require.Empty(t, r.Cause)
	require.Equal(t, 243*time.Second, first.Sub(setupDone), "MUTATION: rehearsal-setup-opening")
}

// Preserve the original single-window refusal with the real selector. A host
// with nine updates cannot bind admission, even when all other clocks fit.
func TestClockSelectionUpdatesSingleWindowCharacterization(t *testing.T) {
	p := convergenceFixture(t, 0)
	p.policy.WindowSeconds = 120
	base := p.now()
	var starts []int64
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		starts = append(starts, e.StartUnixSeconds)
		e = nineRelayUpdates(e)
		selected, err := clocks.Select(e, policy)
		require.EqualError(t, err, "clock-select-updates")
		return selected, err.Error()
	}
	r := collectClockSelection(context.Background(), "updates-test", "shared", "shared", p)
	require.Equal(t, "clock-select-updates", r.Cause)
	require.Nil(t, r.Bounds)
	require.NotEmpty(t, starts)
	require.Less(t, starts[len(starts)-1]-starts[0], int64(120))
	require.Equal(t, 154*time.Second, p.now().Sub(base))
}

func TestRehearsalClockSelectionRetriesFreshWindow(t *testing.T) {
	p := convergenceFixture(t, 0)
	p.converge = true
	selections := 0
	var failedEnd int64
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		if selections == 1 {
			failedEnd = e.EndUnixSeconds
			e = nineRelayUpdates(e)
		} else {
			require.Greater(t, e.StartUnixSeconds, failedEnd, "retry must collect a full fresh window")
			for _, h := range e.Hosts {
				require.LessOrEqual(t, h.Tracking.Rows[0].UnixSeconds, failedEnd, "keep predecessor history for boundary checks")
			}
		}
		selected, err := clocks.Select(e, policy)
		if err != nil {
			return selected, err.Error()
		}
		return selected, ""
	}
	r := collectClockSelection(context.Background(), "updates-test", "shared", "shared", p)
	require.Empty(t, r.Cause)
	require.NotNil(t, r.Bounds)
	require.Equal(t, 2, selections)
	require.Len(t, r.FailedWindows, 1)
	failure := r.FailedWindows[0]
	require.Equal(t, "clock-select-updates", failure.Cause)
	require.Equal(t, failedEnd, failure.EndUnixSeconds)
	require.Equal(t, int64(240), failure.EndUnixSeconds-failure.StartUnixSeconds)
	require.Equal(t, map[string]int{"relay": 9, "shared": 30, "w1": 30, "w2": 30}, failure.Updates)

	// Both the archived status and the final public report retain the refusal
	// after admission succeeds, so an operator can see why collection retried.
	dir := t.TempDir()
	var diagnostics bytes.Buffer
	report := passingReport()
	stage := &rehearsal{report: &report, diagnostics: &diagnostics, c: RehearseConfig{Output: dir}}
	stage.recordClockSelectionReport(r)
	raw, err := os.ReadFile(filepath.Join(dir, "clock-selection-status.json"))
	require.NoError(t, err)
	var archived ClockSelectionReport
	require.NoError(t, json.Unmarshal(raw, &archived))
	require.Equal(t, r.FailedWindows, archived.FailedWindows)
	var output bytes.Buffer
	require.NoError(t, report.Write(&output))
	var public Report
	require.NoError(t, json.Unmarshal(output.Bytes(), &public))
	require.Equal(t, r.FailedWindows, public.ClockSelection.FailedWindows)
	require.Contains(t, diagnostics.String(), `"failedWindows"`)
}

func TestRehearsalClockSelectionThreeUpdateFailures(t *testing.T) {
	p := convergenceFixture(t, 0)
	p.converge = true
	selections := 0
	var failedEnd int64
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		if failedEnd != 0 {
			require.Greater(t, e.StartUnixSeconds, failedEnd)
		}
		failedEnd = e.EndUnixSeconds
		e = nineRelayUpdates(e)
		selected, err := clocks.Select(e, policy)
		require.EqualError(t, err, "clock-select-updates")
		return selected, err.Error()
	}
	r := collectClockSelection(context.Background(), "updates-test", "shared", "shared", p)
	require.Equal(t, "clock-select-updates", r.Cause)
	require.Nil(t, r.Bounds)
	require.Equal(t, 3, selections, "MUTATION: rehearsal-extra-windows")
	require.Len(t, r.FailedWindows, 3)
	for _, failure := range r.FailedWindows {
		require.Equal(t, "clock-select-updates", failure.Cause)
		require.Equal(t, map[string]int{"relay": 9, "shared": 30, "w1": 30, "w2": 30}, failure.Updates)
	}
}

func TestRehearsalClockSelectionCeilingFailsFirstWindow(t *testing.T) {
	p := convergenceFixture(t, 0)
	p.converge = true
	selections := 0
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		for i := range e.Hosts {
			if e.Hosts[i].Slot == "shared" {
				for j := range e.Hosts[i].Tracking.Rows {
					row := &e.Hosts[i].Tracking.Rows[j]
					row.MaxErrorDecimal, row.MaxErrorSeconds = "1.099e-04", 0.0001099
				}
			}
		}
		selected, err := clocks.Select(e, policy)
		require.EqualError(t, err, "clock-select-ceiling")
		return selected, err.Error()
	}
	r := collectClockSelection(context.Background(), "ceiling-test", "shared", "shared", p)
	require.Equal(t, "clock-select-ceiling", r.Cause)
	require.Nil(t, r.Bounds)
	require.Equal(t, 1, selections)
	require.Len(t, r.FailedWindows, 1)
	require.Equal(t, "clock-select-ceiling", r.FailedWindows[0].Cause)
	require.Equal(t, map[string]int{"relay": 30, "shared": 30, "w1": 30, "w2": 30}, r.FailedWindows[0].Updates)
}

func TestRehearsalClockSelectionOtherCausesFailFirstWindow(t *testing.T) {
	for _, cause := range []string{"clock-select-growth", "clock-select-break", "clock-select-leap", "clock-select-coverage", "clock-select-stale", "clock-lease-expired"} {
		t.Run(cause, func(t *testing.T) {
			p := convergenceFixture(t, 0)
			p.converge = true
			selections := 0
			p.selectBounds = func(context.Context, clocks.SelectionEvidence, clocks.SelectionPolicy) (clocks.ClockSelection, string) {
				selections++
				return clocks.ClockSelection{}, cause
			}
			r := collectClockSelection(context.Background(), "refusal-test", "shared", "shared", p)
			require.Equal(t, cause, r.Cause)
			require.Nil(t, r.Bounds)
			require.Equal(t, 1, selections)
			require.Len(t, r.FailedWindows, 1)
			require.Equal(t, cause, r.FailedWindows[0].Cause)
		})
	}
}

func TestRehearsalClockSelectionRetriesFitCombinedBudget(t *testing.T) {
	// A late convergence leaves enough time for the original window only.
	p := convergenceFixture(t, clockConvergenceMaximum-90*time.Second)
	p.converge = true
	base := p.now()
	pause := p.pause
	p.pause = func(ctx context.Context, d time.Duration) error {
		if p.now().Sub(base) > 2*time.Second && p.now().Sub(base) < clockConvergenceMaximum-90*time.Second {
			d = max(d, 30*time.Second)
		}
		return pause(ctx, d)
	}
	selections := 0
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		e = nineRelayUpdates(e)
		selected, err := clocks.Select(e, policy)
		require.EqualError(t, err, "clock-select-updates")
		return selected, err.Error()
	}
	r := collectClockSelection(context.Background(), "budget-test", "shared", "shared", p)
	require.Equal(t, "clock-select-updates", r.Cause)
	require.Nil(t, r.Bounds)
	require.Equal(t, 1, selections)
	require.Len(t, r.FailedWindows, 1)
	require.LessOrEqual(t, p.now().Sub(base), 932*time.Second, "seed + convergence + admission + exit guard")
}

func TestRehearsalClockSelectionExactlyOneRetryFits(t *testing.T) {
	p := convergenceFixture(t, 270*time.Second)
	p.converge = true
	base, pause := p.now(), p.pause
	p.pause = func(ctx context.Context, d time.Duration) error {
		if elapsed := p.now().Sub(base); elapsed > 2*time.Second && elapsed < 270*time.Second {
			d = max(d, 30*time.Second)
		}
		return pause(ctx, d)
	}
	selections := 0
	var retryStarted time.Time
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		if selections == 1 {
			retryStarted = p.now()
		}
		selected, err := clocks.Select(nineRelayUpdates(e), policy)
		require.EqualError(t, err, "clock-select-updates")
		return selected, err.Error()
	}
	r := collectClockSelection(context.Background(), "one-retry-test", "shared", "shared", p)
	require.Equal(t, "clock-select-updates", r.Cause)
	require.Nil(t, r.Bounds)
	require.Equal(t, 2, selections, "MUTATION: rehearsal-admission-limit")
	require.Len(t, r.FailedWindows, 2)
	require.LessOrEqual(t, p.now().Sub(base), 932*time.Second, "MUTATION: rehearsal-admission-limit")
	require.Equal(t, 322*time.Second, p.now().Sub(retryStarted), "MUTATION: rehearsal-retry-end")
}

func TestRehearsalClockSelectionTransientProbesRecover(t *testing.T) {
	p := convergenceFixture(t, 0)
	p.converge = true
	selections := 0
	var firstStart int64
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		if selections == 1 {
			firstStart = e.StartUnixSeconds
			// Model incomplete probe coverage on the first admitting read.
			e.Falsification.Records = nil
		} else {
			require.Less(t, e.StartUnixSeconds-firstStart, int64(240), "recover inside the same window")
		}
		selected, err := clocks.Select(e, policy)
		if err != nil {
			require.EqualError(t, err, "clock-select-probes")
			return selected, err.Error()
		}
		return selected, ""
	}
	r := collectClockSelection(context.Background(), "probe-retry-test", "shared", "shared", p)
	require.Empty(t, r.Cause, "MUTATION: rehearsal-transient-probes")
	require.NotNil(t, r.Bounds)
	require.Equal(t, 2, selections)
	require.Empty(t, r.FailedWindows)
}

func TestRehearsalClockSelectionFalsificationStaysSticky(t *testing.T) {
	p := convergenceFixture(t, 0)
	p.converge = true
	read := p.read
	injected := false
	p.read = func(ctx context.Context, slot, script string) (string, error) {
		out, err := read(ctx, slot, script)
		if err != nil || slot != "shared" || injected {
			return out, err
		}
		raw, err := host.Unpack(out, 256000)
		require.NoError(t, err)
		var doc map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &doc))
		if len(doc["probeLog"]) == 0 {
			return out, nil
		}
		var batch clocks.ProbeBatch
		require.NoError(t, json.Unmarshal(doc["probeLog"], &batch))
		if batch.Lines == "" {
			return out, nil
		}
		lines := strings.SplitAfter(batch.Lines, "\n")
		middle := len(lines) / 2
		fields := strings.Fields(lines[middle])
		// One actual disjoint exchange, followed by clean later batches.
		for _, i := range []int{2, 3} {
			ns, err := strconv.ParseInt(fields[i], 10, 64)
			require.NoError(t, err)
			fields[i] = strconv.FormatInt(ns+10_000_000, 10)
		}
		lines[middle] = strings.Join(fields, " ") + "\n"
		batch.Lines = strings.Join(lines, "")
		doc["probeLog"], err = json.Marshal(batch)
		require.NoError(t, err)
		raw, err = json.Marshal(doc)
		require.NoError(t, err)
		var packed bytes.Buffer
		z := gzip.NewWriter(&packed)
		_, err = z.Write(raw)
		require.NoError(t, err)
		require.NoError(t, z.Close())
		injected = true
		return base64.StdEncoding.EncodeToString(packed.Bytes()), nil
	}
	selectBounds := p.selectBounds
	selections := 0
	p.selectBounds = func(ctx context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		found := false
		for _, record := range e.Falsification.Records {
			found = found || record.Status == "falsified"
		}
		require.True(t, found, "the falsification must survive later clean reads")
		selected, cause := selectBounds(ctx, e, policy)
		require.Equal(t, "clock-select-probes", cause)
		return selected, cause
	}
	r := collectClockSelection(context.Background(), "falsified-test", "shared", "shared", p)
	require.True(t, injected)
	require.Greater(t, selections, 1)
	require.Equal(t, "clock-select-probes", r.Cause)
	require.Nil(t, r.Bounds)
	require.Len(t, r.FailedWindows, 1, "probe rereads are one failed window")
	require.Equal(t, "clock-select-probes", r.FailedWindows[0].Cause)
}

// Keep boundary rows, but leave only nine distinct in-window relay updates.
// This isolates the reported failure without changing selector policy.
func nineRelayUpdates(e clocks.SelectionEvidence) clocks.SelectionEvidence {
	for i := range e.Hosts {
		if e.Hosts[i].Slot != "relay" {
			continue
		}
		rows := e.Hosts[i].Tracking.Rows
		var inside []clocks.TrackingRow
		for _, row := range rows {
			if row.UnixSeconds >= e.StartUnixSeconds && row.UnixSeconds <= e.EndUnixSeconds {
				inside = append(inside, row)
			}
		}
		// Spread nine updates across the whole window, keeping every gap below
		// the selector's unchanged 60 s coverage limit.
		wanted := map[clocks.TrackingRow]bool{}
		for j := range 9 {
			wanted[inside[j*(len(inside)-1)/8]] = true
		}
		kept := make([]clocks.TrackingRow, 0, len(rows))
		for _, row := range rows {
			if row.UnixSeconds >= e.StartUnixSeconds && row.UnixSeconds <= e.EndUnixSeconds && !wanted[row] {
				continue
			}
			kept = append(kept, row)
		}
		e.Hosts[i].Tracking.Rows = kept
	}
	return e
}
