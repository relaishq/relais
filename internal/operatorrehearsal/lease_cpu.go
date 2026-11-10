package operatorrehearsal

import (
	"fmt"
	"regexp"
)

const leaseCPUOutputReserve = 128

// The shell times builtin emits self user/system, then children user/system.
// Accept only its bounded numeric trailer; never log arbitrary host stderr.
var leaseCPUTrailer = regexp.MustCompile(`\nclock-lease-cpu\n([0-9]{1,6}m[0-9]{1,2}\.[0-9]{1,6}s) ([0-9]{1,6}m[0-9]{1,2}\.[0-9]{1,6}s)\n([0-9]{1,6}m[0-9]{1,2}\.[0-9]{1,6}s) ([0-9]{1,6}m[0-9]{1,2}\.[0-9]{1,6}s)\n$`)

func splitLeaseCPU(stderr []byte) ([]byte, string) {
	match := leaseCPUTrailer.FindSubmatch(stderr)
	if match == nil || len(match[0]) > leaseCPUOutputReserve {
		return stderr, "cpu=unavailable"
	}
	return stderr[:len(stderr)-len(match[0])], fmt.Sprintf("cpuSelfUser=%s cpuSelfSys=%s cpuChildrenUser=%s cpuChildrenSys=%s", match[1], match[2], match[3], match[4])
}
