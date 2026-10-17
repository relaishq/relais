// Package capacity measures owned service processes during the media window.
package capacity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/common/expfmt"
	"github.com/relaishq/relais/internal/processrss"
)

type Target struct {
	AgentSessions            []string
	Name, URL                string
	RedisAddr, RedisPassword string
	PID                      int
	Done                     <-chan struct{}
	Err                      func() error
}
type MemorySample struct {
	HeapLiveBytes          uint64    `json:"heap_live_bytes,omitempty"` // Caller live set after GC; service sampling leaves this unset.
	PhysicalFootprintBytes uint64    `json:"physical_footprint_bytes,omitempty"`
	At                     time.Time `json:"at"`
	RSSBytes               uint64    `json:"rss_bytes"`
	HeapInUseBytes         uint64    `json:"heap_in_use_bytes,omitempty"`
}

type Process struct {
	Memory                  []MemorySample     `json:"memory_samples,omitempty"`
	Name                    string             `json:"name"`
	PID                     int                `json:"pid"`
	Samples                 int                `json:"samples"`
	ObservedSeconds         float64            `json:"observed_seconds"`
	CPUSeconds              float64            `json:"cpu_seconds"`
	PeakRSSBytes            float64            `json:"peak_rss_bytes,omitempty"`
	RSSByteSeconds          float64            `json:"rss_byte_seconds"`
	First                   map[string]float64 `json:"first_metrics,omitempty"`
	Last                    map[string]float64 `json:"last_metrics,omitempty"`
	Peaks                   map[string]float64 `json:"peak_gauges,omitempty"`
	CPUSecondsPerCallSecond float64            `json:"cpu_seconds_per_call_second"`
	MeanRSSBytesPerCall     float64            `json:"mean_sampled_rss_bytes_per_call,omitempty"`
	RSSKind                 string             `json:"rss_kind"`
	PeakHeapBytes           float64            `json:"peak_heap_bytes"`
	HeapByteSeconds         float64            `json:"heap_byte_seconds"`
	MeanHeapBytesPerCall    float64            `json:"mean_heap_bytes_per_call"`
	RSSUnavailable          bool               `json:"rss_unavailable,omitempty"`
	RatesPerCallSecond      map[string]float64 `json:"rates_per_call_second,omitempty"`
	UnexpectedExit          bool               `json:"unexpected_exit,omitempty"`
	Errors                  []string           `json:"errors,omitempty"`
	cpu, rss                float64
	heap                    float64
	at                      time.Time
}
type Telemetry struct {
	Agents        AgentSamples `json:"agent_diagnostics,omitempty"`
	WindowSeconds float64      `json:"window_seconds"`
	Processes     []Process    `json:"processes"`
	Errors        []string     `json:"errors,omitempty"`
}
type Monitor struct {
	agentDone chan struct{}
	memory    bool
	targets   []Target
	data      Telemetry
	cancel    context.CancelFunc
	done      chan struct{}
	client    *http.Client
	stores    []*redis.Client
	retired   []atomic.Bool
	started   time.Time
}

func Start(ctx context.Context, targets []Target, every time.Duration) *Monitor {
	return start(ctx, targets, every, false)
}

// StartSoak retains current RSS and Go heap-in-use samples for regression.
func StartSoak(ctx context.Context, targets []Target, every time.Duration) *Monitor {
	return start(ctx, targets, every, true)
}

func start(ctx context.Context, targets []Target, every time.Duration, memory bool) *Monitor {
	ctx, cancel := context.WithCancel(ctx)
	m := &Monitor{memory: memory, retired: make([]atomic.Bool, len(targets)), targets: targets, cancel: cancel, done: make(chan struct{}), client: &http.Client{Timeout: 500 * time.Millisecond}}
	for _, t := range targets {
		var client *redis.Client
		if t.RedisAddr != "" {
			client = redis.NewClient(&redis.Options{Addr: t.RedisAddr, Password: t.RedisPassword, MaxRetries: -1, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond, PoolSize: 1})
		}
		m.stores = append(m.stores, client)
		kind := "lifetime_peak"
		if t.RedisAddr != "" {
			kind = "redis_current_rss"
		}
		m.data.Processes = append(m.data.Processes, Process{RSSKind: kind, Name: t.Name, PID: t.PID, Peaks: map[string]float64{}})
	}
	m.agentDone = make(chan struct{})
	hasAgents := false
	for _, target := range targets {
		if len(target.AgentSessions) > 0 {
			hasAgents = true
		}
	}
	if hasAgents {
		go func() { defer close(m.agentDone); m.sampleAgents(ctx) }()
	} else {
		close(m.agentDone)
	}
	m.started = time.Now()
	m.sample(ctx)
	go func() {
		defer close(m.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				m.sample(ctx)
			}
		}
	}()
	return m
}
func (m *Monitor) sample(ctx context.Context) {
	var wg sync.WaitGroup
	for i, t := range m.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &m.data.Processes[i]
			if m.retired[i].Load() {
				return
			}
			select {
			case <-t.Done:
				return
			default:
			}
			values := map[string]float64{}
			var err error
			if t.URL != "" {
				values, err = scrape(ctx, m.client, t.URL+"/metrics")
			} else if m.stores[i] != nil {
				values, err = redisResources(ctx, m.stores[i])
			} else {
				err = fmt.Errorf("no resource endpoint for %s", t.Name)
			}
			if err != nil {
				if ctx.Err() != nil || m.retired[i].Load() {
					return
				}
				select {
				case <-t.Done:
					return
				default:
				}
				p.Errors = append(p.Errors, err.Error())
				return
			}
			if m.retired[i].Load() {
				return
			}
			cpu, ok := values["relais_process_cpu_seconds_total"]
			rss, rssOK := values["relais_process_rss_peak_bytes"]
			if !ok || !rssOK {
				p.Errors = append(p.Errors, "missing process CPU/RSS")
				return
			}
			select {
			case <-t.Done:
				return
			default:
			}
			now := time.Now()
			if m.memory {
				current, rssErr := processrss.Read(t.PID)
				if rssErr != nil {
					p.Errors = append(p.Errors, "current RSS: "+rssErr.Error())
				} else {
					p.Memory = append(p.Memory, MemorySample{At: now, RSSBytes: current.ResidentBytes, PhysicalFootprintBytes: current.PhysicalFootprintBytes, HeapInUseBytes: uint64(values["relais_process_heap_in_use_bytes"])})
				}
			}
			if p.Samples > 0 {
				dt := now.Sub(p.at).Seconds()
				if cpu < p.cpu {
					p.Errors = append(p.Errors, "CPU counter decreased")
					return
				}
				p.CPUSeconds += cpu - p.cpu
				p.RSSByteSeconds += (rss + p.rss) / 2 * dt
				p.HeapByteSeconds += (values["relais_process_heap_bytes"] + p.heap) / 2 * dt
				p.ObservedSeconds += dt
			} else {
				p.First = values
			}
			p.Samples++
			p.heap = values["relais_process_heap_bytes"]
			p.RSSUnavailable = rss == 0
			p.at, p.cpu, p.rss, p.Last = now, cpu, rss, values
			p.PeakRSSBytes = max(p.PeakRSSBytes, rss)
			p.PeakHeapBytes = max(p.PeakHeapBytes, values["relais_process_heap_bytes"])
			for name, value := range values {
				if !strings.Contains(name, "_total") {
					p.Peaks[name] = max(p.Peaks[name], value)
				}
			}
		}()
	}
	wg.Wait()
}
func (m *Monitor) Finish(callers int) Telemetry {
	m.cancel()
	<-m.done
	<-m.agentDone
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.sample(ctx)
	m.client.CloseIdleConnections()
	for _, store := range m.stores {
		if store != nil {
			_ = store.Close()
		}
	}
	m.data.WindowSeconds = time.Since(m.started).Seconds()
	for i, target := range m.targets {
		p := &m.data.Processes[i]
		if !m.retired[i].Load() {
			select {
			case <-target.Done:
				p.UnexpectedExit = true
				message := target.Name + ": process exited without retirement"
				if target.Err != nil {
					if err := target.Err(); err != nil {
						message += ": " + err.Error()
					}
				}
				p.Errors = append(p.Errors, message)
				m.data.Errors = append(m.data.Errors, message)
			default:
			}
		}
		m.data.Errors = append(m.data.Errors, normalizeProcess(p, callers, m.data.WindowSeconds)...)
	}

	return m.data
}

func scrape(ctx context.Context, client *http.Client, endpoint string) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain; version=0.0.4")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("metrics HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 1<<20 {
		return nil, fmt.Errorf("metrics exceed 1 MiB")
	}
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	values := map[string]float64{}
	for name, family := range families {
		if !strings.HasPrefix(name, "relais_") {
			continue
		}
		for _, metric := range family.Metric {
			key := name
			var labels []string
			for _, l := range metric.Label {
				labels = append(labels, l.GetName()+"="+strconv.Quote(l.GetValue()))
			}
			sort.Strings(labels)
			if len(labels) > 0 {
				key += "{" + strings.Join(labels, ",") + "}"
			}
			if metric.Counter != nil {
				values[key] += metric.GetCounter().GetValue()
			}
			if metric.Gauge != nil {
				values[key] += metric.GetGauge().GetValue()
			}
		}
	}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, errors.New("nonfinite or negative metrics sample")
		}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("no Relais metrics")
	}
	return values, nil
}

// Redis INFO supplies its own CPU and current RSS. Application command/wire
// counters exclude this one-per-second driver monitoring connection.
func redisResources(ctx context.Context, client *redis.Client) (map[string]float64, error) {
	body, err := client.Info(ctx, "all").Result()
	if err != nil {
		return nil, err
	}
	fields := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		number, err := strconv.ParseFloat(value, 64)
		if err == nil {
			fields[key] = number
		}
	}
	for _, key := range []string{"used_cpu_user", "used_cpu_sys", "used_memory_rss", "used_memory"} {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("redis INFO missing %s", key)
		}
	}
	return map[string]float64{"relais_process_cpu_seconds_total": fields["used_cpu_user"] + fields["used_cpu_sys"], "relais_process_rss_peak_bytes": fields["used_memory_rss"], "relais_process_heap_bytes": fields["used_memory"]}, nil
}

// Retire stops sampling before an intentional kill. In-flight responses are
// discarded. The last complete pre-fault window remains visible, up to one
// sampling interval before the actual fault; it never estimates later CPU.
func (m *Monitor) Retire(name string) {
	for i, t := range m.targets {
		if t.Name == name {
			select {
			case <-t.Done:
				return
			default:
			}
			m.retired[i].Store(true)
		}
	}
}

// Contributions to a step use its shared window. Memory averages describe
// only the process's observed lifetime; a retired tail is never extrapolated.
func normalizeProcess(p *Process, callers int, windowSeconds float64) []string {
	var problems []string
	if p.Samples < 2 || p.ObservedSeconds <= 0 || windowSeconds <= 0 || callers <= 0 {
		problems = append(problems, p.Name+": missing resource window: "+strings.Join(p.Errors, "; "))
		return problems
	}
	denominator := float64(callers) * windowSeconds
	p.CPUSecondsPerCallSecond = p.CPUSeconds / denominator
	p.MeanRSSBytesPerCall = p.RSSByteSeconds / (float64(callers) * p.ObservedSeconds)
	p.MeanHeapBytesPerCall = p.HeapByteSeconds / (float64(callers) * p.ObservedSeconds)
	p.RatesPerCallSecond = map[string]float64{}
	for name, last := range p.Last {
		if strings.Contains(name, "_total") {
			first, ok := p.First[name]
			if !ok || last < first {
				problems = append(problems, p.Name+": invalid counter "+name)
				continue
			}
			p.RatesPerCallSecond[name] = (last - first) / denominator
		}
	}
	if len(p.Errors) > 0 {
		problems = append(problems, p.Name+": resource sample errors")
	}

	return problems
}
