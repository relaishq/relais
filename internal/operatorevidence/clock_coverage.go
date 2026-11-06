package operatorevidence

import (
	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// CoverageWindows preserves each receipt's timestamps; an aggregate EventResult
// alone can omit intermediate completion times. Indexes match undecided rows.
func CoverageWindows(pool Pool) []observe.CoverageWindow {
	out := []observe.CoverageWindow{}
	for phase, p := range pool.Phases {
		for event, e := range p.Events {
			if len(e.Identities) == 0 {
				out = append(out, observe.CoverageWindow{Phase: phase, Event: event, Kind: e.Event.Kind})
				continue
			}
			for _, id := range e.Identities {
				matched := false
				for _, r := range p.Receipts {
					if r.Identity != id {
						continue
					}
					matched = true
					out = append(out, observe.CoverageWindow{Phase: phase, Event: event, Kind: r.Kind, Worker: r.Identity.Worker, StartUnixNS: clocks.TimestampNS(r.Start.UnixNano()), EndUnixNS: clocks.TimestampNS(r.End.UnixNano())})
				}
				if !matched {
					out = append(out, observe.CoverageWindow{Phase: phase, Event: event, Kind: e.Event.Kind, Worker: id.Worker})
				}
			}
		}
	}
	return out
}
