package clusterprocess

import (
	"os/exec"
	"syscall"
)

func configureParentDeath(cmd *exec.Cmd) { cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL }
