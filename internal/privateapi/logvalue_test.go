package privateapi

import (
	"strings"
	"testing"
)

func TestLogValueQuotesAndBounds(t *testing.T) {
	if got := LogValue("a\nmove failed id=x"); got != `"a\nmove failed id=x"` {
		t.Fatalf("LogValue newline = %s", got)
	}
	long := LogValue(strings.Repeat("x", logValueLimit+1))
	if want := `"` + strings.Repeat("x", logValueLimit) + `..."`; long != want {
		t.Fatalf("LogValue long length = %d, want %d", len(long), len(want))
	}
	if exact := LogValue(strings.Repeat("y", logValueLimit)); exact != `"`+strings.Repeat("y", logValueLimit)+`"` {
		t.Fatalf("LogValue at limit was truncated: %d", len(exact))
	}
}
