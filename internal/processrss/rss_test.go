package processrss

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

func TestCurrentOwnedProcessRSS(t *testing.T) {
	rss, err := Current(os.Getpid())
	if runtime.GOOS == "darwin" && errors.Is(err, ErrUnavailable) {
		t.Skip("Darwin current memory sampling requires cgo; unavailable in this build")
	}
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

func TestDarwinFootprintAndResidentReportedSeparately(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin physical footprint")
	}
	s, err := Read(os.Getpid())
	if errors.Is(err, ErrUnavailable) {
		t.Skip("Darwin current memory sampling requires cgo; unavailable in this build")
	}
	if err != nil || s.ResidentBytes == 0 || s.PhysicalFootprintBytes == 0 {
		t.Fatalf("current memory %+v: %v", s, err)
	}
}
