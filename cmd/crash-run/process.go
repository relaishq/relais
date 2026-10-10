package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type ready struct {
	HTTP  string `json:"http"`
	Media string `json:"media"`
	Leg   string `json:"leg"`
	PID   int    `json:"pid"`
}
type child struct {
	cmd    *exec.Cmd
	pid    int
	mu     sync.Mutex
	reaped bool
	done   chan struct{}
	err    error
	log    *os.File
	path   string
}

func startChild(dir, name string, env []string, args ...string) (*child, error) {
	path := filepath.Join(dir, name+".log")
	log, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Env = log, log, env
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	c := &child{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{}), log: log, path: path}
	go c.wait()
	return c, nil
}

// wait owns reaping. Wait4 must be nonblocking under the same lock used by
// group signals: the PID stays reserved until that syscall reaps the leader.
// A leader may exit between poll and signal, but remains unreaped, so its PID
// cannot identify a different group. No group signal is allowed after reaping.
// These commands use files for stdout/stderr and no pipes or CommandContext;
// there are no exec.Cmd I/O goroutines to join. Release closes its OS handle.
func (c *child) wait() {
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
func (c *child) signalGroup(signal syscall.Signal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reaped {
		return os.ErrProcessDone
	}
	return syscall.Kill(-c.pid, signal)
}
func (c *child) stop() {
	defer func() { _ = c.log.Close() }()
	if errors.Is(c.signalGroup(syscall.SIGTERM), os.ErrProcessDone) {
		return
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
		_ = c.signalGroup(syscall.SIGKILL)
		<-c.done
	}
}
func (c *child) ready(ctx context.Context) (ready, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(c.path)
		if err != nil {
			return ready{}, err
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			var r ready
			if json.Unmarshal(line, &r) == nil && r.HTTP != "" && r.PID == c.pid {
				return r, nil
			}
		}
		select {
		case <-c.done:
			return ready{}, fmt.Errorf("process exited: %w; %s", c.err, data)
		case <-ctx.Done():
			return ready{}, fmt.Errorf("process readiness: %w; log %s", ctx.Err(), c.path)
		case <-ticker.C:
		}
	}
}
func freeTCP() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().String()
	err = listener.Close()
	return addr, err
}
func waitTCP(ctx context.Context, addr string, c *child) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errors.New("redis exited before listening")
		case <-ticker.C:
		}
	}
}
