package privateapi

import "strconv"

// logValueLimit bounds one variable field in a journal line.
const logValueLimit = 256

// LogValue quotes s for a journal line and keeps at most logValueLimit bytes,
// so a request field or error text cannot forge or flood log lines.
func LogValue(s string) string {
	if len(s) > logValueLimit {
		s = s[:logValueLimit] + "..."
	}
	return strconv.Quote(s)
}
