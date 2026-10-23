package metrics

import (
	"strconv"
)

// redisCommandCounter parses only RESP array and bulk lengths. It never retains
// command arguments, audio, or keys. go-redis v9 writes RESP2 commands as bulk arrays.
// Counts begin when a full top-level array header reaches the socket, including
// retries and commands whose later arguments or reply are lost.
type redisCommandCounter struct {
	phase   byte
	line    []byte
	args    int64
	skip    int64
	invalid bool
}

func (r *redisCommandCounter) feed(data []byte) uint64 {
	var commands uint64
	for _, b := range data {
		if r.invalid {
			return commands
		}
		if r.skip > 0 {
			r.skip--
			if r.skip == 0 {
				r.args--
				if r.args == 0 {
					r.phase = 0
				} else {
					r.phase = '$'
				}
			}
			continue
		}
		if r.phase == 0 {
			if b != '*' {
				r.invalid = true
				return commands
			}
			r.phase = '*'
			r.line = r.line[:0]
			continue
		}
		if r.phase == '$' {
			if b != '$' {
				r.invalid = true
				return commands
			}
			r.phase = 'b'
			r.line = r.line[:0]
			continue
		}
		if b != '\n' {
			if len(r.line) >= 22 {
				r.invalid = true
				return commands
			}
			r.line = append(r.line, b)
			continue
		}
		if len(r.line) == 0 || r.line[len(r.line)-1] != '\r' {
			r.invalid = true
			return commands
		}
		n, err := strconv.ParseInt(string(r.line[:len(r.line)-1]), 10, 64)
		if err != nil || n < 0 {
			r.invalid = true
			return commands
		}
		if r.phase == '*' {
			if n == 0 {
				r.invalid = true
				return commands
			}
			r.args = n
			r.phase = '$'
			commands++
		} else {
			if n > int64(^uint64(0)>>1)-2 {
				r.invalid = true
				return commands
			}
			r.skip = n + 2
		}
		r.line = r.line[:0]
	}
	return commands
}
