package operatorrehearsal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/pkg/operator"
	"github.com/stretchr/testify/require"
)

// The restored shared clock initially exceeds both pair ceilings, then settles.
// Keep the bad rows in history: a fresh window must age them out naturally.
func convergenceFixture(t *testing.T, readyAfter time.Duration) clockAdmissionPorts {
	t.Helper()
	p := admissionFixture(t, false, func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		s, err := clocks.Select(e, policy)
		if err != nil {
			return s, err.Error()
		}
		return s, ""
	})
	clock := &virtualClock{now: p.now()}
	p.now, p.pause, p.timeout = clock.Now, clock.Pause, clock.Timeout
	p.poll = 8 * time.Second
	ready := p.now().Add(readyAfter).Unix()
	dir := t.TempDir()
	p.read = func(ctx context.Context, slot, script string) (string, error) {
		cmd := exec.CommandContext(ctx, "python3", "-B", "../operatorgates/host/clock_admission_test.py", dir, slot, fmt.Sprint(p.now().Unix()), "false", "8", "0", "1792886400")
		cmd.Stdin = strings.NewReader(script)
		output, err := cmd.CombinedOutput()
		out := string(output)
		if err != nil || slot != "shared" {
			return out, err
		}
		raw, err := host.Unpack(out, 256000)
		require.NoError(t, err)
		var doc map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &doc))
		var log clocks.TrackingLog
		require.NoError(t, json.Unmarshal(doc["trackingLog"], &log))
		for i := range log.Rows {
			row := &log.Rows[i]
			if row.UnixSeconds < ready {
				row.MaxErrorSeconds, row.MaxErrorDecimal = 0.0001099, "1.099e-04"
			}
		}
		doc["trackingLog"], err = json.Marshal(log)
		require.NoError(t, err)
		raw, err = json.Marshal(doc)
		require.NoError(t, err)
		var packed bytes.Buffer
		z := gzip.NewWriter(&packed)
		_, err = z.Write(raw)
		require.NoError(t, err)
		require.NoError(t, z.Close())
		return base64.StdEncoding.EncodeToString(packed.Bytes()), nil
	}
	return p
}

// Characterization before changing control flow: admission begins while the
// restored clock is still settling. Early high-error rows poison the window.
func TestClockAdmissionBeforeConvergenceCharacterization(t *testing.T) {
	p := convergenceFixture(t, 40*time.Second)
	base := p.now()
	var began time.Time
	p.start = func(context.Context, time.Time) error { began = p.now(); return nil }
	r := collectClockSelection(context.Background(), "convergence-test", "shared", "shared", p)
	require.Less(t, began.Sub(base), 40*time.Second)
	require.Equal(t, "clock-select-ceiling", r.Cause)
	require.Nil(t, r.Bounds)
}

func TestClockAdmissionWaitsForConvergence(t *testing.T) {
	p := convergenceFixture(t, 40*time.Second)
	p.converge = true
	base := p.now()
	var began time.Time
	p.start = func(context.Context, time.Time) error { began = p.now(); return nil }
	selectBounds := p.selectBounds
	p.selectBounds = func(ctx context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		require.GreaterOrEqual(t, e.StartUnixSeconds, began.Unix())
		require.Equal(t, int64(120), e.EndUnixSeconds-e.StartUnixSeconds)
		return selectBounds(ctx, e, policy)
	}
	r := collectClockSelection(context.Background(), "convergence-test", "shared", "shared", p)
	require.Empty(t, r.Cause)
	require.NotNil(t, r.Bounds)
	require.GreaterOrEqual(t, began.Sub(base), 40*time.Second)
}

func TestClockAdmissionConvergenceTimeout(t *testing.T) {
	p := convergenceFixture(t, time.Hour)
	p.converge = true
	base := p.now()
	pause := p.pause
	// Once the seed guard completes, skip across several tracking updates.
	p.pause = func(ctx context.Context, d time.Duration) error {
		if p.now().Sub(base) >= 2*time.Second {
			d = max(d, time.Minute)
		}
		return pause(ctx, d)
	}
	p.start = func(context.Context, time.Time) error { t.Error("helpers started before convergence"); return nil }
	r := collectClockSelection(context.Background(), "convergence-test", "shared", "shared", p)
	require.Equal(t, "clock-select-ceiling", r.Cause)
	require.Nil(t, r.Bounds)
	require.Equal(t, clockConvergenceMaximum+2*time.Second, p.now().Sub(base))
}

// Timeout during a read keeps the last measured cause, even when every host
// command returns cancellation instead of a new sample.
func TestClockConvergenceReadDeadlineKeepsCause(t *testing.T) {
	p := convergenceFixture(t, time.Hour)
	p.converge = true
	base, read := p.now(), p.read
	var readers atomic.Int32
	ready := make(chan struct{})
	p.read = func(ctx context.Context, slot, script string) (string, error) {
		if p.now().Sub(base) < 4*time.Second {
			return read(ctx, slot, script)
		}
		if readers.Add(1) == int32(len(Slots)) {
			close(ready)
		}
		<-ready
		if slot == "shared" {
			_ = p.pause(ctx, clockConvergenceMaximum)
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	p.start = func(context.Context, time.Time) error { t.Error("helpers started before convergence"); return nil }
	r := collectClockSelection(context.Background(), "convergence-test", "shared", "shared", p)
	require.Equal(t, "clock-select-ceiling", r.Cause)
	require.Nil(t, r.Bounds)
	require.Equal(t, clockConvergenceMaximum+2*time.Second, p.now().Sub(base))
}

func TestClockSelectionAndMissingBindingPublicCauses(t *testing.T) {
	r := passingReport()
	r.ClockSelection = &ClockSelectionReport{Cause: "clock-select-ceiling"}
	var diagnostics bytes.Buffer
	stage := &rehearsal{report: &r, diagnostics: &diagnostics}
	_, _, err := BindClockAdmission(t.TempDir(), r.ClockSelection, operator.Scenario{})
	require.EqualError(t, err, "clock-binding-missing")
	stage.recordError("clock-binding-failed", err)
	var raw bytes.Buffer
	require.NoError(t, r.Write(&raw))
	var got Report
	require.NoError(t, json.Unmarshal(raw.Bytes(), &got))
	require.Equal(t, "clock-select-ceiling", got.ClockSelection.Cause)
	require.Contains(t, got.Errors, "clock-binding-missing")
	require.Equal(t, "clock-binding-failed: clock-binding-missing\n", diagnostics.String())
	for _, code := range []string{"clock-select-ceiling", "clock-binding-missing"} {
		require.Equal(t, code, PublicCode(code))
		require.Equal(t, "unclassified", PublicCode(code+":unsafe-detail"))
	}
}
