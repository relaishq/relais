package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// kernelUDPDrops is a diagnostic, never a gate. It is the change in the
// kernel's UDP receive-buffer drop counters (Linux /proc/net/snmp "Udp:"
// RcvbufErrors, and /proc/net/snmp6 Udp6RcvbufErrors when present) between
// soak start and end. The counters belong to the driver's network namespace:
// every loopback role, but not the separate namespaces of netns roles.
type kernelUDPDrops struct {
	Available        bool              `json:"available"`
	Reason           string            `json:"reason,omitempty"`
	Scope            string            `json:"scope,omitempty"`
	Start            kernelUDPCounters `json:"start"`
	End              kernelUDPCounters `json:"end"`
	RcvbufErrors     uint64            `json:"udp_rcvbuf_errors"`
	UDP6             bool              `json:"udp6"`
	UDP6RcvbufErrors uint64            `json:"udp6_rcvbuf_errors"`
}

type kernelUDPCounters struct {
	RcvbufErrors     uint64 `json:"udp_rcvbuf_errors"`
	UDP6             bool   `json:"udp6"`
	UDP6RcvbufErrors uint64 `json:"udp6_rcvbuf_errors"`
}

var errKernelUDPUnavailable = errors.New("kernel UDP counters are read only on Linux")

func sampleKernelUDP() (kernelUDPCounters, error) {
	if runtime.GOOS != "linux" {
		return kernelUDPCounters{}, errKernelUDPUnavailable
	}
	return readKernelUDP("/proc/net")
}

// readKernelUDP reads dir/snmp and, if present, dir/snmp6.
func readKernelUDP(dir string) (kernelUDPCounters, error) {
	var c kernelUDPCounters
	data, err := os.ReadFile(filepath.Join(dir, "snmp"))
	if err != nil {
		return c, err
	}
	if c.RcvbufErrors, err = parseSNMPUDP(string(data)); err != nil {
		return c, err
	}
	data, err = os.ReadFile(filepath.Join(dir, "snmp6"))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.UDP6RcvbufErrors, c.UDP6, err = parseSNMP6UDP(string(data))
	return c, err
}

// parseSNMPUDP reads the "Udp:" header/value line pair. "UdpLite:" differs.
func parseSNMPUDP(data string) (uint64, error) {
	var header []string
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Udp:" {
			continue
		}
		if header == nil {
			header = fields
			continue
		}
		if len(fields) != len(header) {
			return 0, errors.New("snmp Udp: counters malformed")
		}
		for i, name := range header {
			if name == "RcvbufErrors" {
				return strconv.ParseUint(fields[i], 10, 64)
			}
		}
		return 0, errors.New("snmp Udp: RcvbufErrors missing")
	}
	return 0, errors.New("snmp Udp: counters missing")
}

// parseSNMP6UDP reads the "Udp6RcvbufErrors <n>" line; absent means unknown.
func parseSNMP6UDP(data string) (uint64, bool, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Udp6RcvbufErrors" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			return value, err == nil, err
		}
	}
	return 0, false, nil
}

func kernelUDPDelta(goos, topology string, start, end kernelUDPCounters, startErr, endErr error) kernelUDPDrops {
	d := kernelUDPDrops{Start: start, End: end}
	switch {
	case goos != "linux":
		d.Reason = "not available on " + goos
	case startErr != nil || endErr != nil:
		d.Reason = "not available: " + errors.Join(startErr, endErr).Error()
	case end.RcvbufErrors < start.RcvbufErrors:
		d.Reason = "not available: Udp RcvbufErrors decreased"
	default:
		d.Available = true
		d.Scope = "driver network namespace (every loopback role)"
		if topology == "netns" {
			d.Scope = "driver network namespace only; netns roles are not included"
		}
		d.RcvbufErrors = end.RcvbufErrors - start.RcvbufErrors
		if start.UDP6 && end.UDP6 && end.UDP6RcvbufErrors >= start.UDP6RcvbufErrors {
			d.UDP6 = true
			d.UDP6RcvbufErrors = end.UDP6RcvbufErrors - start.UDP6RcvbufErrors
		}
	}
	return d
}
