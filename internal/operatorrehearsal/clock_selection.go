package operatorrehearsal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

const clockProbePath = "/run/relais-clock-probes.bin"
const clockStopPath = "/run/relais-clock-stop"

type ClockSelectionReport struct {
	Cause          string         `json:"cause"`
	Bounds         *clocks.Bounds `json:"selectedBounds,omitempty"`
	Updates        map[string]int `json:"updates,omitempty"`
	ReceiptSHA256  string         `json:"receiptSha256,omitempty"`
	EvidenceSHA256 string         `json:"evidenceSha256,omitempty"`
}

func clockSelectionCause(cause string) string {
	switch cause {
	case "", "clock-select-policy", "clock-select-admission", "clock-select-column14", "clock-select-coverage", "clock-select-break", "clock-select-leap", "clock-select-updates", "clock-select-trailing-window", "clock-select-probes", "clock-select-growth", "clock-select-ceiling", "clock-select-stale", "clock-lease-missing", "clock-lease-expired", "clock-selection-helper", "clock-selection-read", "clock-selection-artifact", "clock-selection-invalid":
		return cause
	}
	return "clock-selection-unavailable"
}

type clockAdmissionPorts struct {
	start        func(context.Context, time.Time) error
	stop         func(context.Context)
	read         func(context.Context, string, string) (string, error)
	selectBounds func(context.Context, clocks.SelectionEvidence, clocks.SelectionPolicy) (clocks.ClockSelection, string)
	pause        func(context.Context, time.Duration) error
	now          func() time.Time
}

func rehearsalClockPolicy() clocks.SelectionPolicy {
	return clocks.SelectionPolicy{WindowSeconds: 120, FloorNS: 50000, ScaleNumerator: 2, ScaleDenominator: 1, AllowanceNS: 10000, SignalReserveNS: 50000, ControlCeilingNS: 250000, WorkerCeilingNS: 350000, MaxDispersionGrowthPPM: "5"}
}

// This step deliberately bypasses Session/Peer: diagnostic failures cannot
// latch the session, add policy clock samples or change the fixed caller flags.
func (r *rehearsal) recordClockSelection(ctx context.Context) {
	components := map[string][]string{}
	for _, slot := range Slots {
		var node struct {
			Components []string `json:"components"`
		}
		_ = json.Unmarshal([]byte(r.topology.Hosts[slot].Metadata.Files["/etc/relais/node.json"].Content), &node)
		components[slot] = node.Components
	}
	front, f := inventory.ComponentSlot(components, "signal")
	control, c := inventory.ComponentSlot(components, "control")
	r.recordClockSelectionReport(collectClockSelection(ctx, r.run, front, control, clockAdmissionPorts{
		stop: r.stopClockHelpers, read: r.readAdmissionClock, selectBounds: r.selectClockBounds, pause: pause, now: time.Now,
		start: func(ctx context.Context, end time.Time) error {
			if !f || !c {
				return errors.New("clock-selection-helper")
			}
			return r.startClockHelpers(ctx, front, control, end)
		},
	}))
}

func (r *rehearsal) recordClockSelectionReport(result *ClockSelectionReport) {
	r.report.ClockSelection = result
	raw, _ := json.Marshal(result)
	_, _ = fmt.Fprintf(r.diagnostics, "clock-selection %s\n", raw)
	if saveClockJSON(filepath.Join(r.c.Output, "clock-selection-status.json"), result) != nil {
		r.report.ClockSelection = &ClockSelectionReport{Cause: "clock-selection-artifact"}
	}
}

func (r *rehearsal) clockHostCommand(ctx context.Context, slot string, options []string, input []byte, args ...string) (string, error) {
	command, err := r.deployment.execArgs(slot, options, args...)
	if err != nil {
		return "", err
	}
	result := r.runner.Run(ctx, Command{Label: "clock-admission", Args: command, Input: input})
	if result.Err != nil || result.Code == nil || *result.Code != 0 || result.Truncated {
		return "", errors.New("clock-selection-read")
	}
	return string(result.Stdout), nil
}

func (r *rehearsal) startClockHelpers(ctx context.Context, front, control string, end time.Time) error {
	source, err := host.Assets.ReadFile("clock_probe.py")
	if err != nil {
		return err
	}
	// Start every endpoint before starting the frontend's three probe loops.
	order := []string{}
	for _, slot := range Slots {
		if slot != front {
			order = append(order, slot)
		}
	}
	order = append(order, front)
	for _, slot := range order {
		_, err = r.clockHostCommand(ctx, slot, []string{"-i"}, source, "python3", "-c", "import sys; open('/run/relais-clock-helper.py','x').write(sys.stdin.read())")
		if err != nil {
			return err
		}
		seconds := int(time.Until(end) / time.Second)
		if seconds < 1 {
			return errors.New("clock-selection-helper")
		}
		args := []string{"python3", "/run/relais-clock-helper.py", "--listen", r.topology.Hosts[slot].IP + ":24891", "--seconds", strconv.Itoa(seconds), "--stop-file", clockStopPath}
		if slot == front {
			args = append(args, "--output", clockProbePath)
			for _, target := range []string{"control", "w1", "w2"} {
				peer := target
				if peer == "control" {
					peer = control
				}
				args = append(args, "--peer", target+"=http://"+r.topology.Hosts[peer].IP+":24891")
			}
		}
		if _, err = r.clockHostCommand(ctx, slot, []string{"-d"}, nil, args...); err != nil {
			return err
		}
	}
	return nil
}

func (r *rehearsal) readAdmissionClock(ctx context.Context, slot, script string) (string, error) {
	return r.clockHostCommand(ctx, slot, []string{"-i"}, []byte(script), "sh", "-eu")
}

func collectClockSelection(parent context.Context, run, front, control string, p clockAdmissionPorts) *ClockSelectionReport {
	// Seed/drain old history before helper startup or the admission deadline.
	ctx, seedDone := context.WithTimeout(parent, 10*time.Second)
	defer seedDone()
	var started, end time.Time
	defer func() {
		if end.IsZero() {
			return
		}
		if parent.Err() == nil {
			_ = p.pause(parent, max(0, end.Add(2*time.Second).Sub(p.now())))
		}
		if parent.Err() != nil {
			stopCtx, stopDone := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopDone()
			p.stop(stopCtx)
		}
	}()
	result := &ClockSelectionReport{Cause: "clock-selection-unavailable"}
	logs := map[string]clocks.TrackingLog{}
	cursors := map[string]json.RawMessage{}
	var probes clocks.ProbeHistory
	var records []clocks.ProbeRecord
	var probeCursor json.RawMessage
	for (end.IsZero() || p.now().Before(end)) && ctx.Err() == nil {
		batch := map[string]clocks.Sample{}
		e := clocks.SelectionEvidence{RunID: run, FrontendSlot: front, ControlSlot: control, CallerLeapMode: "unsmeared", EndUnixSeconds: 1 << 62}
		admitting := !end.IsZero() && !p.now().Before(started.Add(123*time.Second))
		samples, failed := make([]clocks.Sample, len(Slots)), make([]error, len(Slots))
		var readers sync.WaitGroup
		for i, slot := range Slots {
			readers.Go(func() {
				path := ""
				if slot == front && admitting {
					path = clockProbePath
				}
				out, err := p.read(ctx, slot, host.ClockWithLease(cursors[slot], 0, path, probeCursor))
				if err == nil {
					var raw []byte
					raw, err = host.Unpack(out, 256000)
					if err == nil {
						samples[i], err = clocks.Parse(raw)
					}
				}
				failed[i] = err
			})
		}
		readers.Wait()
		complete := true
		for i, slot := range Slots {
			sample := samples[i]
			if failed[i] != nil || sample.TrackingLog == nil {
				result.Cause, complete = "clock-selection-read", false
				continue
			}
			complete = complete && sample.TrackingLog.ReadComplete
			batch[slot] = sample
			cursors[slot] = sample.TrackingLog.Cursor
			log := logs[slot]
			log.Rows = append(log.Rows, sample.TrackingLog.Rows...)
			log.Breaks = append(log.Breaks, sample.TrackingLog.Breaks...)
			log.ReadComplete = sample.TrackingLog.ReadComplete
			logs[slot] = log
			sample.TrackingLog = &log
			sample = clocks.PublicSample(sample)
			log = *sample.TrackingLog
			log.Cursor = nil
			if len(log.Rows) > 0 {
				e.EndUnixSeconds = min(e.EndUnixSeconds, log.Rows[len(log.Rows)-1].UnixSeconds-1)
			}
			h := clocks.SelectionHost{Slot: slot, LeapMode: "unsmeared", Tracking: log}
			if sample.Lease != nil {
				h.Lease = *sample.Lease
			}
			e.Hosts = append(e.Hosts, h)
		}
		if end.IsZero() && complete {
			// Keep seed overflow breaks outside the selector's two-second margin.
			if p.pause(ctx, 2*time.Second) != nil {
				return result
			}
			seedDone()
			started = p.now()
			end = started.Add(150 * time.Second)
			admission, admissionDone := context.WithTimeout(parent, 150*time.Second)
			defer admissionDone()
			ctx = admission
			setup, setupDone := context.WithTimeout(ctx, 10*time.Second)
			err := p.start(setup, end)
			setupDone()
			if err != nil {
				return &ClockSelectionReport{Cause: "clock-selection-helper"}
			}
		}
		probeFront := ""
		if admitting && batch[front].TrackingLog != nil {
			probeFront = front
		}
		if next := probes.Collect(batch, probeFront, batch[front].ProbeLog); len(next) > 0 {
			probeCursor = next
		}
		e.Falsification = *probes.Snapshot(front, map[string]string{"control": control, "w1": "w1", "w2": "w2"})
		records = mergeClockProbeRecords(records, e.Falsification.Records)
		e.Falsification.Records = records
		e.StartUnixSeconds = e.EndUnixSeconds - 120
		if admitting && complete {
			if time.Unix(e.StartUnixSeconds, 0).Before(started) {
				result.Cause = "clock-select-admission"
			} else {
				selection, cause := p.selectBounds(ctx, e, rehearsalClockPolicy())
				result.Cause = clockSelectionCause(cause)
				if cause == "" {
					result.Bounds = &selection.Receipt.Bounds
					result.ReceiptSHA256, result.EvidenceSHA256 = selection.SHA256, selection.Receipt.EvidenceSHA256
					result.Updates = map[string]int{}
					for _, h := range selection.Receipt.Hosts {
						result.Updates[h.Slot] = h.Updates
					}
					break
				}
			}
		}
		delay := 2 * time.Second
		if !end.IsZero() {
			delay = min(delay, end.Sub(p.now()))
		}
		if complete && !end.IsZero() && !admitting {
			delay = min(end.Sub(p.now()), started.Add(123*time.Second).Sub(p.now()))
		}
		if p.pause(ctx, delay) != nil {
			break
		}
	}
	return result
}

func saveClockJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func (r *rehearsal) selectClockBounds(ctx context.Context, e clocks.SelectionEvidence, policy clocks.SelectionPolicy) (clocks.ClockSelection, string) {
	sort.Slice(e.Hosts, func(i, j int) bool { return e.Hosts[i].Slot < e.Hosts[j].Slot })
	evidence, config := filepath.Join(r.c.Output, "clock-admission-evidence.json"), filepath.Join(r.c.Output, "clock-selection-policy.json")
	var selected clocks.ClockSelection
	if saveClockJSON(evidence, e) != nil || saveClockJSON(config, policy) != nil {
		return selected, "clock-selection-artifact"
	}
	result := r.runner.Run(ctx, Command{Label: "clock-select", Args: []string{r.bridge, "clock-select", "-evidence", evidence, "-policy", config}})
	if result.Err != nil || result.Code == nil || result.Truncated {
		return selected, "clock-selection-invalid"
	}
	if *result.Code != 0 {
		cause := clockSelectionCause(strings.TrimSpace(string(result.Stderr)))
		if cause == "" {
			cause = "clock-selection-invalid"
		}
		return selected, cause
	}
	want, err := clocks.Select(e, policy)
	if err != nil || observe.DecodeClosedLimit(result.Stdout, &selected, nil, 256000) != nil || selected.SHA256 != want.SHA256 {
		return clocks.ClockSelection{}, "clock-selection-invalid"
	}
	// Compare the receipt itself, not just a runner-supplied digest string.
	raw, _ := json.Marshal(selected.Receipt)
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != selected.SHA256 {
		return clocks.ClockSelection{}, "clock-selection-invalid"
	}
	if saveClockJSON(filepath.Join(r.c.Output, "clock-selection.json"), selected) != nil {
		return selected, "clock-selection-artifact"
	}
	return selected, ""
}

func mergeClockProbeRecords(records, updates []clocks.ProbeRecord) []clocks.ProbeRecord {
	for _, record := range updates {
		i := slices.IndexFunc(records, func(r clocks.ProbeRecord) bool {
			return r.Target == record.Target && r.FrontendSend == record.FrontendSend
		})
		if i < 0 {
			records = append(records, record)
		} else if records[i].Status != "falsified" {
			records[i] = record
		}
	}
	return records
}

// Cooperative stop flags also cover a detached helper that starts after cancellation.
func (r *rehearsal) stopClockHelpers(ctx context.Context) {
	var workers sync.WaitGroup
	for _, slot := range Slots {
		workers.Go(func() { _, _ = r.clockHostCommand(ctx, slot, nil, nil, "touch", clockStopPath) })
	}
	workers.Wait()
}
