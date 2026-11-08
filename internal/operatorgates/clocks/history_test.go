package clocks

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func historyWire(v any) string {
	raw, _ := json.Marshal(v)
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(raw)
	_ = w.Close()
	return base64.StdEncoding.EncodeToString(out.Bytes())
}
func TestHistoricalTracking(t *testing.T) {
	for _, mode := range []string{"ok", "wire-limit", "json", "precision", "float-token", "partial", "first-edge", "gap", "gap-60", "gap-61", "range-low", "range-high", "before-run", "boundary", "monotonic", "break"} {
		t.Run(mode, func(t *testing.T) {
			boundary := int64(400000)
			calls := 0
			read := func(ctx context.Context, script string) (string, error) {
				calls++
				if !strings.Contains(script, "historical_tracking(") {
					t.Fatal("wrong host command")
				}

				rows := []TrackingRow{}
				times := []int64{100, 150, 200, 250, 300}
				switch mode {
				case "monotonic":
					times = []int64{100, 150, 140, 190, 240, 300}
				case "gap-60":
					times = []int64{100, 160, 220, 280, 300}
				case "gap-61":
					times = []int64{100, 161, 221, 281, 300}
				}
				for _, at := range times {
					rows = append(rows, TrackingRow{UnixSeconds: at, BeforeRun: true, MaxErrorSeconds: 1.234e-5, MaxErrorDecimal: "1.234e-05", LeapStatus: "N"})
				}
				if mode == "json" {
					return historyWire(struct {
						TrackingLog
						Unexpected bool
					}{TrackingLog{Rows: rows, RunStartUnixMS: boundary, ReadComplete: true}, true}), nil
				}
				if mode == "wire-limit" {
					return historyWire(TrackingLog{Rows: rows, RunStartUnixMS: boundary, ReadComplete: true}) + strings.Repeat(" ", 24000), nil
				}
				if calls == 1 {
					rows = rows[:3]
				} else {
					rows = rows[3:]
				}
				if mode == "partial" && calls == 2 {
					rows[len(rows)-1].UnixSeconds--
				}
				if mode == "precision" {
					rows[0].MaxErrorDecimal = "0.00001234"
				}
				if mode == "float-token" {
					rows[0].MaxErrorDecimal = "1.235e-05"
				}
				if mode == "before-run" {
					rows[0].BeforeRun = false
				}
				if mode == "first-edge" && calls == 1 {
					rows[0].UnixSeconds++
				}
				if mode == "range-low" {
					rows[0].UnixSeconds = 99
				}
				if mode == "range-high" {
					rows[0].UnixSeconds = 301
				}
				if mode == "gap" {
					rows = append(rows[:1], rows[2])
				}
				done := calls == 2
				cursor := json.RawMessage(`{"page":1}`)
				page := TrackingLog{Rows: rows, RunStartUnixMS: boundary, ReadComplete: done, Cursor: cursor}
				if mode == "boundary" {
					page.RunStartUnixMS++
				}
				if mode == "break" {
					page.Breaks = []TrackingBreak{{FromUnixSeconds: 150, UnixSeconds: 151, Kind: "source-change"}}
				}
				return historyWire(page), nil
			}
			log, err := ReadHistoricalTracking(context.Background(), boundary, 100, 300, read)
			want := map[string]string{"wire-limit": "limit", "json": "invalid", "precision": "precision", "float-token": "precision", "partial": "incomplete", "first-edge": "incomplete", "gap": "gap", "gap-61": "gap", "range-low": "invalid", "range-high": "invalid", "before-run": "invalid", "boundary": "gap", "monotonic": "gap", "break": "gap"}[mode]
			if mode == "ok" || mode == "gap-60" {
				if err != nil || !log.ReadComplete || len(log.Rows) != 5 {
					t.Fatal("MUTATION: history-complete", err)
				}
				return
			}
			var refusal *BindingError
			if !errors.As(err, &refusal) || refusal.Cause != "clock-history-"+want || len(log.Rows) != 0 {
				t.Fatal("MUTATION: history-"+mode, err, calls)
			}
		})
	}
}

func TestHistoricalBreakScope(t *testing.T) {
	rows := []TrackingRow{}
	for _, at := range []int64{100, 150, 200, 250, 300} {
		rows = append(rows, TrackingRow{UnixSeconds: at, BeforeRun: true, MaxErrorSeconds: 1.234e-5, MaxErrorDecimal: "1.234e-05"})
	}
	for _, kind := range []string{"source-change", "leap-status", "cursor-gap", "self-consistency", "clock-step", "log-loss"} {
		for _, at := range []int64{99, 100, 300, 301, 305, 400} {
			b := TrackingBreak{FromUnixSeconds: at, UnixSeconds: at, Kind: kind}
			if kind != "leap-status" {
				b.FromUnixSeconds--
			}
			want := at >= 100 && at <= 300
			if kind == "log-loss" && at == 301 {
				want = true
			}
			_, err := ReadHistoricalTracking(context.Background(), 400000, 100, 300, func(context.Context, string) (string, error) {
				return historyWire(TrackingLog{Rows: rows, RunStartUnixMS: 400000, ReadComplete: true, Breaks: []TrackingBreak{b}}), nil
			})
			if (err != nil) != want {
				t.Fatalf("MUTATION: history-break-scope %s %d: %v", kind, at, err)
			}
		}
	}
}
