package clocks

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			if kind == "log-loss" {
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

func TestHistoricalMalformedFirstCopy(t *testing.T) {
	const base = int64(1792886400)
	row := func(second int64) string {
		return time.Unix(base+second, 0).UTC().Format("2006-01-02 15:04:05") + " GPS 2 0 0 0 N 1 0 0 0 0 1.234e-05\n"
	}
	// The first of two updates in the first second is malformed. With no
	// predecessor, the parser stamps the loss at the current wall time.
	text := strings.Replace(row(100), "GPS 2", "GPS broken", 1)
	for second := int64(100); second <= 301; second++ {
		text += row(second)
	}
	path := filepath.Join(t.TempDir(), "tracking.log")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := ReadHistoricalTracking(context.Background(), (base+400)*1000, base+100, base+300, func(ctx context.Context, script string) (string, error) {
		body := strings.TrimSuffix(strings.TrimPrefix(script, "python3 - <<'PY'\n"), "PY\n")
		at := strings.LastIndex(body, "\nprint(")
		if at < 0 {
			t.Fatal("missing historical reader entry point")
		}
		setup := fmt.Sprintf("\nTRACKING_LOG=pathlib.Path(%q)\ndaemon_identity=lambda:'test-daemon'\n", path)
		cmd := exec.CommandContext(ctx, "python3", "-B", "-c", body[:at]+setup+body[at:])
		out, err := cmd.CombinedOutput()
		return string(out), err
	})
	var refusal *BindingError
	if !errors.As(err, &refusal) || refusal.Cause != "clock-history-gap" || len(log.Rows) != 0 {
		t.Fatalf("MUTATION: history-malformed-first-copy: rows=%d err=%v", len(log.Rows), err)
	}
}
