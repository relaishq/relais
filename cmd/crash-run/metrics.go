package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/relais/internal/clusterprocess"
)

var metricsInterval = time.Second

// ProcessMetrics retains bounded observations per process, not a time series.
// Values are the last successful scrape, even when the process later exits.
// Rates use two successful observations of each lifetime counter. They do not
// estimate packets sent after the last scrape of a killed process.
type ProcessMetrics struct {
	Process    string             `json:"process"`
	Samples    int                `json:"samples"`
	FirstAt    time.Time          `json:"first_at,omitempty"`
	LastAt     time.Time          `json:"last_at,omitempty"`
	Exited     bool               `json:"exited"`
	Error      string             `json:"error,omitempty"`
	Values     map[string]float64 `json:"values,omitempty"`
	GaugePeaks map[string]float64 `json:"observed_gauge_peaks,omitempty"`
	Rates      map[string]float64 `json:"observed_rate_per_second,omitempty"`
}

type metricsTarget struct {
	url      string
	done     <-chan struct{}
	inFlight bool // guarded by metricsRun.mu; overlapping samples skip this target
	first    map[string]float64
	data     ProcessMetrics
}

type metricsRun struct {
	mu      sync.Mutex
	targets []*metricsTarget
	cancel  context.CancelFunc
	done    chan struct{}
	client  *http.Client
}

func newMetricsRun() *metricsRun {
	ctx, cancel := context.WithCancel(context.Background())
	r := &metricsRun{
		cancel: cancel, done: make(chan struct{}),
		client: &http.Client{Timeout: 500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	go func() {
		defer close(r.done)
		if metricsInterval == 0 {
			return
		}
		tick := time.NewTicker(metricsInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				r.sample(ctx)
			}
		}
	}()
	return r
}

func (r *metricsRun) add(name, endpoint string, child *clusterprocess.Child) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append(r.targets, &metricsTarget{url: endpoint + "/metrics", done: child.Done(), data: ProcessMetrics{Process: name}})
}

func (r *metricsRun) sample(ctx context.Context) {
	r.mu.Lock()
	targets := append([]*metricsTarget(nil), r.targets...)
	r.mu.Unlock()
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		r.sampleTarget(ctx, target)
	}
}

// sampleBeforeFault shares one short budget across all targets. It never waits
// for a periodic scrape already in flight, keeping fault timing bounded.
func (r *metricsRun) sampleBeforeFault(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	r.sample(ctx)
}

func (r *metricsRun) sampleTarget(ctx context.Context, target *metricsTarget) {
	r.mu.Lock()
	select {
	case <-target.done:
		target.data.Exited, target.data.Error = true, ""
		r.mu.Unlock()
		return
	default:
	}
	if target.inFlight {
		r.mu.Unlock()
		return
	}
	target.inFlight = true
	r.mu.Unlock()

	values, counters, err := scrapeMetrics(ctx, r.client, target.url)
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	target.inFlight = false
	select {
	case <-target.done:
		// Discard responses from an exited generation, including refused scrapes
		// and a replacement that may have reused the same URL meanwhile.
		target.data.Exited, target.data.Error = true, ""
		return
	default:
	}
	if err != nil {
		target.data.Error = err.Error()
		return
	}
	if target.data.Samples == 0 {
		target.data.FirstAt = now
		target.first = values
		target.data.GaugePeaks = make(map[string]float64)
	}
	target.data.Samples++
	target.data.LastAt, target.data.Values, target.data.Error = now, values, ""
	target.data.Rates = make(map[string]float64)
	for name, value := range values {
		if !counters[name] {
			if peak, seen := target.data.GaugePeaks[name]; !seen || value > peak {
				target.data.GaugePeaks[name] = value
			}
		} else if first, ok := target.first[name]; ok && target.data.Samples > 1 && value >= first {
			target.data.Rates[name] = (value - first) / now.Sub(target.data.FirstAt).Seconds()
		}
	}
}

func (r *metricsRun) finish(dir string) []ProcessMetrics {
	r.cancel()
	<-r.done
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.sample(ctx)
	r.client.CloseIdleConnections()
	out := make([]ProcessMetrics, 0, len(r.targets))
	for _, target := range r.targets {
		out = append(out, target.data)
	}
	printMetrics(out)
	encoded, err := json.MarshalIndent(out, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "metrics.json"), append(encoded, '\n'), 0600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "METRICS report write failed: %v\n", err)
	}
	return out
}

func scrapeMetrics(ctx context.Context, client *http.Client, endpoint string) (map[string]float64, map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/plain; version=0.0.4")
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("metrics HTTP status %d", response.StatusCode)
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxBody {
		return nil, nil, errors.New("metrics response exceeds 1 MiB")
	}
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, nil, err
	}
	values, counters := make(map[string]float64), make(map[string]bool)
	put := func(name string, labels []*dto.LabelPair, value float64, counter bool) {
		key := metricKey(name, labels)
		values[key], counters[key] = value, counter
	}
	for name, family := range families {
		if !strings.HasPrefix(name, "relais_") {
			continue
		}
		for _, metric := range family.GetMetric() {
			switch family.GetType() {
			case dto.MetricType_COUNTER:
				put(name, metric.Label, metric.GetCounter().GetValue(), true)
			case dto.MetricType_GAUGE:
				put(name, metric.Label, metric.GetGauge().GetValue(), false)
			case dto.MetricType_HISTOGRAM:
				put(name+"_count", metric.Label, float64(metric.GetHistogram().GetSampleCount()), true)
				put(name+"_sum", metric.Label, metric.GetHistogram().GetSampleSum(), true)
			case dto.MetricType_SUMMARY:
				put(name+"_count", metric.Label, float64(metric.GetSummary().GetSampleCount()), true)
				put(name+"_sum", metric.Label, metric.GetSummary().GetSampleSum(), true)
			case dto.MetricType_UNTYPED:
				put(name, metric.Label, metric.GetUntyped().GetValue(), false)
			}
		}
	}
	if len(values) == 0 {
		return nil, nil, errors.New("metrics response has no Relais samples")
	}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, nil, errors.New("metrics response has a non-finite sample")
		}
	}
	return values, counters, nil
}

func metricKey(name string, labels []*dto.LabelPair) string {
	if len(labels) == 0 {
		return name
	}
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		parts = append(parts, label.GetName()+"="+strconv.Quote(label.GetValue()))
	}
	sort.Strings(parts)
	return name + "{" + strings.Join(parts, ",") + "}"
}

func printMetrics(processes []ProcessMetrics) {
	fmt.Println("METRICS (last successful sample per process; rates over observed interval)")
	for _, process := range processes {
		fmt.Printf("  process=%s samples=%d exited=%t", process.Process, process.Samples, process.Exited)
		if process.Error != "" {
			fmt.Printf(" error=%q", process.Error)
		}
		if process.Samples == 0 {
			fmt.Print(" NOT-RUN: no successful scrape")
		} else {
			fmt.Printf(" sample_age_ms=%.1f", float64(time.Since(process.LastAt))/float64(time.Millisecond))
		}
		fmt.Println()
		for i, name := range sortedMetricNames(process.Values) {
			if i%4 == 0 {
				fmt.Print("    ")
			} else {
				fmt.Print("  ")
			}
			short := strings.TrimPrefix(name, "relais_")
			for _, role := range []string{"relay_", "worker_", "control_"} {
				if strings.HasPrefix(short, role) {
					short = strings.TrimPrefix(short, role)
					break
				}
			}
			fmt.Printf("%s=%g", short, process.Values[name])
			if rate, ok := process.Rates[name]; ok && (strings.Contains(name, "packets_") || strings.Contains(name, "route_writes_")) {
				fmt.Printf("(%.1f/s)", rate)
			}
			if peak, ok := process.GaugePeaks[name]; ok {
				fmt.Printf("(peak=%g)", peak)
			}
			if i%4 == 3 {
				fmt.Println()
			}
		}
		if len(process.Values)%4 != 0 {
			fmt.Println()
		}
	}
}

func sortedMetricNames(values map[string]float64) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
