package operatortransport

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
)

type Subprocess struct {
	*Pipe
	cmd *exec.Cmd
}

// Start uses structured argv and a fixed environment. It never invokes a shell
// or forwards the driver's credentials to an adapter executable.
func Start(ctx context.Context, executable string, args []string) (*Subprocess, error) {
	if !filepath.IsAbs(executable) {
		return nil, errors.New("adapter-path-not-absolute")
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		return nil, errors.New("adapter-start-failed")
	}
	return &Subprocess{Pipe: NewPipe(out, in), cmd: cmd}, nil
}
func (s *Subprocess) Wait() error { return s.cmd.Wait() }
