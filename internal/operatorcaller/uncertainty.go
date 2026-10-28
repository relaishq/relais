package operatorcaller

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/relaishq/relais/pkg/operator"
)

func uncertainty(f Fence, kind, id, caller, action string, end, now time.Time, rule string) {
	if recorder, ok := f.(operator.UncertaintyRecorder); ok {
		u := operator.Uncertainty{Kind: kind, RequestID: id, Target: "caller", CallerID: caller, Action: action, Rule: rule}
		if !end.IsZero() {
			u.DeadlineUnixMS = strconv.FormatInt(end.UnixMilli(), 10)
			u.RemainingMS = end.Sub(now).Milliseconds()
		}
		recorder.RecordUncertainty(u)
	}
}
func uncertaintyRule(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	switch err.Error() {
	case "caller-deadline", "caller-probe-deadline":
		return "timeout"
	case "invalid-caller-receipt":
		return "malformed-reply"
	case "caller-stopped":
		return "caller-stopped"
	default:
		return "read-failed"
	}
}
