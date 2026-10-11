// Package loadgen runs the ordinary harness caller and measures its generator.
package loadgen

import (
	"fmt"
	"math"
	"runtime"
	"runtime/metrics"
	"sync"
	"syscall"
	"time"
)

// Lateness uses bounded, conservative 100us buckets up to one second. Overflow
// quantiles use the exact maximum, so the histogram never understates lateness.
type Lateness struct {
	Packets uint64        `json:"packets"`
	P50     time.Duration `json:"p50_ns"`
	P99     time.Duration `json:"p99_ns"`
	Max     time.Duration `json:"max_ns"`
}
type histogram struct {
	bins    [10001]uint64
	count   uint64
	maximum time.Duration
}

func (h *histogram) add(d time.Duration) {
	d = max(d, 0)
	i := min(int((d+100*time.Microsecond-1)/(100*time.Microsecond)), len(h.bins)-1)
	h.bins[i]++
	h.count++
	h.maximum = max(h.maximum, d)
}
func (h *histogram) quantile(q float64) time.Duration {
	target := uint64(math.Ceil(float64(h.count) * q))
	if target == 0 {
		return 0
	}
	var n uint64
	for i, count := range h.bins {
		n += count
		if n >= target {
			if i == len(h.bins)-1 {
				return h.maximum
			}
			return min(time.Duration(i)*100*time.Microsecond, h.maximum)
		}
	}
	return h.maximum
}
func (h *histogram) summary() Lateness {
	return Lateness{h.count, h.quantile(.5), h.quantile(.99), h.maximum}
}

type Meter struct {
	mu              sync.Mutex
	total, interval histogram
}

func (m *Meter) Observe(_ string, scheduled, actual time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total.add(actual.Sub(scheduled))
	m.interval.add(actual.Sub(scheduled))
}
func (m *Meter) Sample() (Lateness, Lateness) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all, interval := m.total.summary(), m.interval.summary()
	m.interval = histogram{}
	return all, interval
}

type Limits struct {
	P99        time.Duration `json:"p99_lateness_limit_ns"`
	CPUPercent float64       `json:"cpu_percent_per_gomaxprocs_limit"`
}

func DefaultLimits() Limits { return Limits{20 * time.Millisecond, 85} }
func (l Limits) Validate() error {
	if l.P99 <= 0 || l.CPUPercent <= 0 || l.CPUPercent > 100 || math.IsNaN(l.CPUPercent) {
		return fmt.Errorf("positive lateness and CPU limit in (0,100] required")
	}
	return nil
}

type Stats struct {
	At               time.Time     `json:"at"`
	Interval         time.Duration `json:"interval_ns"`
	CPUPercent       float64       `json:"cpu_percent_one_core"`
	CPUPerGOMAXPROCS float64       `json:"cpu_percent_per_gomaxprocs"`
	GOMAXPROCS       int           `json:"gomaxprocs"`
	HeapBytes        uint64        `json:"heap_bytes"`
	RuntimeBytes     uint64        `json:"runtime_bytes"`
	PeakRSSBytes     uint64        `json:"peak_rss_bytes"`
	Lateness         Lateness      `json:"lateness"`
	IntervalLateness Lateness      `json:"interval_lateness"`
}
type Sampler struct {
	at  time.Time
	cpu time.Duration
}

func cpuUsage() (time.Duration, uint64, error) {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil {
		return 0, 0, err
	}
	cpu := time.Duration(r.Utime.Sec+r.Stime.Sec)*time.Second + time.Duration(r.Utime.Usec+r.Stime.Usec)*time.Microsecond
	rss := uint64(r.Maxrss)
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	return cpu, rss, nil
}
func (s *Sampler) Reset() error { cpu, _, err := cpuUsage(); s.at, s.cpu = time.Now(), cpu; return err }
func (s *Sampler) Sample(m *Meter) (Stats, error) {
	cpu, rss, err := cpuUsage()
	if err != nil {
		return Stats{}, err
	}
	now := time.Now()
	elapsed := now.Sub(s.at)
	if s.at.IsZero() || elapsed <= 0 {
		return Stats{}, fmt.Errorf("resource sampler was not started")
	}
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}}
	metrics.Read(samples)
	all, interval := m.Sample()
	pct := 100 * float64(cpu-s.cpu) / float64(elapsed)
	out := Stats{now, elapsed, pct, pct / float64(runtime.GOMAXPROCS(0)), runtime.GOMAXPROCS(0), samples[0].Value.Uint64(), samples[1].Value.Uint64(), rss, all, interval}
	s.at, s.cpu = now, cpu
	return out, nil
}
func (l Limits) Causes(s Stats) []string {
	var reasons []string
	if s.Lateness.Packets == 0 {
		reasons = append(reasons, "no successful media sends")
	}
	if s.IntervalLateness.P99 > l.P99 {
		reasons = append(reasons, fmt.Sprintf("send lateness p99 %s exceeds %s", s.IntervalLateness.P99, l.P99))
	}
	if s.CPUPerGOMAXPROCS > l.CPUPercent {
		reasons = append(reasons, fmt.Sprintf("CPU %.2f%% per GOMAXPROCS exceeds %.2f%%", s.CPUPerGOMAXPROCS, l.CPUPercent))
	}
	return reasons
}
