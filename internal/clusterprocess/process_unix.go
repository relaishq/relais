//go:build !windows

package clusterprocess

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureCommand(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// wait owns reaping. Wait4 must be nonblocking under the same lock used by
// group signals: the PID stays reserved until that syscall reaps the leader.
// A leader may exit between poll and signal, but remains unreaped, so its PID
// cannot identify a different group. No group signal is allowed after reaping.
// These commands use files for stdout/stderr and no pipes or CommandContext;
// there are no exec.Cmd I/O goroutines to join. Release closes its OS handle.
func (c *Child) wait() {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(c.pid, &status, syscall.WNOHANG, nil)
		if pid == c.pid || (err != nil && !errors.Is(err, syscall.EINTR)) {
			c.reaped = true
			c.err = err
			if err == nil && !status.Exited() {
				c.err = fmt.Errorf("process killed by %s", status.Signal())
			}
			if err == nil && status.Exited() && status.ExitStatus() != 0 {
				c.err = fmt.Errorf("process exited with status %d", status.ExitStatus())
			}
			_ = c.cmd.Process.Release()
			close(c.done)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		<-ticker.C
	}
}
func (c *Child) SignalGroup(signal syscall.Signal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reaped {
		return os.ErrProcessDone
	}
	return syscall.Kill(-c.pid, signal)
}
