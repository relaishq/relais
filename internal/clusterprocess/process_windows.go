package clusterprocess

import (
	"errors"
	"os/exec"
	"syscall"
)

var errProcessDriverUnsupported = errors.New("the process driver requires Unix process groups")

func configureCommand(*exec.Cmd) error            { return errProcessDriverUnsupported }
func (c *Child) wait()                            {} // Start refuses before a child exists.
func (c *Child) SignalGroup(syscall.Signal) error { return errProcessDriverUnsupported }
