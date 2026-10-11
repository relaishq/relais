package loadgen

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/metrics"
	"testing"

	"github.com/relais/internal/processrss"
)

func TestRuntimeMemoryAccountsForReservedClasses(t *testing.T) {
	runtime.GC()
	m := readRuntimeMemory()
	classes := m.HeapObjectsBytes + m.HeapUnusedBytes + m.HeapFreeBytes + m.HeapReleasedBytes + m.HeapStacksBytes + m.OSStacksBytes + m.OtherBytes
	if classes != m.TotalBytes || m.OtherBytes > m.TotalBytes || m.HeapObjectsBytes == 0 || m.HeapStacksBytes == 0 || m.AllocatedBytes < m.HeapObjectsBytes || m.Goroutines == 0 {
		t.Fatalf("invalid runtime memory accounting: %+v", m)
	}
	samples := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(samples)
	if m.HeapLiveBytes == 0 || m.HeapLiveBytes != samples[0].Value.Uint64() {
		t.Fatalf("live heap did not use the GC live metric: %+v", m)
	}
}

func TestSoakSamplerRetainsMemoryBreakdown(t *testing.T) {
	if err := processrss.Check(); err != nil {
		t.Skipf("current process memory unavailable: %v", err)
	}
	runtime.GC()
	s := &Sampler{CurrentMemory: true}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Sample(&Meter{})
	if err != nil || stats.RuntimeMemory == nil || stats.RuntimeMemory.TotalBytes == 0 || stats.RuntimeMemory.HeapLiveBytes == 0 || stats.RSSBytes == 0 || stats.HeapInUseBytes == 0 {
		t.Fatalf("missing soak memory evidence: %+v, %v", stats, err)
	}
	var wire bytes.Buffer
	writer := NewWriter(&wire)
	writer.Write(Message{Type: "stats", PID: 42, Stats: &stats})
	writer.Write(Message{Type: "done", PID: 42})
	if err := writer.Err(); err != nil {
		t.Fatal(err)
	}
	report, err := ReadSoakProcess(&wire, 42, 0, DefaultLimits())
	if err != nil || len(report.Samples) != 1 || report.Samples[0].RuntimeMemory == nil || *report.Samples[0].RuntimeMemory != *stats.RuntimeMemory {
		t.Fatalf("caller protocol lost runtime memory evidence: %+v, %v", report, err)
	}
}

func TestMemoryProfilesUseOnlyTheOperatorMux(t *testing.T) {
	mux := http.NewServeMux()
	registerMemoryDiagnostics(mux, nil)
	for _, path := range []string{"/debug/pprof/heap?gc=1", "/debug/pprof/allocs"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, response.Code)
		}
		profile, err := gzip.NewReader(response.Body)
		if err != nil {
			t.Fatalf("%s: not a compressed pprof profile: %v", path, err)
		}
		data, err := io.ReadAll(profile)
		_ = profile.Close()
		if err != nil || len(data) == 0 {
			t.Fatalf("%s: empty or invalid profile: %v", path, err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	if _, pattern := http.DefaultServeMux.Handler(request); pattern != "" {
		t.Fatalf("profile unexpectedly exposed on the global mux: %q", pattern)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/debug/pprof/heap", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("profile accepted non-GET request: %d", response.Code)
	}
}
