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
	return clockAdmissionPorts{policy: rehearsalClockPolicy(), leapMode: "unsmeared", start: func(context.Context, time.Time) error { return nil }, read: read, selectBounds: selectBounds, pause: func(ctx context.Context, d time.Duration) error {
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
func TestClockSelectionLeaseRefusalNamesSlot(t *testing.T) {
	const referenceAgeNS = 5200000000
	p := admissionFixture(t, false, func(_ context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
		s, err := clocks.Select(e, policy)
		if err != nil {
			return s, err.Error()
		}
		return s, ""
	})
	read := p.read
	var elapsed int64
	p.read = func(ctx context.Context, slot, script string) (string, error) {
		out, err := read(ctx, slot, script)
		if err != nil || slot != "w2" || p.now().Unix() < 1792886525 {
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
		elapsed = o.HostElapsedNS
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
	require.Equal(t, &ClockLeaseRefusal{Slot: "w2", AgedLeaseAgeNS: referenceAgeNS + elapsed, BudgetNS: 5000000000}, r.LeaseRefusal)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"leaseRefusal":{"slot":"w2","agedLeaseAgeNs":`)
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
