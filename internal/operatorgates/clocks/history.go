package clocks

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/relaishq/relais/internal/operatorgates/host"
	"github.com/relaishq/relais/internal/operatorgates/observe"
	"github.com/relaishq/relais/pkg/operator"
)

// ReadHistoricalTracking is unwired. first/last are inclusive frozen row seconds,
// including the predecessor/successor and all same-second updates. boundary is a
// separate gate-owned read boundary; elapsed time since freeze never refuses.
// read must honor ctx and its existing deadline. A refusal returns no partial log.
func ReadHistoricalTracking(ctx context.Context, boundary, first, last int64, read func(context.Context, string) (string, error)) (TrackingLog, error) {
	refuse := func(c string) (TrackingLog, error) { return TrackingLog{}, &BindingError{Cause: "clock-history-" + c} }
	if first < 2 || last < first || last > (1<<63-1)/1000-1 || (last+1)*1000 > boundary {
		return refuse("invalid")
	}
	out := TrackingLog{RunStartUnixMS: boundary}
	var cursor json.RawMessage
	for range 2048 {
		script := host.HistoricalClock(cursor, boundary, first, last)
		if len(script) > operator.MaxScriptBytes {
			return refuse("limit")
		}
		stdout, err := read(ctx, script)
		if err != nil {
			return TrackingLog{}, errors.Join(&BindingError{Cause: "clock-history-incomplete"}, err)
		}
		if len(stdout) > operator.MaxHostOutputBytes {
			return refuse("limit")
		}
		raw, err := host.Unpack(stdout, 256000)
		var page struct {
			TrackingLog
			Cause string `json:"cause"`
		}
		if err != nil || observe.DecodeClosedLimit(raw, &page, nil, 256000) != nil {
			return refuse("invalid")
		}
		if page.Cause != "" {
			switch page.Cause {
			case "clock-history-invalid", "clock-history-limit", "clock-history-changed", "clock-history-gap", "clock-history-precision", "clock-history-incomplete":
				return TrackingLog{}, &BindingError{Cause: page.Cause}
			}
			return refuse("invalid")
		}
		if page.RunStartUnixMS != boundary {
			return refuse("gap")
		}
		for _, b := range page.Breaks {
			if historicalBreakTouches(b, first, last) {
				return refuse("gap")
			}
		}
		for _, r := range page.Rows {
			if r.UnixSeconds < first || r.UnixSeconds > last || !r.BeforeRun {
				return refuse("invalid")
			}
			v, e := strconv.ParseFloat(r.MaxErrorDecimal, 64)
			if _, err := Column14MaximumNS(r.MaxErrorDecimal); err != nil || e != nil || v != r.MaxErrorSeconds {
				return refuse("precision")
			}
			if n := len(out.Rows); n > 0 && (r.UnixSeconds < out.Rows[n-1].UnixSeconds || r.UnixSeconds-out.Rows[n-1].UnixSeconds > 60) {
				return refuse("gap")
			}
			out.Rows = append(out.Rows, r)
		}
		if len(out.Rows) > 16384 {
			return refuse("limit")
		}
		if page.ReadComplete {
			if len(out.Rows) == 0 || out.Rows[0].UnixSeconds != first || out.Rows[len(out.Rows)-1].UnixSeconds != last {
				return refuse("incomplete")
			}
			out.ReadComplete = true
			return out, nil
		}
		if len(page.Cursor) == 0 || string(page.Cursor) == "null" {
			return refuse("incomplete")
		}
		cursor = page.Cursor
	}
	return refuse("limit")
}

// State changes happen at the reported update. A missing-update span starts
// after its predecessor. The parser's EOF cap confines every log loss to this read.
func historicalBreakTouches(b TrackingBreak, first, last int64) bool {
	if b.Kind == "log-loss" {
		return true
	}
	if b.FromUnixSeconds < 0 || b.UnixSeconds < b.FromUnixSeconds {
		return true
	}
	start := b.FromUnixSeconds
	switch b.Kind {
	case "source-change", "leap-status", "self-consistency", "clock-step":
		start = b.UnixSeconds
	case "cursor-gap":
		if start < b.UnixSeconds {
			start++
		}
	}
	return start <= last && b.UnixSeconds >= first
}
