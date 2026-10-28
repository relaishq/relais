package operatorcaller

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// CollectHost records allocated CPUs separately from the host topology. It is
// called in the public bridge, before launching a credential-free child.
func CollectHost(ctx context.Context) (Host, error) {
	if runtime.GOOS != "linux" {
		return Host{}, errors.New("caller-host-requires-linux")
	}
	command := exec.CommandContext(ctx, "nproc")
	command.Env = PublicEnvironment(nil)
	out, err := command.Output()
	if err != nil {
		return Host{}, err
	}
	available, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return Host{}, err
	}
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return Host{}, err
	}
	logical := 0
	model := ""
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "processor" {
			if logical >= maxSafeInteger {
				return Host{}, errors.New("caller-cpu-count-overflow")
			}
			logical++
		}
		if model == "" && (key == "model name" || key == "Hardware" || key == "Processor") {
			model = value
		}
	}
	host := Host{1, available, logical, model, 2, runtime.GOARCH, runtime.GOOS, time.Now()}
	return host, host.Validate()
}
