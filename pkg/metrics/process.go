package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Sample is a process-local observation. Names and types must stay fixed
// across snapshots. Samples have no session or caller labels.
type Sample struct {
	Name, Help string
	Type       prometheus.ValueType
	Value      float64
}

func Counter(name, help string, value uint64) Sample {
	return Sample{Name: name, Help: help, Type: prometheus.CounterValue, Value: float64(value)}
}

func Gauge(name, help string, value int) Sample {
	return Sample{Name: name, Help: help, Type: prometheus.GaugeValue, Value: float64(value)}
}

type processCollector struct{ snapshot func() []Sample }

func (c processCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, sample := range c.snapshot() {
		ch <- prometheus.NewDesc(sample.Name, sample.Help, nil, nil)
	}
}

func (c processCollector) Collect(ch chan<- prometheus.Metric) {
	for _, sample := range c.snapshot() {
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(sample.Name, sample.Help, nil, nil), sample.Type, sample.Value)
	}
}

// ProcessHandler uses an isolated registry for this instance, excluding the
// global egress and HTTP metrics. Mount it only on the process's private API.
// Additional collectors are the extension point for replay-buffer (#27)
// ring use, drops and replays when those counters become available.
func ProcessHandler(snapshot func() []Sample, additional ...prometheus.Collector) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(processCollector{snapshot: snapshot})
	registry.MustRegister(additional...)
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// CheckpointCollectors selects only the checkpoint collectors from the global
// registry. Their lifetime is process-wide, so multiple in-process workers or
// control planes share these observations; separate binaries do not.
func CheckpointCollectors() []prometheus.Collector {
	// Expose all bounded outcomes, including zeroes before the first event.
	CheckpointWrites.WithLabelValues("success")
	CheckpointWrites.WithLabelValues("failure")
	CheckpointEnvelopeEvents.WithLabelValues("scaled")
	CheckpointEnvelopeEvents.WithLabelValues("definitive-loss")
	return []prometheus.Collector{CheckpointWrites, CheckpointEnvelopeEvents, CheckpointAge}
}
