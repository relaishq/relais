package callharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"
)

var ErrClockEvidence = errors.New("invalid clock evidence")

// OffsetInterval is server minus caller time. Bounds are inclusive. Durations
// and media gaps always stay on the caller's monotonic clock.
type OffsetInterval struct {
	Lower time.Duration `json:"lower_ns"`
	Upper time.Duration `json:"upper_ns"`
}

func (i OffsetInterval) Valid() bool { return i.Lower <= i.Upper }
func (i OffsetInterval) Compose(other OffsetInterval) (OffsetInterval, error) {
	if !i.Valid() || !other.Valid() {
		return OffsetInterval{}, errors.New("invalid clock interval")
	}
	add := func(a, b time.Duration) (time.Duration, bool) {
		if b > 0 && a > time.Duration(math.MaxInt64)-b || b < 0 && a < time.Duration(math.MinInt64)-b {
			return 0, false
		}
		return a + b, true
	}
	lower, lok := add(i.Lower, other.Lower)
	upper, uok := add(i.Upper, other.Upper)
	if !lok || !uok {
		return OffsetInterval{}, errors.New("clock interval composition overflow")
	}
	return OffsetInterval{Lower: lower, Upper: upper}, nil
}
func (i OffsetInterval) Widen(elapsed time.Duration, ppm float64) (OffsetInterval, error) {
	if !i.Valid() || math.IsNaN(ppm) || math.IsInf(ppm, 0) || ppm < 0 || ppm >= 1e6 {
		return OffsetInterval{}, errors.New("invalid clock interval or drift")
	}
	width := math.Ceil(math.Abs(float64(elapsed)) * ppm / 1e6)
	if width > float64(math.MaxInt64)/4 {
		return OffsetInterval{}, errors.New("clock drift overflow")
	}
	d := time.Duration(width)
	if i.Lower < time.Duration(math.MinInt64)+d || i.Upper > time.Duration(math.MaxInt64)-d {
		return OffsetInterval{}, errors.New("clock interval widening overflow")
	}
	return OffsetInterval{Lower: i.Lower - d, Upper: i.Upper + d}, nil
}

// TimeInterval is a server event mapped onto the caller timeline.
type TimeInterval struct {
	Earliest time.Time `json:"earliest"`
	Latest   time.Time `json:"latest"`
}

func (i OffsetInterval) Map(server time.Time) TimeInterval {
	return TimeInterval{Earliest: server.Add(-i.Upper), Latest: server.Add(-i.Lower)}
}

type ClockProbe struct {
	CallerSend    time.Time `json:"caller_send"`
	CallerReceive time.Time `json:"caller_receive"`
	ServerReceive time.Time `json:"server_receive"`
	ServerSend    time.Time `json:"server_send"`
}

func (p ClockProbe) Offset() (OffsetInterval, error) {
	// Sub uses monotonic time when present. Round's wall-only subtraction lets
	// us detect a caller wall clock jump during the HTTP request.
	elapsed := p.CallerReceive.Sub(p.CallerSend)
	wall := p.CallerReceive.Round(0).Sub(p.CallerSend.Round(0))
	processing := p.ServerSend.Sub(p.ServerReceive)
	if p.CallerSend.IsZero() || p.ServerReceive.IsZero() || elapsed < 0 || processing < 0 || processing > elapsed || absDuration(wall-elapsed) > time.Millisecond {
		return OffsetInterval{}, errors.New("clock jump or invalid probe ordering")
	}
	i := OffsetInterval{Lower: p.ServerSend.Sub(p.CallerReceive), Upper: p.ServerReceive.Sub(p.CallerSend)}
	if !i.Valid() {
		return OffsetInterval{}, errors.New("empty clock probe interval")
	}
	return i, nil
}
func absDuration(d time.Duration) time.Duration {
	if d == time.Duration(math.MinInt64) {
		return time.Duration(math.MaxInt64)
	}
	if d < 0 {
		return -d
	}
	return d
}

// ProbeClock uses the caller's existing trusted/authenticated HTTP client.
func ProbeClock(ctx context.Context, client *http.Client, endpoint string) (ClockProbe, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ClockProbe{}, err
	}
	p := ClockProbe{CallerSend: time.Now()}
	res, err := client.Do(req)
	if err != nil {
		return p, err
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		ReceivedAt time.Time `json:"received_at"`
		SentAt     time.Time `json:"sent_at"`
	}
	if res.StatusCode != http.StatusOK {
		return p, fmt.Errorf("clock probe: %s", res.Status)
	}
	err = json.NewDecoder(http.MaxBytesReader(nil, res.Body, 4096)).Decode(&body)
	p.CallerReceive = time.Now()
	p.ServerReceive, p.ServerSend = body.ReceivedAt, body.SentAt
	if err != nil {
		return p, err
	}
	if p.ServerReceive.IsZero() || p.ServerSend.IsZero() {
		return p, errors.New("clock probe timestamps unavailable")
	}
	_, err = p.Offset()
	if err != nil {
		err = fmt.Errorf("%w: %v", ErrClockEvidence, err)
	}
	return p, err
}

// ClockTracker retains timestamp probes. A detected jump invalidates the run,
// including earlier events; accepting a later sample cannot repair clock trust.
type ClockTracker struct {
	mu      sync.Mutex
	probes  []ClockProbe
	ppm     float64
	invalid error
	rate    *ClockRateInterval
}

func NewClockTracker(ppm float64) (*ClockTracker, error) {
	if _, err := (OffsetInterval{}).Widen(0, ppm); err != nil {
		return nil, err
	}
	return &ClockTracker{ppm: ppm}, nil
}
func (c *ClockTracker) Add(p ClockProbe) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalid != nil {
		return c.invalid
	}
	next, err := p.Offset()
	if err == nil {
		for _, old := range c.probes {
			elapsed := p.CallerSend.Sub(old.CallerSend)
			wall := p.CallerSend.Round(0).Sub(old.CallerSend.Round(0))
			if elapsed <= 0 || absDuration(wall-elapsed) > time.Millisecond {
				err = errors.New("clock jump or invalid ordering between probes")
				break
			}
			prev, _ := old.Offset()
			// Each offset was observed somewhere within its RTT. Include that
			// anchor uncertainty before deriving a conservative relative rate.
			uncertainty := max(old.CallerReceive.Sub(old.CallerSend), p.CallerReceive.Sub(p.CallerSend))
			// Float subtraction avoids duration overflow for remote wall clocks.
			distance := float64(elapsed) + float64(uncertainty)
			lowerDelta, upperDelta := float64(next.Lower)-float64(prev.Upper), float64(next.Upper)-float64(prev.Lower)
			rate := ClockRateInterval{LowerPPM: lowerDelta / distance * 1e6, UpperPPM: upperDelta / distance * 1e6}
			if elapsed > uncertainty {
				// Negative lower / positive upper use the shortest anchor distance.
				// Overlapping request brackets give no finite measured rate bound.
				shortest := float64(elapsed - uncertainty)
				if lowerDelta < 0 {
					rate.LowerPPM = lowerDelta / shortest * 1e6
				}
				if upperDelta > 0 {
					rate.UpperPPM = upperDelta / shortest * 1e6
				}
				if c.rate == nil {
					observed := rate
					c.rate = &observed
				} else {
					c.rate.LowerPPM = min(c.rate.LowerPPM, rate.LowerPPM)
					c.rate.UpperPPM = max(c.rate.UpperPPM, rate.UpperPPM)
				}
			}
			if rate.LowerPPM > c.ppm || rate.UpperPPM < -c.ppm {
				err = fmt.Errorf("clock jump or observed rate [%g,%g] ppm exceeds stated drift bound %g ppm", rate.LowerPPM, rate.UpperPPM, c.ppm)
				break
			}
		}
	}
	if err != nil {
		c.invalid = err
		return err
	}
	if len(c.probes) >= 4096 {
		c.invalid = errors.New("clock probe capacity exceeded")
		return c.invalid
	}
	c.probes = append(c.probes, p)
	return nil
}

// Map composes frontend-minus-caller probes with host-minus-frontend bounds.
// Intersect all drift-widened samples at the event. A low-RTT earlier probe
// can provide a tighter bound than the nearest probe; repeated measurements
// never assume symmetric one-way latency or divide the full bound by two.
func (c *ClockTracker) Map(server time.Time, host OffsetInterval) (TimeInterval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalid != nil {
		return TimeInterval{}, c.invalid
	}
	if len(c.probes) == 0 || server.IsZero() || !host.Valid() {
		return TimeInterval{}, errors.New("clock mapping unavailable")
	}
	var mapped TimeInterval
	for _, p := range c.probes {
		probe, _ := p.Offset()
		combined, err := probe.Compose(host)
		if err != nil {
			return TimeInterval{}, err
		}
		a := absDuration(server.Add(-combined.Lower).Sub(p.CallerSend))
		b := absDuration(server.Add(-combined.Upper).Sub(p.CallerSend))
		distance := math.Ceil(float64(max(a, b)) / (1 - c.ppm/1e6))
		if distance >= float64(math.MaxInt64) {
			return TimeInterval{}, errors.New("clock anchor distance overflow")
		}
		combined, err = combined.Widen(time.Duration(distance), c.ppm)
		if err != nil {
			return TimeInterval{}, err
		}
		sample := combined.Map(server)
		if mapped.Earliest.IsZero() || sample.Earliest.After(mapped.Earliest) {
			mapped.Earliest = sample.Earliest
		}
		if mapped.Latest.IsZero() || sample.Latest.Before(mapped.Latest) {
			mapped.Latest = sample.Latest
		}
	}
	if mapped.Latest.Before(mapped.Earliest) {
		return TimeInterval{}, errors.New("clock probes disagree after drift widening")
	}
	return mapped, nil
}
func (c *ClockTracker) Probes() []ClockProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ClockProbe(nil), c.probes...)
}

func (c *ClockTracker) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invalid
}

// Invalidate preserves a failed probe as lost clock trust for this run.
func (c *ClockTracker) Invalidate(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalid == nil {
		c.invalid = err
	}
}

// DriftPPM is the immutable relative rate bound used for event mapping.
func (c *ClockTracker) DriftPPM() float64 { return c.ppm }

// ClockRateInterval is the envelope of frontend-minus-caller average rate
// bounds from probe pairs; the relative rate need not stay constant. A WAN
// pair may provide only a wide bound; its midpoint is never treated as a rate.
type ClockRateInterval struct {
	LowerPPM float64 `json:"lower_ppm"`
	UpperPPM float64 `json:"upper_ppm"`
}

func (c *ClockTracker) ObservedRate() *ClockRateInterval {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rate == nil {
		return nil
	}
	rate := *c.rate
	return &rate
}
