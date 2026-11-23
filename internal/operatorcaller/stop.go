package operatorcaller

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

// StoppedError preserves a caller's terminal outcome separately from malformed
// evidence. Reason is a fixed public code, never the mailbox's free-form text.
type StoppedError struct{ Reason StopReason }

func (e *StoppedError) Error() string {
	if e.Reason == "" {
		return "caller-stopped"
	}
	return "caller-stopped:" + string(e.Reason)
}

type StopReason string

var stopReasons = map[string]StopReason{
	"Generator saturation hard ceiling":              "generator-saturation-hard-ceiling",
	"Generator saturation sustained wall-time":       "generator-saturation-sustained",
	"Generator saturation exceeds planned 1% budget": "generator-saturation-budget",
	"Invalid generator statistics":                   "invalid-generator-statistics",
	"Caller stream/evidence failure":                 "caller-stream-failure",
	"Caller supervision failure":                     "caller-supervision-failure",
}

// Public also validates saved artifacts before their reason is published.
func (r StopReason) Public() StopReason {
	return StopReason(operator.PublicCallerStopReason(string(r)))
}

type StopReceipt struct {
	RunID  string     `json:"runId"`
	Job    string     `json:"job"`
	Reason StopReason `json:"reason"`
}

type CompletionReceipt struct {
	RunID      string     `json:"runId"`
	Job        string     `json:"job"`
	Done       bool       `json:"done"`
	Stopped    bool       `json:"stopped"`
	ExitCode   *int       `json:"exitCode"`
	Sessions   []string   `json:"sessions"`
	StopReason StopReason `json:"stopReason,omitempty"`
}

// CheckStopped is a pure observation. A caller stop never confirms an outstanding
// command, clears a fence, or qualifies collection as successful.
func (b *Broker) CheckStopped(ctx context.Context) (err error) {
	b.requestID++
	id := strconv.FormatUint(b.requestID, 10)
	defer func() {
		if err != nil {
			var stopped *StoppedError
			var reason StopReason
			if errors.As(err, &stopped) {
				reason = stopped.Reason
			}
			end := time.UnixMilli(b.Manifest.Deadline)
			if deadline, ok := ctx.Deadline(); ok {
				end = minTime(end, deadline)
			}
			uncertainty(b.Fence, "mailbox.read", id, b.Manifest.CallerID, "stop", end, b.Clock.Now(), uncertaintyRule(err), reason)
		}
	}()
	r, err := b.Mailbox.Read(ctx, b.Manifest.Job()+"/stop.json")
	if err != nil || !r.Found {
		return err
	}
	var receipt struct {
		RunID  string `json:"runId"`
		Job    string `json:"job"`
		Reason string `json:"reason"`
	}
	if decode(r.Data, &receipt) != nil || receipt.RunID != b.Manifest.RunID || receipt.Job != "caller-1" {
		return errors.New("invalid-caller-receipt")
	}
	reason := stopReasons[receipt.Reason].Public()
	b.Stop = &StopReceipt{RunID: receipt.RunID, Job: receipt.Job, Reason: reason}
	return &StoppedError{Reason: reason}
}
