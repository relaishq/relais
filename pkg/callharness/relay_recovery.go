package callharness

import (
	"slices"
	"time"
)

// RelayRecoveryGap measures last caller media before loss to decoded caller video after observed commit.
// Audio RTP must also advance after the observation. This upper-bounds the first decoded frame after the actual worker switch and
// cannot count an old-slot frame still in flight at loss as recovery.
func (c *Call) RelayRecoveryGap(fault, observedCommit time.Time) time.Duration {
	last, first, audio := c.RelayRecoveryPoints(fault, observedCommit)
	if first == 0 || last == 0 || audio == 0 {
		return 0
	}
	return max(first, audio) - last
}

// RelayRecoveryPoints returns the caller-relative last media before fault,
// first fresh decoded video and first advancing audio after observedCommit.
// Missing observations are zero, including when only one track recovered.
func (c *Call) RelayRecoveryPoints(fault, observedCommit time.Time) (last, video, audio time.Duration) {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	return c.rec.relayRecoveryPoints(fault.Sub(c.rec.start), observedCommit.Sub(c.rec.start), time.Duration(1<<63-1))
}

// Caller holds r.mu. until bounds the opt-in relay-loss observation window.
func (r *recorder) relayRecoveryPoints(at, observedCommit, until time.Duration) (last, video, audio time.Duration) {
	for _, track := range r.tracks {
		for _, arrival := range track.arrivals {
			if arrival > until {
				continue
			}
			if arrival < at {
				last = max(last, arrival)
			}
			if track.kind == kindAudio && arrival >= observedCommit && audio == 0 {
				audio = arrival
			}
		}
		if track.video != nil {
			for _, frame := range track.video.frames {
				if frame.at <= until && frame.at >= observedCommit && frame.decodable && !frame.repeated {
					video = frame.at
					break
				}
			}
		}
	}
	return last, video, audio
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
