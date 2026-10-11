package processrss

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func read(pid int) (Sample, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return Sample{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return Sample{}, fmt.Errorf("missing resident pages")
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	return Sample{ResidentBytes: pages * uint64(os.Getpagesize())}, err
}
