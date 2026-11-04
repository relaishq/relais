package mediaworker

import (
	"context"
	"slices"

	"github.com/pion/rtcp"
	"github.com/relaishq/relais/internal/iceufrag"
	"github.com/relaishq/relais/pkg/sessionstore"
)

// RelayDeliveryReport is the worker's fenced per-track C evidence (ADR 0016
// section 7): the caller's receiver reports, arriving through the current
// relay path, cover RTP sent only on that path. Control matches Generation,
// Slot and Epoch against the store before retaining Delivered.
type RelayDeliveryReport struct {
	Generation uint64   `json:"generation"`
	Slot       string   `json:"slot"`
	Epoch      uint64   `json:"epoch"`
	Switched   bool     `json:"switched"`
	Required   []string `json:"required"`
	Delivered  []string `json:"delivered,omitempty"`
}

// relayDelivery holds C for one switched generation. boundary is the highest
// extended index each required track sent before the switch (or before resume,
// for a generation this worker did not switch). An empty or older report, a
// report on the previous path, or a report for another SSRC never counts.
type relayDelivery struct {
	generation uint64
	boundary   map[uint32]uint32 // outbound SSRC -> last extended sequence sent on the old path
	mids       map[uint32]string
	delivered  map[string]bool
}

// requiredMIDsLocked returns the negotiated tracks on which the worker is
// sending media now. A track that never sent is not required: until the
// worker can inject confirmation media, it cannot promise a silent track.
func (s *session) requiredMIDsLocked() []string {
	var mids []string
	for _, t := range []*trackState{&s.state.Audio, &s.state.Video} {
		if t.negotiated() && t.Packets > 0 {
			mids = append(mids, t.MID)
		}
	}
	return mids
}

// newRelayDeliveryLocked fixes the per-track sequence boundaries now. It runs
// at the switch, before any media is sent on the new path, and after a resume.
func (s *session) newRelayDeliveryLocked(generation uint64, required []string) *relayDelivery {
	d := &relayDelivery{generation: generation, boundary: map[uint32]uint32{}, mids: map[uint32]string{}, delivered: map[string]bool{}}
	for _, t := range []*trackState{&s.state.Audio, &s.state.Video} {
		if t.negotiated() && slices.Contains(required, t.MID) {
			// RTCP's extended highest sequence is cycles<<16|seq in 32 bits,
			// the low 32 bits of the SRTP index the worker tracks.
			d.boundary[t.SSRC] = uint32(t.HighestSentIndex)
			d.mids[t.SSRC] = t.MID
		}
	}
	return d
}

// noteDeliveryLocked runs under mu for authenticated SRTCP that arrived on the
// current path. It compares the full 32-bit extended highest sequence with
// serial arithmetic, so an older cycle is stale and a long forward run is not.
func (s *session) noteDeliveryLocked(packets []rtcp.Packet, path relayPath) {
	d := s.relayDelivery
	if d == nil || !path.leg.IsValid() || path.leg != s.currentLeg() || path.caller != s.state.ICE.RemoteAddr {
		return
	}
	for _, packet := range packets {
		var reports []rtcp.ReceptionReport
		switch p := packet.(type) {
		case *rtcp.ReceiverReport:
			reports = p.Reports
		case *rtcp.SenderReport:
			reports = p.Reports
		}
		for _, r := range reports {
			mid, ok := d.mids[r.SSRC]
			if ok && int32(r.LastSequenceNumber-d.boundary[r.SSRC]) > 0 {
				d.delivered[mid] = true
			}
		}
	}
}

// RelayDelivery reports C for generation. For the pending generation it only
// reports the required set, which control fixes before the event. For the live
// generation without evidence on this worker (a resume), it starts fresh
// boundaries now, so only new receiver reports can count.
func (w *Worker) RelayDelivery(ctx context.Context, id string, generation uint64) (RelayDeliveryReport, error) {
	if err := ctx.Err(); err != nil {
		return RelayDeliveryReport{}, err
	}
	if w.cfg.Relay == nil {
		return RelayDeliveryReport{}, ErrRelayUnsupported
	}
	s := w.session(id)
	if s == nil {
		return RelayDeliveryReport{}, ErrUnknownSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fenced.Load() {
		return RelayDeliveryReport{}, sessionstore.ErrLeaseLost
	}
	_, live, _ := iceufrag.Parse(s.state.ICE.LocalUfrag)
	report := RelayDeliveryReport{Generation: generation, Epoch: s.lease.Epoch}
	if s.state.ICE.Relay != nil {
		report.Slot = s.state.ICE.Relay.ID
	}
	switch {
	case generation > live:
		if p := s.pendingRestart; p != nil && p.ice.Relay != nil && generation == p.from.Generation+1 {
			report.Slot = p.ice.Relay.ID
			report.Required = slices.Clone(p.required)
			return report, nil
		}
		report.Required = s.requiredMIDsLocked()
		return report, nil
	case generation < live:
		return RelayDeliveryReport{}, sessionstore.ErrRelayConflict
	}
	if s.relayDelivery == nil || s.relayDelivery.generation != live {
		s.relayDelivery = s.newRelayDeliveryLocked(live, s.requiredMIDsLocked())
	}
	d := s.relayDelivery
	report.Switched = true
	for _, mid := range d.mids {
		report.Required = append(report.Required, mid)
		if d.delivered[mid] {
			report.Delivered = append(report.Delivered, mid)
		}
	}
	slices.Sort(report.Required)
	slices.Sort(report.Delivered)
	return report, nil
}
