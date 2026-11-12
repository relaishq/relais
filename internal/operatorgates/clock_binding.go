package operatorgates

import (
	"context"
	"encoding/json"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// BindClock owns both historical verification and the cursor used by live reads.
// Read boundaries are taken now; the original armed/end times never move.
func (s *Session) BindClock(ctx context.Context, p Peer, c ResourceConfig, end time.Duration) error {
	refuse := func(cause string) error { return &clocks.BindingError{Cause: cause} }
	if s.scenario.ClockReceiptSHA256 == "" || len(s.clockArtifacts.ReceiptGZIP) == 0 || len(s.clockArtifacts.EvidenceGZIP) == 0 {
		return refuse("clock-binding-missing")
	}
	raw, evidence, err := s.clockArtifacts.Unpack()
	if err != nil {
		return refuse("clock-binding-invalid")
	}
	var receipt clocks.SelectionReceipt
	var input clocks.SelectionEvidence
	if observe.DecodeClosedLimit(raw, &receipt, nil, operator.ClockArtifactLimit) != nil || observe.DecodeClosedLimit(evidence, &input, nil, operator.ClockArtifactLimit) != nil {
		return refuse("clock-binding-invalid")
	}
	selected, err := clocks.Select(input, c.ClockPolicy)
	if err != nil {
		return refuse("clock-binding-selection-differ")
	}
	identity, err := operator.BuildScenarioIdentity(s.scenario, selected.SHA256)
	if err != nil || identity.Digest != s.scenario.Digest || selected.SHA256 != s.scenario.ClockReceiptSHA256 {
		return refuse("clock-binding-digest-differ")
	}
	caller := clocks.Bounds{FrontendControlBoundNS: c.Endpoints.FrontendControlBoundNS, FrontendWorkerBoundNS: c.Endpoints.FrontendWorkerBoundNS, DriftPPM: float64(c.Endpoints.DriftPPM)}
	if _, err = s.FreshInventory(ctx, p, c.Pins, end, "", false); err != nil {
		return err
	}
	front, _ := inventory.ComponentSlot(s.inventoryComponents, "signal")
	control, _ := inventory.ComponentSlot(s.inventoryComponents, "control")
	in := clocks.ReceiptBindingInput{ReceiptJSON: raw, EvidenceJSON: evidence, ReceiptSHA256: s.scenario.ClockReceiptSHA256, EvidenceSHA256: receipt.EvidenceSHA256, RunID: c.Endpoints.RunID, FrontendSlot: front, ControlSlot: control, CallerLeapMode: c.ClockLeapMode, ConfigBounds: c.Bounds, CallerBounds: caller, ExpectedPolicy: c.ClockPolicy}
	cursors := map[string]json.RawMessage{}
	historical := clocks.Evidence{Clocks: map[string]clocks.Sample{}}
	for _, h := range input.Hosts {
		// A cursorless tail may omit rows under wire pressure. Use it only to find
		// EOF. The independent precision pager verifies the frozen interval below.
		var sample clocks.Sample
		for attempt := 0; attempt < 64; attempt++ {
			boundary := s.clock.Wall().UnixMilli()
			err = s.observeHost(ctx, p, h.Slot, "clock", host.ClockWithLease(cursors[h.Slot], boundary, "", nil), end, func(stdout string) error {
				data, e := host.Unpack(stdout, 256000)
				if e != nil {
					return e
				}
				sample, e = clocks.Parse(data)
				return e
			})
			if err != nil {
				return err
			}
			if sample.TrackingLog == nil || len(sample.TrackingLog.Cursor) == 0 {
				return refuse("clock-history-incomplete")
			}
			cursors[h.Slot] = sample.TrackingLog.Cursor
			if sample.TrackingLog.ReadComplete {
				break
			}
			if attempt == 63 {
				return refuse("clock-history-limit")
			}
		}
		boundary := s.clock.Wall().UnixMilli()
		first, last := clocks.FrozenTrackingRange(h.Tracking.Rows, input.StartUnixSeconds, input.EndUnixSeconds)
		log, e := clocks.ReadHistoricalTracking(ctx, boundary, first, last, func(ctx context.Context, script string) (stdout string, e error) {
			e = s.observeHost(ctx, p, h.Slot, "clock", script, end, func(out string) error { stdout = out; return nil })
			return
		})
		if e != nil {
			return e
		}
		historical.Clocks[h.Slot] = clocks.Sample{TrackingLog: &log}
		in.BeforeRun = append(in.BeforeRun, clocks.BindingHost{Slot: h.Slot, LeapMode: c.ClockLeapMode, Tracking: log})
	}
	binding, err := clocks.BindReceipt(in)
	if err != nil {
		return err
	}
	proof, err := (clocks.CoverageEvidence{FrontendSlot: front, ControlSlot: control}).WithBinding(binding, receipt)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.clockBinding = proof.Binding
	s.clockCursors = cursors
	s.clockProbeFile = c.Parameters.ClockProbeFile
	s.clockProbeHistory.Collect(historical.Clocks, "", nil)
	s.mu.Unlock()
	return nil
}

func (s *Session) BoundLeaseGuard(p Peer, pause func(context.Context, time.Duration) error) (*LeaseGuard, error) {
	if s.clockBinding == nil {
		return nil, &clocks.BindingError{Cause: "clock-binding-missing"}
	}
	b := s.clockBinding
	proof := clocks.CoverageEvidence{FrontendSlot: b.Receipt.FrontendSlot, ControlSlot: b.Receipt.ControlSlot, Bounds: b.Binding.Bounds, ClockReceiptSHA256: b.Binding.ReceiptSHA256, Binding: b}
	return NewLeaseGuard(proof, s, func(ctx context.Context, end time.Duration) (LeaseRead, error) {
		read, err := s.ReadLeases(ctx, p, end)
		s.retainClockAttempt(read.Evidence)
		return read, err
	}, pause)
}

func (g *LeaseGuard) Snapshot() (clocks.Evidence, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rows := map[string]clocks.Sample{}
	values := map[string]int64{}
	at := g.clock.Elapsed()
	for slot, l := range g.leases {
		v, err := l.At(at)
		if err != nil {
			return clocks.Evidence{}, err
		}
		if v.Expired {
			return clocks.Evidence{}, fault("clock-lease-expired")
		}
		sample := g.samples[slot]
		sample.RecordedLease = &v
		rows[slot] = sample
		values[slot] = v.BoundNS
	}
	b := g.bounds
	b.FrontendWorkerBoundNS = g.workerBound
	e, err := clocks.Check(rows, b, g.frontend, g.control)
	e.Control = values[g.frontend] + values[g.control]
	e.Worker = values[g.frontend] + max(values["w1"], values["w2"])
	e.At = g.clock.Wall()
	return e, err
}
