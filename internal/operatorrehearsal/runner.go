package operatorrehearsal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

type Command struct {
	Label string
	Args  []string
	Input []byte
}
type CommandResult struct {
	Code           *int
	Stdout, Stderr []byte
	Truncated      bool
	Err            error
}
type Runner interface {
	Run(context.Context, Command) CommandResult
}
type LocalRunner struct{}

// Package transaction output is much larger than a host protocol response.
// Bound image capture separately without changing the host evidence budget.
const imageBuildOutputBytes = 4 << 20

func commandOutputLimit(label string) int {
	if label == "image" {
		return imageBuildOutputBytes
	}
	return operator.MaxHostOutputBytes
}

type boundedOutput struct {
	limit     int
	data      []byte
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.truncated {
		return n, nil
	}
	limit := b.limit
	if limit == 0 {
		limit = operator.MaxHostOutputBytes
	}
	remaining := limit - len(b.data)
	if n > remaining {
		b.truncated = true
		b.data = nil
		return n, nil
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (t *Topology) diagnosticStreams(r CommandResult) string {
	return t.diagnosticStreamsLimit(r, operator.MaxHostOutputBytes)
}

func (t *Topology) diagnosticStreamsLimit(r CommandResult, limit int) string {
	// A capture boundary, interruption or lost completion can split a secret.
	// Full-value replacement cannot make those bytes safe to print.
	if r.Truncated || r.Err != nil || r.Code == nil || *r.Code < 0 || *r.Code == 124 || *r.Code == 125 || *r.Code == 137 || len(r.Stdout) > limit || len(r.Stderr) > limit {
		return "stdout/stderr: incomplete output suppressed\n"
	}
	return "stdout:\n" + t.Redact(string(r.Stdout)) + "\nstderr:\n" + t.Redact(string(r.Stderr)) + "\n"
}

func (LocalRunner) Run(ctx context.Context, command Command) CommandResult {
	if len(command.Args) == 0 {
		return CommandResult{Err: errors.New("empty-command")}
	}
	cmd := exec.CommandContext(ctx, command.Args[0], command.Args[1:]...)
	// The job token is available only to P2's PublicReader. No credential, profile,
	// cloud config or caller token is inherited by a child command.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "AWS_EC2_METADATA_DISABLED=true"}
	cmd.Stdin = bytes.NewReader(command.Input)
	cmd.WaitDelay = time.Second
	out := boundedOutput{limit: commandOutputLimit(command.Label)}
	stderr := boundedOutput{limit: commandOutputLimit(command.Label)}
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	result := CommandResult{Stdout: out.data, Stderr: stderr.data, Truncated: out.truncated || stderr.truncated}
	if ctx.Err() != nil {
		result.Err = ctx.Err()
		return result
	}
	if err == nil {
		code := 0
		result.Code = &code
		return result
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		result.Code = &code
	} else {
		result.Err = errors.New("command-not-confirmed")
	}
	return result
}

func sortSecrets(values []string) {
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
}
func replaceSecret(text, secret string) string { return strings.ReplaceAll(text, secret, "[secret]") }
