package voice

import (
	"errors"
	"time"
)

// duration reads the Opus TOC. It does not decode or claim full codec framing
// validation. The media host independently validates emitted packets.
func packetDuration(p []byte) (time.Duration, error) {
	if len(p) == 0 {
		return 0, errors.New("empty Opus packet")
	}
	config := p[0] >> 3
	var us int
	switch {
	case config >= 16:
		us = 2500 << (config & 3)
	case config >= 12:
		us = 10000 << (config & 1)
	default:
		us = []int{10000, 20000, 40000, 60000}[config&3]
	}
	frames := 1
	switch p[0] & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 {
			return 0, errors.New("short Opus count")
		}
		frames = int(p[1] & 63)
	}
	if frames == 0 || frames*us > 120000 {
		return 0, errors.New("invalid Opus duration")
	}
	return time.Duration(frames*us) * time.Microsecond, nil
}
