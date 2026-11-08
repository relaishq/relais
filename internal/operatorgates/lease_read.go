package operatorgates

import (
	"context"
	"encoding/json"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
)

// ReadLeases is unwired until 5b6. Batch sends share one request-send timestamp;
// serial sends get independent timestamps. Tracking/probe cursors remain owned
// by the session, and transport uncertainty retains the normal fatal latch.
func (s *Session) ReadLeases(ctx context.Context, p Peer, end time.Duration) (LeaseRead, error) {
	rows, sent := map[string]clocks.Sample{}, map[string]time.Duration{}
	s.mu.Lock()
	front, _ := inventory.ComponentSlot(s.inventoryComponents, "signal")
	control, _ := inventory.ComponentSlot(s.inventoryComponents, "control")
	path, probeCursor := s.clockProbeFile, s.clockProbeCursor
	s.mu.Unlock()
	readProbes := path != "" && s.clockProbeRefresh.TryLock()
	if readProbes {
		defer s.clockProbeRefresh.Unlock()
	}
	err := s.observeHosts(ctx, p, "clock", end, func(slot string) (string, error) {
		s.mu.Lock()
		cursor := s.clockCursors[slot]
		s.mu.Unlock()
		probe := ""
		if readProbes && slot == front {
			probe = path
		}
		return host.ClockWithLease(cursor, s.Budget.armed, probe, probeCursor), nil
	}, func(slot, stdout string) error {
		raw, err := host.Unpack(stdout, 256000)
		if err != nil {
			return err
		}
		rows[slot], err = clocks.Parse(raw)
		if err == nil && rows[slot].TrackingLog != nil {
			s.mu.Lock()
			if s.clockCursors == nil {
				s.clockCursors = map[string]json.RawMessage{}
			}
			s.clockCursors[slot] = rows[slot].TrackingLog.Cursor
			s.mu.Unlock()
		}
		return err
	}, func(slot string, at time.Duration) { sent[slot] = at })
	e := clocks.Evidence{Version: clocks.EvidenceVersion, At: s.clock.Wall(), Clocks: rows}
	s.mu.Lock()
	if path != "" {
		source := ""
		if readProbes {
			source = front
		}
		if next := s.clockProbeHistory.Collect(rows, source, rows[source].ProbeLog); len(next) > 0 {
			s.clockProbeCursor = next
		}
		e.Falsification = s.clockProbeHistory.Snapshot(front, map[string]string{"control": control, "w1": "w1", "w2": "w2"})
	}
	s.mu.Unlock()
	return LeaseRead{Evidence: e, Sent: sent}, err
}
