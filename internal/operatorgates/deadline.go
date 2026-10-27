package operatorgates

import (
	"errors"
	"strconv"
	"time"
)

const GateMaximum = 20 * time.Minute
const MutationWindow = 103 * time.Minute
const SessionMaximum = 105 * time.Minute
const PoolReserve = 2 * time.Minute
const WorkerEmptyWindow = 90 * time.Second
const WorkerEmptyReserve = 35 * time.Second

// Clock separates wall time from elapsed time. Wall steps cannot extend a budget.
type Clock interface {
	Wall() time.Time
	Elapsed() time.Duration
}
type RealClock struct{ start time.Time }

func NewRealClock() *RealClock              { return &RealClock{start: time.Now()} }
func (c *RealClock) Wall() time.Time        { return time.Now() }
func (c *RealClock) Elapsed() time.Duration { return time.Since(c.start) }

// FakeClock is event-driven and belongs to the replay loop, never the adapter.
type FakeClock struct {
	Now  time.Time
	Tick time.Duration
}

func (c *FakeClock) Wall() time.Time         { return c.Now }
func (c *FakeClock) Elapsed() time.Duration  { return c.Tick }
func (c *FakeClock) Advance(d time.Duration) { c.Now = c.Now.Add(d); c.Tick += d }

type Budget struct {
	clock   Clock
	end     time.Duration
	wallEnd int64
	armed   int64
}

func NewBudget(clock Clock, armed, end string) (Budget, error) {
	a, e1 := strconv.ParseInt(armed, 10, 64)
	e, e2 := strconv.ParseInt(end, 10, 64)
	if e1 != nil || e2 != nil || a < 0 || a > clock.Wall().UnixMilli() || e < a || e-a > SessionMaximum.Milliseconds() {
		return Budget{}, errors.New("invalid-budget")
	}
	remaining := max(int64(0), e-clock.Wall().UnixMilli())
	return Budget{clock: clock, end: clock.Elapsed() + time.Duration(remaining)*time.Millisecond, wallEnd: e, armed: a}, nil
}
func (b Budget) Remaining() time.Duration { return max(time.Duration(0), b.end-b.clock.Elapsed()) }
func (b Budget) GateEnd() (time.Duration, bool) {
	if b.Remaining() <= 0 {
		return b.end, false
	}
	return min(b.end, b.clock.Elapsed()+GateMaximum), true
}
func (b Budget) PoolEnd() time.Duration { return min(b.end, b.clock.Elapsed()+PoolReserve) }
func (b Budget) MutationEnd() time.Duration {
	return b.end - time.Duration(max(int64(0), b.wallEnd-(b.armed+MutationWindow.Milliseconds())))*time.Millisecond
}
func (b Budget) Wire(end time.Duration) (string, int64) {
	end = min(end, b.end)
	remaining := max(int64(0), (end - b.clock.Elapsed()).Milliseconds())
	absolute := b.wallEnd - (b.end - end).Milliseconds()
	return strconv.FormatInt(absolute, 10), remaining
}
