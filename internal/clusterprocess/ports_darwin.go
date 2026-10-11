package clusterprocess

import "golang.org/x/sys/unix"

func ephemeralPortRange() (portBand, error) {
	first, err := unix.SysctlUint32("net.inet.ip.portrange.first")
	if err != nil {
		return portBand{}, err
	}
	last, err := unix.SysctlUint32("net.inet.ip.portrange.last")
	if err != nil {
		return portBand{}, err
	}
	return portBand{int(first), int(last)}, nil
}
