package mediaworker

import (
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Measure the fake wire itself so a bounded capture channel cannot be mistaken
// for production pacing pressure. All writes still use the original sink.
type bridgeWireProbe struct {
	packetConn
	original *dtlsWireConn
	maxDepth atomic.Int64
	mu       sync.Mutex
	wall     []time.Duration
}

func (p *bridgeWireProbe) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	depth := int64(min(cap(p.original.packets), len(p.original.packets)+1))
	for {
		old := p.maxDepth.Load()
		if old >= depth || p.maxDepth.CompareAndSwap(old, depth) {
			break
		}
	}
	at := time.Now()
	n, err := p.original.WriteToUDPAddrPort(b, addr)
	p.mu.Lock()
	p.wall = append(p.wall, time.Since(at))
	p.mu.Unlock()
	return n, err
}
func (p *bridgeWireProbe) summary() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{"capacity": cap(p.original.packets), "maximum_depth": p.maxDepth.Load(), "write_p99_ms": bridgeQuantile(p.wall, .99), "write_max_ms": bridgeQuantile(p.wall, 1)}
}

type bridgeLoadFactor struct {
	Procs          int
	Busy           bool
	Stall, Renewal time.Duration
}

func bridgeFactor(name string) (bridgeLoadFactor, bool) {
	factors := map[string]bridgeLoadFactor{
		"baseline": {Procs: 2},
		"combined": {Procs: 1, Busy: true, Stall: 150 * time.Millisecond, Renewal: 250 * time.Millisecond},
		"cpu1":     {Procs: 1, Busy: true},
		"cpu2":     {Procs: 2, Busy: true},
		"stalls":   {Procs: 2, Stall: 150 * time.Millisecond},
		"renewal":  {Procs: 2, Renewal: 250 * time.Millisecond},
	}
	f, ok := factors[name]
	return f, ok
}

type bridgeQueueSample struct {
	At            time.Time
	Source, Pacer int
}
type bridgeStallObservation struct {
	Start, End, Release             time.Time
	SourceBaseline, PacerBaseline   int
	SourceStable, PacerStable       time.Time
	SourceRecovered, PacerRecovered time.Time
	CensoredAt                      time.Time
}

// Bounded fixture-only observations. Recovery requires 100 ms continuously at
// or below the largest queue seen in the preceding 500 ms, after output resumes.
// Source and pacer queues are deliberately measured separately.
type bridgeStallObserver struct {
	mu      sync.Mutex
	history []bridgeQueueSample
	stalls  []bridgeStallObservation
}

func (m *bridgeStallObserver) sample(at time.Time, source, pacer int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = append(m.history, bridgeQueueSample{at, source, pacer})
	cut := 0
	for cut < len(m.history) && at.Sub(m.history[cut].At) > 500*time.Millisecond {
		cut++
	}
	m.history = m.history[cut:]
	if len(m.stalls) == 0 {
		return
	}
	s := &m.stalls[len(m.stalls)-1]
	if s.Release.IsZero() || at.Before(s.Release) || !s.CensoredAt.IsZero() {
		return
	}
	advance := func(value, baseline int, stable, recovered *time.Time) {
		if !recovered.IsZero() {
			return
		}
		if value > baseline {
			*stable = time.Time{}
			return
		}
		if stable.IsZero() {
			*stable = at
		}
		if at.Sub(*stable) >= 100*time.Millisecond {
			*recovered = at
		}
	}
	advance(source, s.SourceBaseline, &s.SourceStable, &s.SourceRecovered)
	advance(pacer, s.PacerBaseline, &s.PacerStable, &s.PacerRecovered)
}
func (m *bridgeStallObserver) begin(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.stalls) > 0 {
		m.stalls[len(m.stalls)-1].CensoredAt = at
	}
	s := bridgeStallObservation{Start: at}
	for _, h := range m.history {
		if !h.At.After(at) && at.Sub(h.At) <= 500*time.Millisecond {
			s.SourceBaseline = max(s.SourceBaseline, h.Source)
			s.PacerBaseline = max(s.PacerBaseline, h.Pacer)
		}
	}
	m.stalls = append(m.stalls, s)
}
func (m *bridgeStallObserver) end(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stalls[len(m.stalls)-1].End = at
}
func (m *bridgeStallObserver) release(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stalls[len(m.stalls)-1].Release = at
}
func (m *bridgeStallObserver) copy() []bridgeStallObservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.stalls)
}
func bridgeStallOverlap(ready, admitted time.Time, stalls []bridgeStallObservation) time.Duration {
	var overlap time.Duration
	for _, s := range stalls {
		if s.End.IsZero() {
			continue
		}
		start, end := ready, admitted
		if s.Start.After(start) {
			start = s.Start
		}
		if s.End.Before(end) {
			end = s.End
		}
		if end.After(start) {
			overlap += end.Sub(start)
		}
	}
	return overlap
}
func (m *bridgePacketTiming) stallSummary(stalls []bridgeStallObservation, sourceEnd time.Time) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	var adjustedLower, adjustedUpper, cohortLower, cohortUpper, direct []time.Duration
	var sourceRecovery, pacerRecovery []time.Duration
	var records []map[string]any
	var slow, explained, sourceCensored, pacerCensored int
	for id, admitted := range m.admitted {
		ready, rok := m.ready[id]
		before, bok := m.before[id]
		if !rok || !bok || before.Before(ready) || admitted.Before(before) {
			continue
		}
		wait := bridgeStallOverlap(ready, before, stalls)
		lo, up := before.Sub(ready), admitted.Sub(ready)
		adjustedLower = append(adjustedLower, lo-wait)
		adjustedUpper = append(adjustedUpper, up-wait)
		if wait > 0 {
			cohortLower = append(cohortLower, lo)
			cohortUpper = append(cohortUpper, up)
			direct = append(direct, wait)
		}
		if up > 150*time.Millisecond {
			slow++
			if up-wait <= 150*time.Millisecond {
				explained++
			}
		}
	}
	for _, s := range stalls {
		limit := sourceEnd
		if !s.CensoredAt.IsZero() && s.CensoredAt.Before(limit) {
			limit = s.CensoredAt
		}
		record := map[string]any{"start_before_source_end_s": sourceEnd.Sub(s.Start).Seconds(), "source_baseline": s.SourceBaseline, "pacer_baseline": s.PacerBaseline}
		for _, kind := range []string{"source", "pacer"} {
			recovered := s.SourceRecovered
			if kind == "pacer" {
				recovered = s.PacerRecovered
			}
			ok := !s.End.IsZero() && !recovered.IsZero() && !recovered.After(limit)
			record[kind+"_recovered"] = ok
			if ok {
				duration := recovered.Sub(s.End)
				record[kind+"_recovery_ms"] = float64(duration) / float64(time.Millisecond)
				if kind == "source" {
					sourceRecovery = append(sourceRecovery, duration)
				} else {
					pacerRecovery = append(pacerRecovery, duration)
				}
			} else {
				from := s.End
				if from.IsZero() {
					from = s.Start
				}
				record[kind+"_censor_ms"] = float64(max(time.Duration(0), limit.Sub(from))) / float64(time.Millisecond)
				if kind == "source" {
					sourceCensored++
				} else {
					pacerCensored++
				}
			}
		}
		if !s.End.IsZero() {
			record["actual_pause_ms"] = float64(s.End.Sub(s.Start)) / float64(time.Millisecond)
		}
		records = append(records, record)
	}
	quantiles := func(v []time.Duration) []float64 {
		if len(v) == 0 {
			return nil
		}
		return []float64{bridgeQuantile(v, .5), bridgeQuantile(v, .95), bridgeQuantile(v, 1)}
	}
	return map[string]any{"records": records, "source_recovery_ms": quantiles(sourceRecovery), "pacer_recovery_ms": quantiles(pacerRecovery), "source_recovery_censored": sourceCensored, "pacer_recovery_censored": pacerCensored, "direct_wait_ms": quantiles(direct), "cohort_packets": len(direct), "cohort_lower_ms": quantiles(cohortLower), "cohort_upper_ms": quantiles(cohortUpper), "overlap_subtracted_lower_ms": quantiles(adjustedLower), "overlap_subtracted_upper_ms": quantiles(adjustedUpper), "over150_packets": slow, "over150_explained_by_direct_wait": explained}
}
func TestBridgeStallRecoverySeparatesSourceAndPacer(t *testing.T) {
	base := time.Now()
	m := &bridgeStallObserver{}
	m.sample(base, 5, 5)
	m.begin(base.Add(time.Millisecond))
	m.end(base.Add(151 * time.Millisecond))
	m.release(base.Add(152 * time.Millisecond))
	m.sample(base.Add(160*time.Millisecond), 10, 10)
	m.sample(base.Add(180*time.Millisecond), 5, 8)
	m.sample(base.Add(280*time.Millisecond), 5, 5)
	m.sample(base.Add(380*time.Millisecond), 5, 5)
	s := m.copy()[0]
	require.Equal(t, base.Add(280*time.Millisecond), s.SourceRecovered)
	require.Equal(t, base.Add(380*time.Millisecond), s.PacerRecovered)
	require.Equal(t, 100*time.Millisecond, bridgeStallOverlap(base.Add(51*time.Millisecond), base.Add(251*time.Millisecond), m.copy()))
}

func TestBridgeStallSubtractionIsPerPacketAndPreservesRawLatency(t *testing.T) {
	base := time.Now()
	m := &bridgePacketTiming{ready: map[uint32]time.Time{1: base, 2: base.Add(100 * time.Millisecond), 3: base.Add(210 * time.Millisecond)}, before: map[uint32]time.Time{1: base.Add(250 * time.Millisecond), 2: base.Add(250 * time.Millisecond), 3: base.Add(220 * time.Millisecond)}, admitted: map[uint32]time.Time{1: base.Add(260 * time.Millisecond), 2: base.Add(260 * time.Millisecond), 3: base.Add(230 * time.Millisecond)}}
	stalls := []bridgeStallObservation{{Start: base.Add(50 * time.Millisecond), End: base.Add(200 * time.Millisecond)}}
	summary := m.stallSummary(stalls, base.Add(time.Second))
	require.Equal(t, []float64{60, 110, 110}, summary["overlap_subtracted_upper_ms"])
	require.Equal(t, 2, summary["over150_explained_by_direct_wait"])
	_, raw, _ := m.paired()
	require.Equal(t, 260.0, bridgeQuantile(raw, .95), "diagnostic subtraction must never change the raw acceptance distribution")
}
