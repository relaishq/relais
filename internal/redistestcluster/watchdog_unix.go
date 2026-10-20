//go:build !windows

package redistestcluster

import (
	"os/exec"
	"syscall"
)

// The guardian must survive a signal to its owning test's process group.
func configureWatchdog(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
