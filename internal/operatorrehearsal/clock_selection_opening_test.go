package operatorrehearsal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/stretchr/testify/require"
)

// Row seconds and probe send times come from hosted run 44042292271. Numeric
// clock errors and RTTs are normalized to known-good values: this test isolates
// the missing opening coverage, using the real ProbeHistory and selector.
func hostedOpeningFixture(t *testing.T, slow bool) clockAdmissionPorts {
	t.Helper()
	var fixture struct {
		WindowStartUnixSeconds int64
		TrackingSeconds        map[string][]int64
		ProbeSendOffsetsNS     map[string][]int64 `json:"probeSendOffsetsNs"`
	}
	raw, err := os.ReadFile("testdata/clock-opening-44042292271.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fixture))
	anchor := time.Unix(fixture.WindowStartUnixSeconds, 0)
	clock := &virtualClock{now: anchor.Add(-4 * time.Second)}
	started := false
	var mu sync.Mutex
	reads, latest := map[string]int{}, map[string]int64{}
	probeReads := 0
	nextProbe := map[string]int64{}
	period := int64(8)
	if slow {
		period = 16
	}
	p := clockAdmissionPorts{converge: true, policy: rehearsalClockPolicy(), leapMode: "unsmeared", poll: 8 * time.Second, latency: 400 * time.Millisecond, now: clock.Now, timeout: clock.Timeout}
	p.pause = func(ctx context.Context, d time.Duration) error {
		// The hosted first read arrived about five seconds after its threshold.
		if d == 243*time.Second {
			d += 5 * time.Second
		}
		return clock.Pause(ctx, d)
	}
	p.start = func(ctx context.Context, _ time.Time) error {
		if slow {
			if err := clock.Pause(ctx, 9*time.Second); err != nil {
				return err
			}
		}
		anchor, started = clock.Now(), true
		return nil
	}
	p.read = func(_ context.Context, slot, _ string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		reads[slot]++
		var seconds []int64
		if !started {
			// Convergence consumes the old target history. On the passing read
			// only the frontend has new rows; the target probe timelines stay empty.
			for _, at := range fixture.TrackingSeconds[slot] {
				if reads[slot] == 1 && at < 0 && (slot != "shared" || at <= -18) || reads[slot] == 2 && slot == "shared" && (at == -10 || at == -2) {
					seconds = append(seconds, at)
				}
			}
		} else {
			if reads[slot] == 3 {
				if slow && (slot == "w1" || slot == "w2") {
					seconds = append(seconds, 16)
				}
				for _, at := range fixture.TrackingSeconds[slot] {
					if at > 0 && (!slow || slot != "w1" && slot != "w2" || at > 16) {
						seconds = append(seconds, at)
					}
				}
				latest[slot] = seconds[len(seconds)-1]
			}
			for at := latest[slot] + period; at <= clock.Now().Unix()-anchor.Unix()-1; at += period {
				seconds = append(seconds, at)
				latest[slot] = at
			}
		}
		log := &clocks.TrackingLog{ReadComplete: true}
		for _, at := range seconds {
			row := clocks.TrackingRow{UnixSeconds: anchor.Unix() + at, Reference: "fixture", Stratum: 2, LeapStatus: "N", MaxErrorSeconds: 0.00001, MaxErrorDecimal: "1.000e-05"}
			if !started && slot == "shared" && reads[slot] == 1 {
				row.MaxErrorSeconds, row.MaxErrorDecimal = 0.0001099, "1.099e-04"
			}
			log.Rows = append(log.Rows, row)
		}
		ns := clock.Now().UnixNano()
		wire := struct {
			clocks.Sample
			ProbeLog *clocks.ProbeBatch `json:"probeLog,omitempty"`
		}{Sample: clocks.Sample{TrackingLog: log, ErrorBoundNS: 10002, RateErrorPPM: 0.2, MeasuredUnixNS: fmt.Sprint(ns), Lease: &clocks.LeaseObservation{Available: true, ErrorBoundNS: 10002, MeasuredUnixNS: fmt.Sprint(ns), ReferenceUnixNS: fmt.Sprint(ns - 100000000), ReferenceAgeNS: 100000000, PollSeconds: "8", ResidualPPM: "0.1", SkewPPM: "0.1", DispersionStartSeconds: "0.000001000", DispersionEndSeconds: "0.000001200", IntervalNS: 200000000, HostElapsedNS: 250000000}}}
		if started && slot == "shared" {
			var lines strings.Builder
			for target, name := range []string{"control", "w1", "w2"} {
				var offsets []int64
				if probeReads == 0 {
					offsets = fixture.ProbeSendOffsetsNS[name]
					nextProbe[name] = (offsets[len(offsets)-1]/1e9+1)*1e9 + 900000000
				} else {
					for nextProbe[name]+2e9 <= clock.Now().Sub(anchor).Nanoseconds() {
						offsets = append(offsets, nextProbe[name])
						nextProbe[name] += 1e9
					}
				}
				for _, offset := range offsets {
					send := anchor.UnixNano() + offset
					_, _ = fmt.Fprintf(&lines, "%d %019d %019d %019d %019d 0\n", target, send, send+1000, send+1000, send+2000)
				}
			}
			probeReads++
			cursor := json.RawMessage(fmt.Sprintf(`{"file":"fixture","position":%d,"anchor":"%s"}`, probeReads, strings.Repeat("a", 64)))
			wire.ProbeLog = &clocks.ProbeBatch{Lines: lines.String(), Cursor: cursor}
			require.LessOrEqual(t, lines.Len(), 65536, "the hosted probe batch was not truncated")
		}
		raw, err := json.Marshal(wire)
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

func TestRehearsalClockSelectionHostedOpeningRecovers(t *testing.T) {
	p := hostedOpeningFixture(t, false)
	selections := 0
	var firstStart int64
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		if selections == 1 {
			firstStart = e.StartUnixSeconds
			require.Equal(t, int64(1794902338), e.StartUnixSeconds)
			require.Equal(t, int64(1794902578), e.EndUnixSeconds)
			require.Equal(t, map[string]int{"relay": 29, "shared": 28, "w1": 29, "w2": 28}, failedClockWindow(e, "clock-select-probes").Updates)
			unchecked := 0
			first, last := int64(1<<63-1), int64(0)
			for _, r := range e.Falsification.Records {
				send, err := strconv.ParseInt(r.FrontendSend, 10, 64)
				require.NoError(t, err)
				first, last = min(first, send), max(last, send)
				if send >= e.StartUnixSeconds*1e9 && send <= e.EndUnixSeconds*1e9 && r.Status == "unchecked" {
					unchecked++
					require.Equal(t, "no-coverage", r.Reason)
					require.Contains(t, []string{"w1", "w2"}, r.Target)
					require.InDelta(t, 2.9e9, float64(send-e.StartUnixSeconds*1e9), 2.1e9)
				}
			}
			require.Equal(t, 9, unchecked, "real ProbeHistory must reproduce the nine hosted opening refusals")
			require.InDelta(t, -0.087e9, float64(first-e.StartUnixSeconds*1e9), 0.001e9)
			require.Greater(t, last, e.EndUnixSeconds*1e9)
			require.Equal(t, clocks.ProbeLossCounts{}, e.Falsification.LossCounts)
		}
		selected, err := clocks.Select(e, policy)
		if err != nil {
			require.EqualError(t, err, "clock-select-probes")
			return selected, err.Error()
		}
		require.GreaterOrEqual(t, e.StartUnixSeconds, firstStart+5, "admitted window must clear the uncovered hosted prefix")
		return selected, ""
	}
	r := collectClockSelection(context.Background(), "hosted-opening", "shared", "shared", p)
	require.Empty(t, r.Cause, "MUTATION: hosted-opening-recovery")
	require.NotNil(t, r.Bounds)
	require.Greater(t, selections, 1)
	require.Empty(t, r.FailedWindows)
}

func TestRehearsalClockSelectionOpeningRecoveryBudget(t *testing.T) {
	// A target's first row arrives at +16 s. Probes through +16.9 s
	// cannot acquire a predecessor later; the trailing window must slide.
	p := hostedOpeningFixture(t, true)
	var end, firstRead, admitted time.Time
	start := p.start
	p.start = func(ctx context.Context, until time.Time) error {
		end = until
		return start(ctx, until)
	}
	selections := 0
	var firstStart int64
	p.selectBounds = func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selections++
		if selections == 1 {
			firstStart = e.StartUnixSeconds
			firstRead = p.now()
			var lastUnchecked int64
			for _, r := range e.Falsification.Records {
				if r.Status == "unchecked" && r.Reason == "no-coverage" {
					send, err := strconv.ParseInt(r.FrontendSend, 10, 64)
					require.NoError(t, err)
					if send <= e.EndUnixSeconds*1e9 {
						lastUnchecked = max(lastUnchecked, send-e.StartUnixSeconds*1e9)
					}
				}
			}
			require.Greater(t, lastUnchecked, int64(16.8e9))
			require.Less(t, lastUnchecked, int64(17e9))
		}
		selected, err := clocks.Select(e, policy)
		if err != nil {
			require.EqualError(t, err, "clock-select-probes")
			return selected, err.Error()
		}
		admitted = p.now()
		require.GreaterOrEqual(t, e.StartUnixSeconds, firstStart+17, "admitted window must start after every target timeline can cover it")
		return selected, ""
	}
	r := collectClockSelection(context.Background(), "opening-budget", "shared", "shared", p)
	require.Empty(t, r.Cause, "MUTATION: opening-recovery-budget")
	require.NotNil(t, r.Bounds)
	require.GreaterOrEqual(t, end.Sub(firstRead), 34*time.Second, "at least twice the 17 s uncovered prefix fits")
	require.True(t, admitted.Before(end))
	require.Greater(t, admitted.Sub(firstRead), 17*time.Second, "16 s row cadence must be exercised while sliding")
	require.Empty(t, r.FailedWindows)
	t.Logf("first read leaves %s; recovery takes %s; admitted with %s remaining", end.Sub(firstRead), admitted.Sub(firstRead), end.Sub(admitted))
}
