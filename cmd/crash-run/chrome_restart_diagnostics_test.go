//go:build !windows

package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	chromeRestartMaxOffers = 16
	chromeRestartMaxSDP    = 64 << 10
	chromeRestartMaxLines  = 512
	chromeRestartMaxDiff   = 24
	chromeRestartLineBytes = 240
)

// All input comes from the worker's credential-free normalized trace, never
// from a browser's raw localDescription. The caller holds traceMu.
type chromeRestartDiagnostics struct {
	initial  map[string]string
	refusals int
	logf     func(string, ...any)
}

func (d *chromeRestartDiagnostics) observe(name, call string, data []byte) bool {
	if name == "worker-restart-initial" {
		if len(d.initial) < chromeRestartMaxOffers && len(data) <= chromeRestartMaxSDP {
			d.initial[call] = string(data)
		}
		return true
	}
	reason, refused := strings.CutPrefix(name, "worker-restart-refused/")
	if !refused {
		return false
	}
	d.refusals++
	if d.refusals > chromeRestartMaxOffers {
		if d.refusals == chromeRestartMaxOffers+1 {
			d.logf("CHROME_RESTART_REFUSED further diagnostics truncated")
		}
		return true
	}
	d.logf("CHROME_RESTART_REFUSED call=%s reason=%s", call, reason)
	if reason == "identity" {
		initial := d.initial[call]
		if initial == "" || len(data) == 0 || len(data) > chromeRestartMaxSDP {
			d.logf("CHROME_RESTART_DIFF call=%s unavailable: initial or restart normalized SDP missing/oversize", call)
			return true
		}
		for _, line := range chromeRestartDiff(initial, string(data)) {
			d.logf("CHROME_RESTART_DIFF call=%s %s", call, line)
		}
	}
	return true
}

// A bounded LCS diff preserves attribute-order differences and aligns inserted
// or removed lines. Both CPU/memory and log size have fixed limits.
func chromeRestartDiff(initial, restart string) []string {
	left := strings.Split(strings.TrimSuffix(initial, "\r\n"), "\r\n")
	right := strings.Split(strings.TrimSuffix(restart, "\r\n"), "\r\n")
	if len(left) > chromeRestartMaxLines || len(right) > chromeRestartMaxLines {
		return []string{"unavailable: normalized SDP exceeds 512-line diff limit"}
	}
	width := len(right) + 1
	lcs := make([]uint16, (len(left)+1)*width)
	for i := len(left) - 1; i >= 0; i-- {
		for j := len(right) - 1; j >= 0; j-- {
			if left[i] == right[j] {
				lcs[i*width+j] = 1 + lcs[(i+1)*width+j+1]
			} else {
				lcs[i*width+j] = max(lcs[(i+1)*width+j], lcs[i*width+j+1])
			}
		}
	}
	var rows []string
	for i, j := 0, 0; i < len(left) || j < len(right); {
		if i < len(left) && j < len(right) && left[i] == right[j] {
			i++
			j++
			continue
		}
		if len(rows) == chromeRestartMaxDiff {
			return append(rows, "diff truncated after 24 changed lines")
		}
		var row string
		if i < len(left) && (j == len(right) || lcs[(i+1)*width+j] >= lcs[i*width+j+1]) {
			row = fmt.Sprintf("initial[%d] - %s", i+1, strconv.Quote(left[i]))
			i++
		} else {
			row = fmt.Sprintf("restart[%d] + %s", j+1, strconv.Quote(right[j]))
			j++
		}
		if len(row) > chromeRestartLineBytes {
			row = row[:chromeRestartLineBytes-3] + "..."
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return []string{"normalized offers identical; inspect the stored identity"}
	}
	return rows
}

func TestChromeRestartDiffExplainsInsertionsAndOrderChanges(t *testing.T) {
	initial := "v=0\r\na=mid:0\r\na=rtcp-rsize\r\na=ssrc:123 cname:caller\r\n"
	restart := "v=0\r\na=mid:0\r\na=ssrc:123 cname:caller\r\na=rtcp-mux-only\r\n"
	diff := strings.Join(chromeRestartDiff(initial, restart), "\n")
	require.Contains(t, diff, `initial[3] - "a=rtcp-rsize"`)
	require.Contains(t, diff, `restart[4] + "a=rtcp-mux-only"`)
	reordered := "v=0\r\na=rtcp-rsize\r\na=mid:0\r\na=ssrc:123 cname:caller\r\n"
	require.Contains(t, strings.Join(chromeRestartDiff(initial, reordered), "\n"), "a=mid:0")
	require.Equal(t, []string{"normalized offers identical; inspect the stored identity"}, chromeRestartDiff(initial, initial))
	whitespace := strings.Replace(initial, "cname:caller\r\n", "cname:caller \r\n", 1)
	require.Contains(t, strings.Join(chromeRestartDiff(initial, whitespace), "\n"), `cname:caller "`)
}

func TestChromeRestartDiagnosticsBoundEvidenceAndRefusals(t *testing.T) {
	var logs []string
	d := chromeRestartDiagnostics{initial: make(map[string]string), logf: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}}
	require.False(t, d.observe("worker-output", "call", nil))
	require.True(t, d.observe("worker-restart-initial", "call", []byte("v=0\r\na=mid:0\r\n")))
	require.True(t, d.observe("worker-restart-refused/identity", "call", []byte("v=0\r\na=mid:1\r\n")))
	require.Contains(t, strings.Join(logs, "\n"), "call=call reason=identity")
	require.Contains(t, strings.Join(logs, "\n"), `initial[2] - "a=mid:0"`)
	d.observe("worker-restart-refused/identity", "missing", nil)
	require.Contains(t, logs[len(logs)-1], "unavailable")
	d.observe("worker-restart-refused/offer_parse", "malformed", nil)
	require.Contains(t, logs[len(logs)-1], "reason=offer_parse")
	for range 100 {
		d.observe("worker-restart-refused/identity_parse", "call", nil)
	}
	require.LessOrEqual(t, len(logs), chromeRestartMaxOffers+5)
	require.Contains(t, logs[len(logs)-1], "further diagnostics truncated")
	d.observe("worker-restart-initial", "oversize", []byte(strings.Repeat("x", chromeRestartMaxSDP+1)))
	require.NotContains(t, d.initial, "oversize")
	for i := range 100 {
		d.observe("worker-restart-initial", fmt.Sprint(i), []byte("v=0\r\n"))
	}
	require.Len(t, d.initial, chromeRestartMaxOffers)
	rows := chromeRestartDiff(strings.Repeat("old\r\n", 100), strings.Repeat(strings.Repeat("x", 1000)+"\r\n", 100))
	require.Len(t, rows, chromeRestartMaxDiff+1)
	for _, row := range rows {
		require.LessOrEqual(t, len(row), chromeRestartLineBytes)
	}
	require.Contains(t, chromeRestartDiff(strings.Repeat("line\r\n", chromeRestartMaxLines+1), "v=0")[0], "512-line diff limit")
}
