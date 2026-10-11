//go:build !darwin && !linux

package clusterprocess

import "errors"

func ephemeralPortRange() (portBand, error) {
	return portBand{}, errors.New("ephemeral port range unavailable on this OS")
}
