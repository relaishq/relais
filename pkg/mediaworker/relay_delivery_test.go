package mediaworker

import (
	"context"
	"net/netip"
	"testing"

	"github.com/pion/rtcp"
	"github.com/stretchr/testify/require"
)

// C counts only fresh receiver reports on the current path that cover RTP
// sent after the switch (ADR 0016 section 7).
func TestRelayDeliveryNeedsFreshReportsOnNewPath(t *testing.T) {
	oldLeg, newLeg := netip.MustParseAddrPort("127.0.0.1:1001"), netip.MustParseAddrPort("127.0.0.1:1002")
	caller := netip.MustParseAddrPort("192.0.2.7:5000")
	s := &session{}
	s.state.Audio = trackState{MID: "0", SSRC: 111, Packets: 10, HighestSentIndex: 3<<16 | 65530}
	s.state.Video = trackState{MID: "1", SSRC: 222, Packets: 5, HighestSentIndex: 100}
	s.state.ICE.Relay = &RelaySlot{ID: "r2", Leg: newLeg}
	s.state.ICE.RemoteAddr = caller
	require.Equal(t, []string{"0", "1"}, s.requiredMIDsLocked())
	s.relayDelivery = s.newRelayDeliveryLocked(4, []string{"0", "1"})
	current := relayPath{newLeg, caller}
	rr := func(ssrc, seq uint32) []rtcp.Packet {
		return []rtcp.Packet{&rtcp.ReceiverReport{SSRC: 9, Reports: []rtcp.ReceptionReport{{SSRC: ssrc, LastSequenceNumber: seq}}}}
	}
	delivered := func() map[string]bool { return s.relayDelivery.delivered }

	s.noteDeliveryLocked(rr(111, 3<<16|65535), relayPath{oldLeg, caller})
	s.noteDeliveryLocked(rr(111, 3<<16|65535), relayPath{newLeg, netip.MustParseAddrPort("192.0.2.7:5001")})
	require.Empty(t, delivered(), "reports from the previous path or another caller address never count")
	s.noteDeliveryLocked(rr(111, 3<<16|65530), current)
	s.noteDeliveryLocked([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 9}}, current)
	s.noteDeliveryLocked(rr(333, 1000), current)
	require.Empty(t, delivered(), "an old, empty or foreign report never counts")

	s.noteDeliveryLocked(rr(111, 4<<16|2), current) // across the sequence wrap
	require.Equal(t, map[string]bool{"0": true}, delivered(), "audio alone does not deliver video")
	s.noteDeliveryLocked([]rtcp.Packet{&rtcp.SenderReport{SSRC: 9, Reports: []rtcp.ReceptionReport{{SSRC: 222, LastSequenceNumber: 101}}}}, current)
	require.Equal(t, map[string]bool{"0": true, "1": true}, delivered())

	// Extended sequence numbers: a stale previous-cycle report with a higher
	// low half is not fresh, and a long forward run (beyond 2^15) still is.
	s.state.Video.HighestSentIndex = 4<<16 | 100
	s.relayDelivery = s.newRelayDeliveryLocked(5, []string{"1"})
	s.noteDeliveryLocked(rr(222, 3<<16|200), current)
	require.Empty(t, delivered(), "a previous-cycle report is stale")
	s.noteDeliveryLocked(rr(222, 4<<16|100+40000), current)
	require.Equal(t, map[string]bool{"1": true}, delivered(), "a large forward distance is fresh")

	// A new generation starts with nothing delivered.
	s.state.Video.HighestSentIndex = 400
	s.relayDelivery = s.newRelayDeliveryLocked(6, []string{"1"})
	s.noteDeliveryLocked(rr(222, 300), current)
	s.noteDeliveryLocked(rr(111, 5<<16), current)
	require.Empty(t, delivered(), "evidence from an earlier generation or an unrequired track never carries over")
}

func TestRequiredMIDsSkipSilentTracks(t *testing.T) {
	s := &session{}
	s.state.Audio = trackState{MID: "0", Packets: 3}
	s.state.Video = trackState{MID: "1"}
	require.Equal(t, []string{"0"}, s.requiredMIDsLocked())
}

// After a resume the worker has no C state for the live generation. The
// first query fixes fresh boundaries; a stale report from before the resume
// never counts, and a later fresh one does.
func TestRelayDeliveryAfterResumeNeedsFreshReport(t *testing.T) {
	leg, caller := netip.MustParseAddrPort("127.0.0.1:1002"), netip.MustParseAddrPort("192.0.2.7:5000")
	s := &session{id: "call"}
	s.state.ICE.LocalUfrag = "call+3"
	s.state.ICE.Relay = &RelaySlot{ID: "r2", Leg: leg}
	s.state.ICE.RemoteAddr = caller
	s.state.Audio = trackState{MID: "0", SSRC: 111, Packets: 50, HighestSentIndex: 2<<16 | 500}
	w := &Worker{cfg: Config{Relay: &RelayConfig{}}, sessions: map[string]*session{"call": s}}
	report, err := w.RelayDelivery(context.Background(), "call", 3)
	require.NoError(t, err)
	require.True(t, report.Switched)
	require.Equal(t, []string{"0"}, report.Required)
	require.Empty(t, report.Delivered)
	current := relayPath{leg, caller}
	s.noteDeliveryLocked([]rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 111, LastSequenceNumber: 2<<16 | 400}}}}, current)
	report, err = w.RelayDelivery(context.Background(), "call", 3)
	require.NoError(t, err)
	require.Empty(t, report.Delivered, "a report from before the resume boundary is stale")
	s.noteDeliveryLocked([]rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 111, LastSequenceNumber: 2<<16 | 501}}}}, current)
	report, err = w.RelayDelivery(context.Background(), "call", 3)
	require.NoError(t, err)
	require.Equal(t, []string{"0"}, report.Delivered)
}
