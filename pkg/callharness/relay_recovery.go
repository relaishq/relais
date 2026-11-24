package callharness

import (
	"slices"
	"time"
)

// RelayRecoveryGap measures last caller media before loss to decoded caller video after observed commit.
// Audio RTP must also advance after the observation. This upper-bounds the first decoded frame after the actual worker switch and
// cannot count an old-slot frame still in flight at loss as recovery.
func (c *Call) RelayRecoveryGap(fault, observedCommit time.Time) time.Duration {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	at := fault.Sub(c.rec.start)
	var last, first, audio time.Duration
	for _, track := range c.rec.tracks {
		for _, arrival := range track.arrivals {
			if arrival < at {
				last = max(last, arrival)
			}
			if track.kind == kindAudio && arrival >= observedCommit.Sub(c.rec.start) && audio == 0 {
				audio = arrival
			}
		}
		if track.video != nil {
			for _, frame := range track.video.frames {
				if frame.at >= observedCommit.Sub(c.rec.start) && frame.decodable && !frame.repeated {
					first = frame.at
					break
				}
			}
		}
	}
	if first == 0 || last == 0 || audio == 0 {
		return 0
	}
	return max(first, audio) - last
}

// RelaySnapshot is a bounded caller observation; it does not change completion.
func (c *Call) RelaySnapshot() *Report { return c.rec.report() }

// RelayDTLSStates reports transport continuity across ICE restarts. ICE can
// return to checking during a planned change without reconnecting DTLS.
func (c *Call) RelayDTLSStates() []StateChange {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()
	return slices.Clone(c.dtlsStates)
}
