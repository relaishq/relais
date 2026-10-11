package callharness

import "time"

func timePointer(at time.Time) *time.Time { return &at }

func (o *stunObserver) observeMediaSend(packet []byte, actual time.Time) {
	// SRTP retains the RTP header. Exclude STUN, DTLS and RTCP, whose
	// payload-type range is reserved by RTP/RTCP multiplexing.
	if o.sendTiming == nil || len(packet) < 12 || packet[0]>>6 != 2 || (packet[1] >= 192 && packet[1] <= 223) {
		return
	}
	var scheduled *time.Time
	var kind string
	switch packet[1] & 0x7f {
	case opusPayloadType:
		kind, scheduled = kindAudio, o.audioSchedule.Load()
	case vp8PayloadType:
		kind, scheduled = kindVideo, o.videoSchedule.Load()
	}
	if scheduled != nil {
		o.sendTiming(kind, *scheduled, actual)
	}
}
