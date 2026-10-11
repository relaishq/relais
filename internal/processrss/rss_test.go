package processrss

import (
	"os"
	"testing"
)

func TestCurrentOwnedProcessRSS(t *testing.T) {
	rss, err := Current(os.Getpid())
	if err != nil || rss == 0 {
		t.Fatalf("current RSS=%d err=%v", rss, err)
	}
	if _, err := Current(0); err == nil {
		t.Fatal("invalid PID accepted")
	}
	if _, err := Current(2147483647); err == nil {
		t.Fatal("absent process accepted")
	}
}
