package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/relaishq/relais/pkg/controlplane"
)

func validateAttachConfig(cfg config) error {
	if cfg.ActionTimeout < 0 {
		return errors.New("-action-timeout must be non-negative")
	}
	if cfg.AttachControl == "" {
		if cfg.AttachAuthFile != "" || cfg.FaultHook != "" {
			return errors.New("-attach-auth-file and -fault-hook require -attach-control")
		}
		return nil
	}
	if selectedAgent(cfg) == "demo" {
		return errors.New("-agent=demo is unsupported in attach mode")
	}
	if cfg.RelayBuffer != "on" && cfg.RelayBuffer != "off" {
		return errors.New("-relay-buffer=on or off is required in attach mode")
	}
	if cfg.Redis != "" || cfg.LocalNetwork != "" {
		return errors.New("-attach-control is mutually exclusive with -redis and -local-network")
	}
	u, err := url.Parse(cfg.AttachControl)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("-attach-control must be an HTTP(S) base URL without credentials, path, query or fragment")
	}
	return nil
}

// privateapi and relais-control currently accept no credential at all. Do not
// invent a Bearer/Basic protocol or silently send a file to an unauthenticated
// peer. Keep the flag explicit for launchers; fail closed on a supplied secret.
func readAttachAuth(path string) error {
	if path == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open -attach-auth-file")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0177 != 0 {
		return errors.New("-attach-auth-file must be a regular file with mode 0600 or stricter")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return errors.New("cannot read bounded -attach-auth-file")
	}
	if len(bytes.TrimSpace(data)) != 0 {
		return errors.New("control private API has no credential authentication; omit -attach-auth-file")
	}
	return nil
}

func launchAttached(ctx context.Context, cfg config) (*demo, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := readAttachAuth(cfg.AttachAuthFile); err != nil {
		return nil, err
	}
	hook := cfg.FaultHook
	if hook != "" {
		var err error
		hook, err = filepath.Abs(hook)
		if err != nil {
			return nil, errors.New("invalid -fault-hook path")
		}
		info, err := os.Stat(hook)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return nil, errors.New("-fault-hook must be an executable file")
		}
	}
	id, err := newLaunchID()
	if err != nil {
		return nil, err
	}
	token, err := configToken(cfg)
	if err != nil {
		return nil, err
	}
	d := &demo{ctx: ctx, attached: true, faultHook: hook, actionTimeout: cfg.ActionTimeout,
		client: &http.Client{Timeout: 10 * time.Second}, control: strings.TrimRight(cfg.AttachControl, "/"),
		launchID: id, launchToken: token, agentName: selectedAgent(cfg), relayBuffer: selectedRelayBuffer(cfg)}
	if err := d.prepareResults(cfg.ResultsDir); err != nil {
		return nil, err
	}
	status, err := d.status(ctx)
	if err != nil {
		return nil, errors.New("cannot read attached control status")
	}
	d.expectedPoolSize = healthyPool(status.Workers)
	if d.expectedPoolSize == 0 {
		return nil, errors.New("attached control has no healthy registered workers")
	}
	return d, nil
}

func healthyWorker(w controlplane.WorkerStatus) bool {
	// Control owns heartbeat aging and marks expired registrations Dead. Do not
	// compare a remote wall clock to the page server's clock.
	return !w.Dead && !w.Recovering && !w.Draining && !w.LastHeartbeat.IsZero()
}
func healthyPool(workers []controlplane.WorkerStatus) int {
	n := 0
	for _, w := range workers {
		if healthyWorker(w) {
			n++
		}
	}
	return n
}
func (d *demo) actionDuration() time.Duration {
	if d.actionTimeout > 0 {
		return d.actionTimeout
	}
	if d.attached {
		return 90 * time.Second
	}
	return 15 * time.Second // preserve the local launcher's existing deadline
}

// A baseline identifies events by control's own times. Hook and page clocks
// cannot decide whether a remote takeover is new.
type takeoverKey struct {
	ID, From   string
	Start, End string
}
type takeoverBaseline map[takeoverKey]struct{}

func eventKey(event controlplane.MoveResult) takeoverKey {
	return takeoverKey{event.ID, event.From, event.Start.UTC().Format(time.RFC3339Nano), event.End.UTC().Format(time.RFC3339Nano)}
}
func baselineTakeovers(events []controlplane.MoveResult) takeoverBaseline {
	baseline := make(takeoverBaseline, len(events))
	for _, event := range events {
		baseline[eventKey(event)] = struct{}{}
	}
	return baseline
}

// Both ownership modes require a new terminal event and a settled dead owner.
func (d *demo) waitTakeover(ctx context.Context, owner controlplane.CallStatus, baseline takeoverBaseline) (controlplane.MoveResult, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var candidates []controlplane.MoveResult
	failure := func(err error) (controlplane.MoveResult, error) {
		// Only matching candidates from the last status sample, bounded by the
		// control API's recent-event ring. Preserve the underlying deadline cause.
		data, _ := json.Marshal(candidates)
		return controlplane.MoveResult{}, fmt.Errorf("takeover wait: %w; last candidate events: %s", err, data)
	}
	for {
		status, err := d.status(ctx)
		if err != nil {
			return failure(err)
		}
		recovered := false
		for _, w := range status.Workers {
			if w.Name == owner.Owner {
				recovered = w.Dead && !w.Recovering
			}
		}
		candidates = nil
		for _, event := range status.Takeovers {
			if event.ID != owner.ID || event.From != owner.Owner {
				continue
			}
			candidates = append(candidates, event)
		}
		for _, event := range candidates {
			_, existed := baseline[eventKey(event)]
			if recovered && !existed && !event.End.IsZero() {
				return event, nil
			}
		}
		select {
		case <-ctx.Done():
			return failure(ctx.Err())
		case <-ticker.C:
		}
	}
}

func (d *demo) killAttached(ctx context.Context, id string) (actionResult, error) {
	result := actionResult{Kind: "kill", ID: id}
	if d.faultHook == "" {
		return result, errors.New("-fault-hook is required for kill in attach mode")
	}
	owner, err := d.owner(ctx, id)
	if err != nil {
		return result, err
	}
	result.ID, result.From = owner.ID, owner.Owner
	// A failed prior replacement must not let the next action kill the survivor.
	status, err := d.status(ctx)
	if err != nil {
		return result, err
	}
	ownerHealthy := false
	for _, w := range status.Workers {
		if w.Name == owner.Owner && healthyWorker(w) {
			ownerHealthy = true
		}
	}
	if !ownerHealthy || status.PoolSize != d.expectedPoolSize || status.PoolSize < 2 {
		return result, errors.New("attached worker pool is not ready for kill")
	}
	baseline := baselineTakeovers(status.Takeovers)
	killed, err := d.runHook(ctx, "kill", owner.Owner, owner.ID)
	if err != nil {
		return result, err
	}
	result.PID, result.At = killed.PID, killed.KilledAt
	takeover, err := d.waitTakeover(ctx, owner, baseline)
	if err != nil {
		return result, err
	}
	if !takeover.Lost {
		result.To = takeover.To
	}
	replacement, err := d.runHook(ctx, "replace", owner.Owner, "")
	if err != nil {
		return result, err
	}
	result.Replacement = replacement.Replacement
	if err := d.waitAttachedPool(ctx, replacement.Replacement); err != nil {
		return result, err
	}
	if takeover.Lost || result.To == "" {
		return result, errors.New("call was lost during takeover")
	}
	return result, nil
}

func (d *demo) waitAttachedPool(ctx context.Context, replacement string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := d.status(ctx)
		if err != nil {
			return err
		}
		for _, w := range status.Workers {
			if w.Name == replacement && healthyWorker(w) && status.PoolSize == d.expectedPoolSize {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type hookReceipt struct {
	OK          bool      `json:"ok"`
	Action      string    `json:"action"`
	Worker      string    `json:"worker"`
	PID         int       `json:"pid"`
	KilledAt    time.Time `json:"killedAt"`
	Replacement string    `json:"replacement"`
	ReadyAt     time.Time `json:"readyAt"`
}

// Bound memory even when a faulty hook floods either stream. Stdout must fit;
// stderr retains a raw 64 KiB tail for redaction before its 4 KiB result. Writers always consume the entire write.
type hookOutput struct {
	data           []byte
	limit          int
	tail, overflow bool
}

func (b *hookOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data)+n > b.limit {
		b.overflow = true
	}
	if b.tail {
		if n >= b.limit {
			b.data = append(b.data[:0], p[n-b.limit:]...)
		} else {
			excess := len(b.data) + n - b.limit
			if excess > 0 {
				b.data = b.data[excess:]
			}
			b.data = append(b.data, p...)
		}
	} else if left := b.limit - len(b.data); left > 0 {
		b.data = append(b.data, p[:min(left, n)]...)
	}
	return n, nil
}

var credentialText = regexp.MustCompile(`(?i)(authorization|token|password|secret|credential|api[_-]?key)(["']?\s*[:=]\s*["']?)[^\r\n"']+`)

func redactHookStderr(data []byte, env []string) string {
	s := strings.ToValidUTF8(string(data), "?")
	s = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return '?'
		}
		return r
	}, s)
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		name = strings.ToUpper(name)
		if len(value) >= 8 && (strings.Contains(name, "KEY") || strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET") || strings.Contains(name, "PASSWORD") || strings.Contains(name, "CREDENTIAL") || strings.Contains(name, "AUTH")) {
			s = strings.ReplaceAll(s, value, "[redacted]")
		}
	}
	s = credentialText.ReplaceAllString(s, "$1$2[redacted]")
	if len(s) > 4096 {
		s = s[len(s)-4096:]
		if newline := strings.IndexByte(s, '\n'); newline >= 0 {
			s = s[newline+1:]
		} else {
			s = ""
		}
	}
	return strings.TrimSpace(s)
}

func (d *demo) runHook(ctx context.Context, action, worker, session string) (hookReceipt, error) {
	args := []string{action, "--worker", worker}
	if action == "kill" {
		args = append(args, "--session", session)
	}
	// No shell, no added credentials or cluster variables. The operator supplies
	// the hook's minimal environment when launching the page server.
	cmd := exec.CommandContext(ctx, d.faultHook, args...)
	cmd.Env = make([]string, 0) // empty must not mean inherit the parent environment
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OPENAI_API_KEY=") && !strings.HasPrefix(entry, "RELAIS_VOICE_WARM=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 250 * time.Millisecond
	stdout := &hookOutput{limit: 64 << 10}
	stderr := &hookOutput{limit: 64 << 10, tail: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	failure := func(reason string) (hookReceipt, error) {
		tail := redactHookStderr(stderr.data, cmd.Env)
		if tail != "" {
			reason += "; stderr: " + tail
		}
		return hookReceipt{}, fmt.Errorf("fault hook %s: %s", action, reason)
	}
	if ctx.Err() != nil {
		return failure("action timeout or cancellation")
	}
	if err != nil {
		return failure("non-zero exit or execution failure")
	}
	if stdout.overflow {
		return failure("stdout too large")
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.data))
	dec.DisallowUnknownFields()
	var receipt hookReceipt
	if dec.Decode(&receipt) != nil || dec.Decode(new(any)) != io.EOF {
		return failure("expected exactly one JSON object")
	}
	if !receipt.OK || receipt.Action != action || receipt.Worker != worker {
		return failure("invalid or unsuccessful receipt")
	}
	if action == "kill" {
		_, offset := receipt.KilledAt.Zone()
		if receipt.PID <= 0 || receipt.KilledAt.IsZero() || offset != 0 {
			return failure("invalid kill PID or UTC killedAt")
		}
	} else if receipt.Replacement == "" || receipt.Replacement == worker || receipt.ReadyAt.IsZero() {
		return failure("invalid replacement or readyAt")
	}
	return receipt, nil
}
