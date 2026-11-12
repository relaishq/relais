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

type ClockSelectionReport struct {
	Cause          string         `json:"cause"`
	Bounds         *clocks.Bounds `json:"selectedBounds,omitempty"`
	Updates        map[string]int `json:"updates,omitempty"`
	ReceiptSHA256  string         `json:"receiptSha256,omitempty"`
	EvidenceSHA256 string         `json:"evidenceSha256,omitempty"`
	// Set only on a clock-lease-expired refusal; values come from the sanitized evidence.
	LeaseRefusal *ClockLeaseRefusal `json:"leaseRefusal,omitempty"`
}

type ClockLeaseRefusal struct {
	Slot           string `json:"slot"`
	AgedLeaseAgeNS int64  `json:"agedLeaseAgeNs"`
	BudgetNS       int64  `json:"budgetNs"`
}

// leaseRefusal names the first expired host in slot order, as the selector does.
func leaseRefusal(cause string, hosts []clocks.SelectionHost) *ClockLeaseRefusal {
	if cause != "clock-lease-expired" {
		return nil
	}
	var out *ClockLeaseRefusal
	for _, h := range hosts {
		if age, budget, ok := clocks.ExpiredLeaseAge(h.Lease); ok && (out == nil || h.Slot < out.Slot) {
			out = &ClockLeaseRefusal{Slot: h.Slot, AgedLeaseAgeNS: age, BudgetNS: budget}
		}
	}
	return out
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
	read         func(context.Context, string, string) (string, error)
	selectBounds func(context.Context, clocks.SelectionEvidence, clocks.SelectionPolicy) (clocks.ClockSelection, string)
	pause        func(context.Context, time.Duration) error
	now          func() time.Time
	policy       clocks.SelectionPolicy
	leapMode     string
	timeout      func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	// latency and poll size helper setup and the admission window.
	latency, poll time.Duration
}

func rehearsalClockPolicy() clocks.SelectionPolicy {
	return clocks.SelectionPolicy{WindowSeconds: 120, FloorNS: 50000, ScaleNumerator: 2, ScaleDenominator: 1, AllowanceNS: 10000, SignalReserveNS: 50000, ControlCeilingNS: 250000, WorkerCeilingNS: 350000, MaxDispersionGrowthPPM: "5"}
}

// This step deliberately bypasses Session/Peer: diagnostic failures cannot
// latch the session, add policy clock samples or change the fixed caller flags.
func (r *rehearsal) recordClockSelection(ctx context.Context) {
	components, addresses := map[string][]string{}, map[string]string{}
	for _, slot := range Slots {
		var node struct {
			Components []string `json:"components"`
		}
		_ = json.Unmarshal([]byte(r.topology.Hosts[slot].Metadata.Files["/etc/relais/node.json"].Content), &node)
		components[slot] = node.Components
		addresses[slot] = r.topology.Hosts[slot].IP
	}
	// An unresolved role leaves its slot empty, which refuses helper startup.
	front, _ := inventory.ComponentSlot(components, "signal")
	control, _ := inventory.ComponentSlot(components, "control")
	a := ClockAdmission{RunID: r.run, FrontendSlot: front, ControlSlot: control, CallerLeapMode: "unsmeared", Policy: rehearsalClockPolicy(), Addresses: addresses, Output: r.c.Output, Host: r.clockHostCommand}
	if until, _ := strconv.ParseInt(r.boundInit.EndUnixMS, 10, 64); until != 0 {
		a.HelpersUntil = time.UnixMilli(until)
	}
	p := a.ports()
	// The rehearsal also proves the clock-select CLI against the in-process selector.
	p.selectBounds = r.selectClockBounds
	r.recordClockSelectionReport(collectClockSelection(ctx, a.RunID, front, control, p))
}

func (r *rehearsal) recordClockSelectionReport(result *ClockSelectionReport) {
	r.report.ClockSelection = result
	raw, _ := json.Marshal(result)
	_, _ = fmt.Fprintf(r.diagnostics, "clock-selection %s\n", raw)
	if saveClockJSON(filepath.Join(r.c.Output, "clock-selection-status.json"), result) != nil {
		r.report.ClockSelection = &ClockSelectionReport{Cause: "clock-selection-artifact"}
	}
}

func (r *rehearsal) clockHostCommand(ctx context.Context, slot string, detach bool, input []byte, args ...string) (string, error) {
	option := "-i"
	if detach {
		option = "-d"
	}
	command, err := r.deployment.execArgs(slot, []string{option}, args...)
	if err != nil {
		return "", err
	}
	result := r.runner.Run(ctx, Command{Label: "clock-admission", Args: command, Input: input})
	if result.Err != nil || result.Code == nil || *result.Code != 0 || result.Truncated {
		return "", errors.New("clock-selection-read")
	}
	return string(result.Stdout), nil
}

func collectClockSelection(parent context.Context, run, front, control string, p clockAdmissionPorts) *ClockSelectionReport {
	// Seed/drain old history before helper startup or the admission deadline.
	ctx, seedDone := p.timeout(parent, 10*time.Second)
	setupBudget, window := clockAdmissionTiming(p.latency, p.poll)
	defer seedDone()
	var started, end time.Time
	defer func() {
		if end.IsZero() {
			return
		}
		if parent.Err() == nil {
			_ = p.pause(parent, max(0, end.Add(2*time.Second).Sub(p.now())))
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
		e := clocks.SelectionEvidence{RunID: run, FrontendSlot: front, ControlSlot: control, CallerLeapMode: p.leapMode, EndUnixSeconds: 1 << 62}
		admitting := !end.IsZero() && !p.now().Before(started.Add(123*time.Second))
		samples, failed := make([]clocks.Sample, len(Slots)), make([]error, len(Slots))
		var readers sync.WaitGroup
		for i, slot := range Slots {
			readers.Go(func() {
				path := ""
				if slot == front && admitting {
					path = clockProbePath
				}
				cursor := cursors[slot]
				var retained clocks.TrackingLog
				read := func(ctx context.Context) (clocks.LeaseObservation, error) {
					out, err := p.read(ctx, slot, host.ClockWithLease(cursor, 0, path, probeCursor))
					if err != nil {
						return clocks.LeaseObservation{}, err
					}
					raw, err := host.Unpack(out, 256000)
					if err != nil {
						return clocks.LeaseObservation{}, err
					}
					sample, err := clocks.Parse(raw)
					if err != nil {
						return clocks.LeaseObservation{}, err
					}
					if log := sample.TrackingLog; log != nil {
						retained.Rows = append(retained.Rows, log.Rows...)
						retained.Breaks = append(retained.Breaks, log.Breaks...)
						retained.ReadComplete, retained.Cursor = log.ReadComplete, log.Cursor
						cursor, sample.TrackingLog = log.Cursor, &retained
					}
					samples[i] = sample
					if sample.Lease == nil && admitting {
						return clocks.LeaseObservation{}, errors.New("clock-lease-missing")
					}
					if sample.Lease == nil {
						return clocks.LeaseObservation{}, nil
					}
					return *sample.Lease, nil
				}
				first, err := read(ctx)
				if err == nil && admitting {
					_, err = clocks.RefreshLease(ctx, first, p.now, p.pause, read)
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
				if failed[i] != nil && strings.HasPrefix(failed[i].Error(), "clock-lease-") {
					result.Cause = clockSelectionCause(failed[i].Error())
				}
			}
			if sample.TrackingLog == nil {
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
			h := clocks.SelectionHost{Slot: slot, LeapMode: p.leapMode, Tracking: log}
			if sample.Lease != nil {
				h.Lease = *sample.Lease
			}
			e.Hosts = append(e.Hosts, h)
		}
		// Keep the last named slot if a later expired read cannot be attributed; clear it on any other cause.
		if refusal := leaseRefusal(result.Cause, e.Hosts); refusal != nil || result.Cause != "clock-lease-expired" {
			result.LeaseRefusal = refusal
		}
		if end.IsZero() && complete {
			// Keep seed overflow breaks outside the selector's two-second margin.
			if p.pause(ctx, 2*time.Second) != nil {
				return result
			}
			seedDone()
			started = p.now()
			end = started.Add(window)
			admission, admissionDone := p.timeout(parent, window)
			defer admissionDone()
			ctx = admission
			setup, setupDone := p.timeout(ctx, setupBudget)
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
		e.StartUnixSeconds = e.EndUnixSeconds - clockAdmissionWindowSeconds
		if admitting && complete {
			if time.Unix(e.StartUnixSeconds, 0).Before(started) {
				result.Cause = "clock-select-admission"
			} else {
				selection, cause := p.selectBounds(ctx, e, p.policy)
				result.Cause = clockSelectionCause(cause)
				if refusal := leaseRefusal(result.Cause, e.Hosts); refusal != nil || result.Cause != "clock-lease-expired" {
					result.LeaseRefusal = refusal
				}
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
		delay := clockRetryPause
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
	var selected clocks.ClockSelection
	evidence, config, err := saveClockSelectionInput(r.c.Output, e, policy)
	if err != nil {
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
