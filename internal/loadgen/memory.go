package loadgen

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"

	"github.com/relais/internal/processrss"
	"github.com/relais/pkg/callharness"
)

// RuntimeMemory separates heap objects (including garbage awaiting collection)
// from unused spans, free pages and stacks. Released pages are eligible for OS
// reclaim; platform accounting may still include them in resident memory.
// OSStacksBytes does not account for all cgo thread stacks.
type RuntimeMemory struct {
	HeapLiveBytes     uint64 `json:"heap_live_bytes"`
	HeapObjectsBytes  uint64 `json:"heap_objects_bytes"`
	HeapUnusedBytes   uint64 `json:"heap_unused_bytes"`
	HeapFreeBytes     uint64 `json:"heap_free_bytes"`
	HeapReleasedBytes uint64 `json:"heap_released_bytes"`
	HeapStacksBytes   uint64 `json:"heap_stacks_bytes"`
	OSStacksBytes     uint64 `json:"os_stacks_bytes"`
	OtherBytes        uint64 `json:"other_bytes"`
	TotalBytes        uint64 `json:"total_bytes"`
	AllocatedBytes    uint64 `json:"allocated_bytes"`
	Goroutines        uint64 `json:"goroutines"`
}

func readRuntimeMemory() RuntimeMemory {
	samples := [...]metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/memory/classes/heap/stacks:bytes"},
		{Name: "/memory/classes/os-stacks:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/gc/heap/live:bytes"},
	}
	metrics.Read(samples[:])
	m := RuntimeMemory{HeapObjectsBytes: samples[0].Value.Uint64(), HeapUnusedBytes: samples[1].Value.Uint64(), HeapFreeBytes: samples[2].Value.Uint64(), HeapReleasedBytes: samples[3].Value.Uint64(), HeapStacksBytes: samples[4].Value.Uint64(), OSStacksBytes: samples[5].Value.Uint64(), TotalBytes: samples[6].Value.Uint64(), AllocatedBytes: samples[7].Value.Uint64(), Goroutines: samples[8].Value.Uint64()}
	m.OtherBytes = m.TotalBytes - m.HeapObjectsBytes - m.HeapUnusedBytes - m.HeapFreeBytes - m.HeapReleasedBytes - m.HeapStacksBytes - m.OSStacksBytes
	// This is the live set marked by the previous natural GC, not the current
	// object allocation count. Sampling does not trigger collection.
	m.HeapLiveBytes = samples[9].Value.Uint64()
	return m
}

// These opt-in soak diagnostics share the caller's existing loopback-only
// operator listener. No global pprof mux or additional listener is installed.
func registerMemoryDiagnostics(mux *http.ServeMux, calls []*callharness.Call) {
	for _, name := range []string{"heap", "allocs"} {
		mux.HandleFunc("GET /debug/pprof/"+name, func(w http.ResponseWriter, r *http.Request) {
			// Collection happens only at an explicit operator request. Ordinary
			// one-second sampling and memory judgment never force GC.
			if name == "heap" && r.URL.Query().Get("gc") == "1" {
				runtime.GC()
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_ = pprof.Lookup(name).WriteTo(w, 0)
		})
	}
	mux.HandleFunc("GET /debug/memory", func(w http.ResponseWriter, _ *http.Request) {
		memory, err := processrss.Read(os.Getpid())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		recordings := make([]callharness.RecordingReport, len(calls))
		for i, c := range calls {
			recordings[i] = c.Recording()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Runtime    RuntimeMemory                 `json:"runtime"`
			Process    processrss.Sample             `json:"process"`
			Recordings []callharness.RecordingReport `json:"recordings"`
		}{readRuntimeMemory(), memory, recordings})
	})
}
