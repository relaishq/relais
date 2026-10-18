//go:build !linux

package clusterprocess

import "os/exec"

func configureParentDeath(*exec.Cmd) {}
