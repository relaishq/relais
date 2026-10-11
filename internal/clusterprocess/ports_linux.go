package clusterprocess

import (
	"fmt"
	"os"
)

func ephemeralPortRange() (portBand, error) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return portBand{}, err
	}
	var ephemeral portBand
	_, err = fmt.Sscanf(string(data), "%d %d", &ephemeral.first, &ephemeral.last)
	return ephemeral, err
}
