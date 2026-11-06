package operatorrehearsal

import (
	"fmt"
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/observe"
)

// Coverage contains no opaque identities. Preserve its fixed slot/cause labels
// and use a whole-second coverage origin without changing the legacy media
// origin. MediaTimeOffsetNS records the exact fractional difference.
func sanitizeCoverageTimes(e clocks.CoverageEvidence, anchor time.Time) clocks.CoverageEvidence {
	e = e.Public()
	if anchor.IsZero() {
		for _, a := range e.Attempts {
			if !a.At.IsZero() {
				anchor = a.At.Truncate(time.Second)
				break
			}
		}
	}
	if anchor.IsZero() {
		anchor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	e.MediaTimeOffsetNS = int64(anchor.Nanosecond())
	shift := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix() - anchor.Unix()
	ns := func(s string) string {
		n, ok := observe.CoverageNS(s)
		if !ok {
			return "unavailable"
		}
		n, ok = observe.AddSigned(n, shift*1e9)
		if !ok {
			return "unavailable"
		}
		return fmt.Sprintf("%019d", n)
	}
	for _, attempts := range [][]clocks.CoverageAttempt{e.Attempts, e.DrainAttempts} {
		for i := range attempts {
			a := &attempts[i]
			if !a.At.IsZero() {
				a.At = a.At.Add(time.Duration(shift) * time.Second)
			}
			for slot, t := range a.Clocks {
				for j := range t.Rows {
					t.Rows[j].UnixSeconds += shift
				}
				for j := range t.Breaks {
					t.Breaks[j].FromUnixSeconds += shift
					t.Breaks[j].UnixSeconds += shift
				}
				a.Clocks[slot] = t
			}
		}
	}
	for slot, b := range e.MissingIntervals {
		b.FromUnixSeconds += shift
		b.UnixSeconds += shift
		e.MissingIntervals[slot] = b
	}
	for i := range e.Windows {
		w := &e.Windows[i]
		w.StartUnixNS = ns(w.StartUnixNS)
		w.EndUnixNS = ns(w.EndUnixNS)
	}
	for i := range e.KillBrackets {
		b := &e.KillBrackets[i]
		b.MidpointUnixNS = ns(b.MidpointUnixNS)
		b.BeforeUnixNS = ns(b.BeforeUnixNS)
		b.AfterUnixNS = ns(b.AfterUnixNS)
	}
	e.Evaluate()
	return e
}
