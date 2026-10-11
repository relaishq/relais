//go:build !windows

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"runtime"
	runtimemetrics "runtime/metrics"
	"syscall"
)

func processResources() []Sample {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return nil
	}
	rss := float64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	cpu := float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	memory := []runtimemetrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	runtimemetrics.Read(memory)
	return []Sample{
		{Name: "relais_process_cpu_seconds_total", Help: "Process user plus system CPU seconds, sampled at scrape.", Type: prometheus.CounterValue, Value: cpu},
		{Name: "relais_process_rss_peak_bytes", Help: "Lifetime peak resident memory, including setup; sampled at scrape.", Type: prometheus.GaugeValue, Value: rss},
		{Name: "relais_process_heap_bytes", Help: "Current Go heap object bytes, sampled at scrape.", Type: prometheus.GaugeValue, Value: float64(memory[0].Value.Uint64())},
	}
}
