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
	for _, mode := range []string{"ok", "wire-limit", "json", "precision", "partial", "stalled", "gap"} {
		t.Run(mode, func(t *testing.T) {
			boundary := int64(400000)
			calls := 0
			read := func(ctx context.Context, script string) (string, error) {
				calls++
				if !strings.Contains(script, "historical_tracking(") {
					t.Fatal("wrong host command")
				}

				rows := []TrackingRow{}
				for _, at := range []int64{100, 150, 200, 250, 300} {
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
					rows[0].MaxErrorDecimal = ""
				}
				if mode == "gap" {
					rows = append(rows[:1], rows[2])
				}
				done := calls == 2
				cursor := json.RawMessage(`{"page":1}`)
				if mode == "stalled" {
					done = false
				}
				return historyWire(TrackingLog{Rows: rows, RunStartUnixMS: boundary, ReadComplete: done, Cursor: cursor}), nil
			}
			log, err := ReadHistoricalTracking(context.Background(), boundary, 100, 300, read)
			want := map[string]string{"wire-limit": "limit", "json": "invalid", "precision": "precision", "partial": "incomplete", "stalled": "incomplete", "gap": "gap"}[mode]
			if mode == "ok" {
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
