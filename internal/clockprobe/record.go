package clockprobe

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
)

const LineBytes = 84

type Result uint8

const (
	Success Result = iota
	RequestFailed
	InvalidSample
	CapacityReached
)

// Record contains wall-clock nanoseconds, including invalid samples verbatim.
// Zero host timestamps mean that no usable response was available.
type Record struct {
	Target                                               Target
	FrontendSend, HostReceive, HostSend, FrontendReceive int64
	Result                                               Result
}

func (r Record) valid() bool {
	if r.FrontendSend < 0 || r.HostReceive < 0 || r.HostSend < 0 || r.FrontendReceive < 0 {
		return false
	}
	if r.Target == capacityTarget && r.Result == CapacityReached {
		return r.FrontendSend == 0 && r.HostReceive == 0 && r.HostSend == 0 && r.FrontendReceive == 0
	}
	return r.Target <= WorkerTwo && r.Result <= InvalidSample
}

func (r Record) MarshalLine() ([]byte, error) {
	if !r.valid() {
		return nil, fmt.Errorf("invalid clock probe record")
	}
	return fmt.Appendf(nil, "%d %019d %019d %019d %019d %d\n", r.Target, r.FrontendSend, r.HostReceive, r.HostSend, r.FrontendReceive, r.Result), nil
}

// ParseLine returns io.ErrUnexpectedEOF for an unfinished final line. A reader
// must leave its cursor there and wait for completion rather than count a loss.
func ParseLine(line []byte) (Record, error) {
	if len(line) < LineBytes && !bytes.ContainsRune(line, '\n') {
		return Record{}, io.ErrUnexpectedEOF
	}
	bad := func() (Record, error) { return Record{}, fmt.Errorf("invalid clock probe line") }
	if len(line) != LineBytes || line[83] != '\n' {
		return bad()
	}
	for _, i := range []int{1, 21, 41, 61, 81} {
		if line[i] != ' ' {
			return bad()
		}
	}
	if line[0] < '0' || line[0] > '3' || line[82] < '0' || line[82] > '3' {
		return bad()
	}
	values := [4]int64{}
	for i, start := range []int{2, 22, 42, 62} {
		for _, digit := range line[start : start+19] {
			if digit < '0' || digit > '9' {
				return bad()
			}
		}
		n, err := strconv.ParseInt(string(line[start:start+19]), 10, 64)
		if err != nil {
			return bad()
		}
		values[i] = n
	}
	r := Record{Target: Target(line[0] - '0'), FrontendSend: values[0], HostReceive: values[1], HostSend: values[2], FrontendReceive: values[3], Result: Result(line[82] - '0')}
	if !r.valid() {
		return bad()
	}
	return r, nil
}
