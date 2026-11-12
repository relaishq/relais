package operatorrehearsal

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/stretchr/testify/require"
)

func admissionFixture(t *testing.T, old bool, selectBounds func(context.Context, clocks.SelectionEvidence, clocks.SelectionPolicy) (clocks.ClockSelection, string)) clockAdmissionPorts {
	t.Helper()
	start := time.Unix(1792886400, 0)
	now := start
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	dir := t.TempDir()
	read := func(_ context.Context, slot, script string) (string, error) {
		cmd := exec.Command("python3", "-B", "../operatorgates/host/clock_admission_test.py", dir, slot, fmt.Sprint(clock().Unix()), fmt.Sprint(old))
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return clockAdmissionPorts{policy: rehearsalClockPolicy(), leapMode: "unsmeared", start: func(context.Context, time.Time) error { return nil }, read: read, timeout: context.WithTimeout, selectBounds: selectBounds, pause: func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		return nil
	}, now: clock}
}

func TestClockSelectionRefreshLease(t *testing.T) {
	for _, mode := range []string{"fresh", "late", "nil-seed"} {
		t.Run(mode, func(t *testing.T) {
			reads, selections, admittingReads := 0, 0, 0
			var cursor json.RawMessage
			p := admissionFixture(t, false, func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
				selections++
				s, err := clocks.Select(e, policy)
				if err != nil {
					return s, err.Error()
				}
				return s, ""
			})
			// Every pause from admission on steps at least 5 s, so no subtest needs a
			// wall-time budget. "late" then makes one stale refresh read at +5 s,
			// reaches the 10 s refresh deadline at +10 s and reads the fresh lease
			// on the retry at +15 s. RefreshLease's polling has its own tests.
			pause := p.pause
			p.pause = func(ctx context.Context, d time.Duration) error {
				if p.now().Unix() >= 1792886525 {
					d = max(d, 5*time.Second)
				}
				return pause(ctx, d)
			}
			read := p.read
			p.read = func(ctx context.Context, slot, script string) (string, error) {
				if slot != "w2" {
					return read(ctx, slot, script)
				}
				reads++
				if mode == "nil-seed" && reads == 2 && script != host.ClockWithLease(cursor, 0, "", nil) {
					t.Error("MUTATION: admission-seed-cursor")
					return "", errors.New("cursor")
				}
				out, err := read(ctx, slot, script)
				if err != nil {
					return out, err
				}
				seed := mode == "nil-seed" && reads == 1
				if !seed {
					if mode == "nil-seed" || p.now().Unix() < 1792886525 {
						return out, nil
					}
					admittingReads++
					if mode == "fresh" && admittingReads > 1 || mode == "late" && p.now().Unix() >= 1792886536 {
						return out, nil
					}
				}
				raw, err := host.Unpack(out, 256000)
				require.NoError(t, err)
				var doc map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &doc))
				if seed {
					var log clocks.TrackingLog
					require.NoError(t, json.Unmarshal(doc["trackingLog"], &log))
					cursor = log.Cursor
					delete(doc, "lease")
				} else {
					var o clocks.LeaseObservation
					require.NoError(t, json.Unmarshal(doc["lease"], &o))
					o.ReferenceAgeNS = 6140000000
					var measured int64
					_, err = fmt.Sscan(o.MeasuredUnixNS, &measured)
					require.NoError(t, err)
					o.ReferenceUnixNS = fmt.Sprint(measured - o.ReferenceAgeNS)
					doc["lease"], err = json.Marshal(o)
					require.NoError(t, err)
				}
				raw, err = json.Marshal(doc)
				require.NoError(t, err)
				var packed bytes.Buffer
				z := gzip.NewWriter(&packed)
				_, err = z.Write(raw)
				require.NoError(t, err)
				require.NoError(t, z.Close())
				return base64.StdEncoding.EncodeToString(packed.Bytes()), nil
			}
			r := collectClockSelection(context.Background(), "refresh-test", "shared", "shared", p)
			if r.Cause != "" || r.Bounds == nil || reads < 2 || selections != 1 || r.Bounds.FrontendControlBoundNS != 120000 || r.Bounds.FrontendWorkerBoundNS != 170000 {
				t.Fatal("MUTATION: admission-retry-dropped", mode, r, reads)
			}
		})
	}
}
func TestClockSelectionAdmissionArtifacts(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "relais-gates")
	out, err := exec.Command("go", "build", "-o", binary, "../../cmd/relais-gates").CombinedOutput()
	require.NoError(t, err, string(out))
	r := &rehearsal{c: RehearseConfig{Output: t.TempDir()}, bridge: binary, runner: LocalRunner{}}
	for _, retry := range []bool{false, true} {
		calls := 0
		var phaseStart, admitted time.Time
		var helpersStarted atomic.Bool
		var pnow func() time.Time
		p := admissionFixture(t, false, func(ctx context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
			calls++
			if retry && calls == 1 {
				return clocks.ClockSelection{}, "clock-lease-missing"
			}
			selection, cause := r.selectClockBounds(ctx, e, policy)
			if cause == "" {
				admitted = pnow()
			}
			return selection, cause
		})
		pnow = p.now
		p.start = func(ctx context.Context, end time.Time) error {
			helpersStarted.Store(true)
			phaseStart = end.Add(-150 * time.Second)
			return p.pause(ctx, 3*time.Second)
		}
		read, ready := p.read, make(chan struct{})
		var readers atomic.Int32
		var missed atomic.Bool
		p.read = func(ctx context.Context, slot, script string) (string, error) {
			if n := readers.Add(1); n <= 4 {
				if helpersStarted.Load() {
					t.Error("MUTATION: admission-seed-order")
				}
				if n == 4 {
					close(ready)
				}
				select {
				case <-ready:
				case <-time.After(3 * time.Second):
					t.Error("MUTATION: admission-parallel")
					return "", errors.New("barrier")
				}
			}
			if retry && slot == "shared" && p.now().Unix() >= 1792886523 && !missed.Swap(true) {
				return "", errors.New("transient read")
			}
			return read(ctx, slot, script)
		}
		result := collectClockSelection(context.Background(), "admission-test", "shared", "shared", p)
		if result.Cause != "" || result.Bounds == nil || result.Bounds.FrontendControlBoundNS != 120000 || result.Bounds.FrontendWorkerBoundNS != 170000 || result.Updates["w2"] < 10 {
			t.Fatal("MUTATION: admission-artifacts", result)
		}
		raw, err := os.ReadFile(filepath.Join(r.c.Output, "clock-admission-evidence.json"))
		require.NoError(t, err)
		if bytes.Contains(raw, []byte("192.0.2.9")) || fmt.Sprintf("%x", sha256.Sum256(raw)) != result.EvidenceSHA256 {
			t.Fatal("MUTATION: admission-redaction", string(raw))
		}
		if !retry && admitted.Sub(phaseStart) != 123*time.Second {
			t.Fatal("MUTATION: admission-first-attempt", admitted.Sub(phaseStart))
		}
		if p.now().Sub(phaseStart) != 152*time.Second {
			t.Fatal("MUTATION: admission-lifetime", p.now())
		}
	}
}
func TestClockSelectionRejectsPreRestoreWindow(t *testing.T) {
	p := admissionFixture(t, true, func(_ context.Context, e clocks.SelectionEvidence, p clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		s, err := clocks.Select(e, p)
		if err != nil {
			return s, err.Error()
		}
		return s, ""
	})
	r := collectClockSelection(context.Background(), "admission-test", "shared", "shared", p)
	if r.Bounds != nil || r.Cause != "clock-select-admission" {
		t.Fatal("MUTATION: admission-pre-restore", r)
	}
}
func TestClockSelectionVerifiesCLI(t *testing.T) {
	var evidence clocks.SelectionEvidence
	policy := rehearsalClockPolicy()
	p := admissionFixture(t, false, func(_ context.Context, e clocks.SelectionEvidence, p clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		evidence = e
		selection, err := clocks.Select(e, p)
		if err != nil {
			return selection, err.Error()
		}
		return selection, ""
	})
	if got := collectClockSelection(context.Background(), "test", "shared", "shared", p); got.Bounds == nil {
		t.Fatal(got)
	}
	for _, mode := range []string{"tamper", "exit", "empty-error"} {
		r := &rehearsal{c: RehearseConfig{Output: t.TempDir()}, bridge: "relais-gates"}
		r.runner = commandRunner(func(_ context.Context, _ Command) CommandResult {
			s, _ := clocks.Select(evidence, policy)
			status, reason := 1, "clock-select-ceiling"
			if mode == "tamper" {
				s.Receipt.Bounds.FrontendWorkerBoundNS++
				data, _ := json.Marshal(s.Receipt)
				s.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
				status, reason = 0, ""
			}
			if mode == "empty-error" {
				reason = ""
			}
			data, _ := json.Marshal(s)
			return CommandResult{Code: &status, Stdout: data, Stderr: []byte(reason)}
		})
		if _, cause := r.selectClockBounds(context.Background(), evidence, policy); cause == "" {
			t.Fatal("MUTATION: admission-cli-binding", mode, cause)
		}
	}
}

func TestClockSelectionReportNeutral(t *testing.T) {
	for _, cause := range []string{"clock-select-ceiling", "private-clock-label", ""} {
		r := passingReport()
		var before, after bytes.Buffer
		require.NoError(t, r.Write(&before))
		stage := &rehearsal{report: &r, c: RehearseConfig{Output: t.TempDir()}, diagnostics: io.Discard}
		stage.recordClockSelectionReport(&ClockSelectionReport{Cause: cause, Bounds: &clocks.Bounds{FrontendControlBoundNS: 120000, FrontendWorkerBoundNS: 170000, DriftPPM: 100}, Updates: map[string]int{"w2": 120}})
		require.NoError(t, r.Write(&after))
		var a, b map[string]json.RawMessage
		_ = json.Unmarshal(before.Bytes(), &a)
		_ = json.Unmarshal(after.Bytes(), &b)
		delete(b, "clockSelection")
		aa, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		if !bytes.Equal(aa, bb) {
			t.Fatal("MUTATION: selection-verdict-neutral")
		}
		if bytes.Contains(after.Bytes(), []byte("private-clock-label")) {
			t.Fatal("MUTATION: selection-cause-redaction")
		}
	}
}

func TestClockSelectionPartialStartWaits(t *testing.T) {
	for _, when := range []string{"none", "start", "wait"} {
		ctx, cancel := context.WithCancel(context.Background())
		p := admissionFixture(t, false, nil)
		pause := p.pause
		p.pause = func(ctx context.Context, d time.Duration) error {
			if when == "wait" && d > 10*time.Second {
				cancel()
			}
			return pause(ctx, d)
		}
		var began time.Time
		p.start = func(ctx context.Context, end time.Time) error {
			began = p.now()
			if end.Sub(began) != 150*time.Second {
				t.Fatal("MUTATION: admission-lifetime")
			}
			_ = p.pause(ctx, 10*time.Second)
			if when == "start" {
				cancel()
			}
			return errors.New("partial start")
		}
		result := collectClockSelection(ctx, "test", "shared", "shared", p)
		want := 152 * time.Second
		if when != "none" {
			want = 10 * time.Second
		}
		if result.Cause != "clock-selection-helper" || p.now().Sub(began) != want {
			t.Fatal("MUTATION: admission-cancel-prompt", result, p.now().Sub(began))
		}
		cancel()
	}
}

func TestClockSelectionStickyProbe(t *testing.T) {
	records := mergeClockProbeRecords(nil, []clocks.ProbeRecord{{Target: "w1", FrontendSend: "1", Status: "falsified"}})
	changed := records[0]
	changed.Status = "not-falsified"
	if got := mergeClockProbeRecords(records, []clocks.ProbeRecord{changed}); got[0].Status != "falsified" {
		t.Fatal("MUTATION: admission-sticky")
	}
}

// A lease aged just above the 5-poll budget names the slot, aged age and budget.
// With two expired slots the refusal names the lowest slot name, as the selector
// does: "shared" sorts before "w1" but is read after it.
func TestClockSelectionLeaseRefusalNamesSlot(t *testing.T) {
	const referenceAgeNS = 5200000000
	for named, expired := range map[string][]string{"w2": {"w2"}, "shared": {"w1", "shared"}} {
		t.Run(strings.Join(expired, "+"), func(t *testing.T) {
			p := admissionFixture(t, false, func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
				s, err := clocks.Select(e, policy)
				if err != nil {
					return s, err.Error()
				}
				return s, ""
			})
			// Every pause after the first admitting read steps past the admission
			// deadline: one attempt and one refresh poll, with no wall-time budget.
			// The retry and refresh loops have their own tests.
			pause := p.pause
			p.pause = func(ctx context.Context, d time.Duration) error {
				if p.now().Unix() >= 1792886525 {
					d = max(d, 30*time.Second)
				}
				return pause(ctx, d)
			}
			read := p.read
			var mu sync.Mutex
			elapsed := map[string]int64{}
			p.read = func(ctx context.Context, slot, script string) (string, error) {
				out, err := read(ctx, slot, script)
				if err != nil || !slices.Contains(expired, slot) || p.now().Unix() < 1792886525 {
					return out, err
				}
				raw, err := host.Unpack(out, 256000)
				require.NoError(t, err)
				var doc map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &doc))
				var o clocks.LeaseObservation
				require.NoError(t, json.Unmarshal(doc["lease"], &o))
				var measured int64
				_, err = fmt.Sscan(o.MeasuredUnixNS, &measured)
				require.NoError(t, err)
				o.ReferenceAgeNS, o.ReferenceUnixNS = referenceAgeNS, fmt.Sprint(measured-referenceAgeNS)
				mu.Lock()
				elapsed[slot] = o.HostElapsedNS
				mu.Unlock()
				doc["lease"], err = json.Marshal(o)
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
			r := collectClockSelection(context.Background(), "refusal-test", "shared", "shared", p)
			require.Equal(t, "clock-lease-expired", r.Cause)
			require.Nil(t, r.Bounds)
			require.Len(t, elapsed, len(expired))
			if r.LeaseRefusal == nil || r.LeaseRefusal.Slot != named {
				t.Fatal("MUTATION: lease-refusal-lowest-slot", r.LeaseRefusal)
			}
			require.Equal(t, &ClockLeaseRefusal{Slot: named, AgedLeaseAgeNS: referenceAgeNS + elapsed[named], BudgetNS: 5000000000}, r.LeaseRefusal)
			raw, err := json.Marshal(r)
			require.NoError(t, err)
			require.Contains(t, string(raw), `"leaseRefusal":{"slot":"`+named+`","agedLeaseAgeNs":`)
		})
	}
}

// The admitted status file keeps its exact bytes; refusal fields are absent.
func TestClockSelectionAdmittedStatusBytes(t *testing.T) {
	r := &ClockSelectionReport{Bounds: &clocks.Bounds{FrontendControlBoundNS: 120000, FrontendWorkerBoundNS: 170000, DriftPPM: 100}, Updates: map[string]int{"w2": 120}, ReceiptSHA256: "aa", EvidenceSHA256: "bb"}
	dir := t.TempDir()
	stage := &rehearsal{report: &Report{}, c: RehearseConfig{Output: dir}, diagnostics: io.Discard}
	stage.recordClockSelectionReport(r)
	got, err := os.ReadFile(filepath.Join(dir, "clock-selection-status.json"))
	require.NoError(t, err)
	require.Equal(t, `{"cause":"","selectedBounds":{"frontendControlBoundNs":120000,"frontendWorkerBoundNs":170000,"driftPpm":100},"updates":{"w2":120},"receiptSha256":"aa","evidenceSha256":"bb"}`, string(got))
}

type virtualDeadlineKey struct{}

// virtualClock is an admission clock whose timeouts fire when Pause or a fake
// host command advances it past their deadline. Nothing waits in real time.
type virtualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []virtualTimer
}

type virtualTimer struct {
	at     time.Time
	cancel context.CancelFunc
}

func (c *virtualClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// advance moves time forward to at (never back) and fires due timeouts.
func (c *virtualClock) advance(at time.Time) {
	c.mu.Lock()
	if at.After(c.now) {
		c.now = at
	}
	var due []context.CancelFunc
	kept := c.timers[:0]
	for _, t := range c.timers {
		if t.at.After(c.now) {
			kept = append(kept, t)
		} else {
			due = append(due, t.cancel)
		}
	}
	c.timers = kept
	c.mu.Unlock()
	for _, cancel := range due {
		cancel()
	}
}

func virtualDeadline(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(virtualDeadlineKey{}).(time.Time)
	return at, ok
}

func (c *virtualClock) Timeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	at := c.Now().Add(d)
	if outer, ok := virtualDeadline(parent); ok && outer.Before(at) {
		at = outer
	}
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.timers = append(c.timers, virtualTimer{at, cancel})
	c.mu.Unlock()
	c.advance(time.Time{})
	return context.WithValue(ctx, virtualDeadlineKey{}, at), cancel
}

// spend runs a host action that takes d from start; at its context deadline
// it is killed instead, as the real command would be.
func (c *virtualClock) spend(ctx context.Context, start time.Time, d time.Duration) error {
	if at, ok := virtualDeadline(ctx); ok && at.Before(start.Add(d)) {
		c.advance(at)
		return context.DeadlineExceeded
	}
	c.advance(start.Add(d))
	return ctx.Err()
}

func (c *virtualClock) Pause(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return c.spend(ctx, c.Now(), d)
}

// clockAdmissionRun is one admission against fake hosts. Every host command
// takes cost of virtual time; the four parallel reads of an attempt
// share one issue time, and the host samples its tracking log when the read
// is issued. Tracking rows come every poll period at phase; the frontend
// helper probes from the moment its start command returns.
type clockAdmissionRun struct {
	began, setupDone, end, admitted time.Time
	result                          *ClockSelectionReport
}

func runClockAdmission(t *testing.T, latency, poll, cost time.Duration, phase int64) clockAdmissionRun {
	t.Helper()
	clock := &virtualClock{now: time.Unix(1792886400, 0)}
	dir := t.TempDir()
	probes := int64(1 << 40)
	var mu sync.Mutex
	var batch *struct {
		issued time.Time
		ready  chan struct{}
		n      int
	}
	interval := max(int64(poll/time.Second), 1)
	a := ClockAdmission{RunID: "timing-test", FrontendSlot: "shared", ControlSlot: "shared", CallerLeapMode: "unsmeared", Policy: rehearsalClockPolicy(), Addresses: map[string]string{"relay": "192.0.2.1", "shared": "192.0.2.2", "w1": "192.0.2.3", "w2": "192.0.2.4"}, HelpersUntil: clock.Now().Add(time.Hour), Output: t.TempDir(), HostReadLatency: latency, PollPeriod: poll, Now: clock.Now, Pause: clock.Pause, Timeout: clock.Timeout}
	a.Host = func(ctx context.Context, slot string, detach bool, input []byte, args ...string) (string, error) {
		if args[0] != "sh" {
			if err := clock.spend(ctx, clock.Now(), cost); err != nil {
				return "", err
			}
			if detach && slot == a.FrontendSlot {
				mu.Lock()
				probes = (clock.Now().UnixNano() + 999999999) / 1e9
				mu.Unlock()
			}
			return "", nil
		}
		mu.Lock()
		if batch == nil {
			batch = &struct {
				issued time.Time
				ready  chan struct{}
				n      int
			}{issued: clock.Now(), ready: make(chan struct{})}
		}
		b := batch
		if b.n++; b.n == len(Slots) {
			close(b.ready)
			batch = nil
		}
		from := probes
		mu.Unlock()
		select {
		case <-b.ready:
		case <-time.After(10 * time.Second):
			return "", errors.New("read batch incomplete")
		}
		cmd := exec.Command("python3", "-B", "../operatorgates/host/clock_admission_test.py", dir, slot, fmt.Sprint(b.issued.Unix()), "false", fmt.Sprint(interval), fmt.Sprint(phase), fmt.Sprint(from))
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", err
		}
		return string(out), clock.spend(ctx, b.issued, cost)
	}
	var r clockAdmissionRun
	p := a.ports()
	start, selectBounds := p.start, p.selectBounds
	p.start = func(ctx context.Context, end time.Time) error {
		r.began, r.end = clock.Now(), end
		err := start(ctx, end)
		r.setupDone = clock.Now()
		return err
	}
	p.selectBounds = func(ctx context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		selection, cause := selectBounds(ctx, e, policy)
		if cause == "" {
			r.admitted = clock.Now()
		}
		return selection, cause
	}
	r.result = collectClockSelection(context.Background(), a.RunID, a.FrontendSlot, a.ControlSlot, p)
	return r
}

// Admission at the configured timing: delayed reads, probes only after helper
// setup and slow tracking rows, at every row phase. No flags keep 10 s and 150 s.
func TestClockAdmissionDerivedWindow(t *testing.T) {
	for name, c := range map[string]struct {
		latency, poll time.Duration
		setup, window time.Duration
		phases        int64
	}{
		"reference":  {0, 0, 10 * time.Second, 150 * time.Second, 1},
		"L=2.5s,P=8": {2500 * time.Millisecond, 8 * time.Second, 22 * time.Second, 157 * time.Second, 8},
		"L=4s,P=8":   {4 * time.Second, 8 * time.Second, 34 * time.Second, 172 * time.Second, 8},
		// The seed needs 2L + 5 s: a fixed 10 s ends before the first read and pause.
		"L=9s,P=10": {9 * time.Second, 10 * time.Second, 74 * time.Second, 224 * time.Second, 10},
		// The largest timing a session accepts that admission can select.
		"L=10s,P=10": {10 * time.Second, 10 * time.Second, 82 * time.Second, 234 * time.Second, 10},
	} {
		for phase := range c.phases {
			t.Run(fmt.Sprintf("%s/phase=%d", name, phase), func(t *testing.T) {
				t.Parallel()
				r := runClockAdmission(t, c.latency, c.poll, c.latency, phase)
				if r.result.Cause != "" || r.result.Bounds == nil || r.end.Sub(r.began) != c.window || r.setupDone.Sub(r.began) > c.setup || r.admitted.After(r.end) {
					t.Fatal("MUTATION: admission-derived-window", r.result.Cause, r.end.Sub(r.began), r.setupDone.Sub(r.began), r.admitted.Sub(r.began))
				}
				if c.latency == 0 && r.admitted.Sub(r.began) != 123*time.Second {
					t.Fatal("MUTATION: admission-derived-window", "reference first attempt", r.admitted.Sub(r.began))
				}
			})
		}
	}
}

// Characterization: with no latency given, the fixed 10 s setup kills the
// fourth 2.5 s helper command. The derived cases above complete setup.
func TestClockAdmissionFixedSetupBudget(t *testing.T) {
	r := runClockAdmission(t, 0, 0, 2500*time.Millisecond, 0)
	if r.result.Cause != "clock-selection-helper" || r.setupDone.Sub(r.began) != 10*time.Second {
		t.Fatal("MUTATION: admission-setup-budget", r.result.Cause, r.setupDone.Sub(r.began))
	}
}

// The derivation table, and the up-front refusals: rows too sparse for the
// selector's minimum updates, and a seed plus window above 300 s.
func TestClockAdmissionTiming(t *testing.T) {
	s := func(seconds float64) time.Duration { return time.Duration(seconds * float64(time.Second)) }
	for _, c := range []struct {
		latency, poll, seed, setup, window time.Duration
		cause                              string
	}{
		{0, 0, s(10), s(10), s(150), ""},
		{0, s(1), s(10), s(10), s(150), ""},
		{s(2.5), s(8), s(10), s(22), s(157), ""},
		{s(4), s(8), s(13), s(34), s(172), ""},
		{s(9), s(10), s(23), s(74), s(224), ""},
		{s(10), s(10), s(25), s(82), s(234), ""},
		// Exactly 300 s, then 12 ms over.
		{s(13.25), s(12), s(31.5), s(108), s(268.5), ""},
		{s(13.251), s(12), s(31.502), s(108.008), s(268.51), "clock-admit-budget"},
		// Twelve-second rows still give the selector ten updates in 120 s.
		{s(1), s(13), s(10), s(10), s(150), "clock-admit-poll"},
		{s(4), s(16), s(13), s(34), s(180), "clock-admit-poll"},
	} {
		seed, setup, window := clockAdmissionTiming(c.latency, c.poll)
		err := CheckClockAdmissionTiming(c.latency, c.poll)
		if seed != c.seed || setup != c.setup || window != c.window || c.cause == "" != (err == nil) || err != nil && err.Error() != c.cause {
			t.Fatal("MUTATION: admission-timing", c.latency, c.poll, seed, setup, window, err)
		}
	}
}
