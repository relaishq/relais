// Package opuspacket validates Opus framing without decoding audio.
package opuspacket

import (
	"errors"
	"time"
)

var ErrInvalid = errors.New("invalid Opus packet")

// Duration validates framing and returns duration on the 48 kHz Opus clock.
func Duration(p []byte) (time.Duration, error) {
	invalid := func() (time.Duration, error) {
		return 0, ErrInvalid
	}
	if len(p) == 0 || len(p) > 65536 {
		return invalid()
	}
	config := p[0] >> 3
	var frame time.Duration
	switch {
	case config >= 16:
		frame = (2500 * time.Microsecond) << uint(config&3)
	case config >= 12:
		frame = (10 * time.Millisecond) << uint(config&1)
	default:
		frame = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond}[config&3]
	}
	count := 1
	data := p[1:]
	vbr := false
	switch p[0] & 3 {
	case 1:
		count = 2
	case 2:
		count = 2
		vbr = true
	case 3:
		if len(data) == 0 {
			return invalid()
		}
		flags := data[0]
		data = data[1:]
		count = int(flags & 63)
		vbr = flags&128 != 0
		if flags&64 != 0 {
			padding := 0
			for {
				if len(data) == 0 {
					return invalid()
				}
				v := int(data[0])
				data = data[1:]
				if v == 255 {
					padding += 254
				} else {
					padding += v
					break
				}
			}
			if padding > len(data) {
				return invalid()
			}
			data = data[:len(data)-padding]
		}
	}
	if count == 0 || time.Duration(count)*frame > 120*time.Millisecond {
		return invalid()
	}
	if vbr {
		sizes := 0
		for range count - 1 {
			if len(data) == 0 {
				return invalid()
			}
			n := int(data[0])
			data = data[1:]
			if n >= 252 {
				if len(data) == 0 {
					return invalid()
				}
				n += 4 * int(data[0])
				data = data[1:]
			}
			if n > 1275 {
				return invalid()
			}
			sizes += n
		}
		if sizes > len(data) || len(data)-sizes > 1275 {
			return invalid()
		}
	} else if len(data)%count != 0 || len(data)/count > 1275 {
		return invalid()
	}
	return time.Duration(count) * frame, nil
}
