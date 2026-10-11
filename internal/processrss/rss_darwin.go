//go:build darwin && cgo

package processrss

/*
#include <libproc.h>
#include <sys/resource.h>
#include <stdint.h>

// macOS libproc returns current resident bytes, including for owned children.
// https://github.com/apple-oss-distributions/xnu/blob/main/libsyscall/wrappers/libproc/libproc.h
static int owned_process_rss(int pid, uint64_t *rss) {
 struct rusage_info_v2 usage = {0};
 int result = proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)&usage);
 if (result == 0) *rss = usage.ri_resident_size;
 return result;
}
*/
import "C"

import "fmt"

func current(pid int) (uint64, error) {
	var rss C.uint64_t
	result, err := C.owned_process_rss(C.int(pid), &rss)
	if result != 0 {
		return 0, fmt.Errorf("current RSS for PID %d: %w", pid, err)
	}
	return uint64(rss), nil
}
