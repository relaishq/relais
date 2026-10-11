package processidentity

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strconv"
	"strings"
)

func inspect(pid int) (string, bool, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false, err
	}
	ticks, dead, err := parseStat(string(data))
	if err != nil {
		return "", false, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(string(boot)) == "" {
		return "", false, errors.New("empty Linux boot ID")
	}
	return strings.TrimSpace(string(boot)) + ":" + ticks, dead, nil
}
func parseStat(stat string) (string, bool, error) {
	// comm can contain spaces and parentheses; fields begin after its last ')'.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", false, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return "", false, fmt.Errorf("short process stat")
	}
	return fields[19], fields[0] == "Z" || fields[0] == "X", nil
}

// The pidfd pins the signal recipient. Its fdinfo Pid brackets /proc reads,
// refusing a namespace mismatch and recognizing a reaped pinned process.
func fencePlatform(ctx context.Context, id Identity) error {
	if id.PID <= 0 || id.Start == "" {
		return errors.New("invalid fencing target")
	}
	fd, err := unix.PidfdOpen(id.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	check := func() error {
		data, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
		if err != nil {
			return err
		}
		return checkPidfd(string(data), id.PID)
	}
	read := func(pid int) (string, bool, error) {
		if err := check(); err != nil {
			return "", false, err
		}
		start, dead, err := inspect(pid)
		if err != nil {
			return "", false, err
		}
		if err := check(); err != nil {
			return "", false, err
		}
		return start, dead, nil
	}
	return fence(ctx, id, read, func(pid int) error {
		if err := sameExecutable(fmt.Sprintf("/proc/%d/exe", pid), "/proc/self/exe"); err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	})
}
func checkPidfd(info string, pid int) error {
	for _, line := range strings.Split(info, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Pid:" {
			pinned, err := strconv.Atoi(fields[1])
			if err != nil {
				return err
			}
			if pinned == -1 {
				return unix.ESRCH
			}
			if pinned != pid {
				return errors.New("pidfd and /proc PID namespaces differ")
			}
			return nil
		}
	}
	return errors.New("pidfd has no process identity")
}
