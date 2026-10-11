package callharness

import (
	"context"
	"errors"
	"runtime"
	"time"
)

// LongRunOptions schedules caller/operator events while the normal SendMedia
// path runs. Event receives a zero-based event index; nil means no events.
// HeapAfterGC is for isolated memory checks, and pauses this Go process at each
// sample. Heap numbers describe the whole process, not just the recorder.
type LongRunOptions struct {
	Duration, EventEvery, SampleEvery time.Duration
	Event                             func(context.Context, int) error
	HeapAfterGC                       bool
	ObserveMemory                     func(MemorySample)
}

type MemorySample struct {
	At        time.Duration
	HeapAlloc uint64
	Recording RecordingReport
}

// LongRunReport retains at most 256 memory samples, plus the normal per-event
// caller summaries. SamplesDropped records earlier samples rolled away.
type LongRunReport struct {
	Call           *Report
	Memory         []MemorySample
	SamplesDropped int
}

// SendLongRun runs one long call, schedules events, samples its memory, then
// hangs up. Dial with RecordingOptions{History:15*time.Second, DecodeEvery:30}
// to select bounded online sampling. Cancellation still closes the call.
func (c *Call) SendLongRun(ctx context.Context, o LongRunOptions) (*LongRunReport, error) {
	if o.Duration <= 0 || (o.Event != nil && o.EventEvery <= 0) {
		return nil, errors.New("callharness: long run needs duration and positive event interval")
	}
	if c.video != nil && c.rec.recording.DecodeEvery <= 0 {
		return nil, errors.New("callharness: long video run requires Recording.DecodeEvery > 0")
	}
	if o.SampleEvery <= 0 {
		o.SampleEvery = time.Minute
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	start := time.Now()
	sent := make(chan error, 1)
	go func() { sent <- c.SendMedia(run, o.Duration) }()
	samples := time.NewTicker(o.SampleEvery)
	defer samples.Stop()
	var events *time.Ticker
	var eventC <-chan time.Time
	if o.Event != nil {
		events = time.NewTicker(o.EventEvery)
		eventC = events.C
		defer events.Stop()
	}
	rep := &LongRunReport{}
	sample := func() {
		if o.HeapAfterGC {
			runtime.GC()
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		point := MemorySample{At: time.Since(start), HeapAlloc: m.HeapAlloc, Recording: c.Recording()}
		if len(rep.Memory) == 256 {
			copy(rep.Memory, rep.Memory[1:])
			rep.Memory = rep.Memory[:255]
			rep.SamplesDropped++
		}
		rep.Memory = append(rep.Memory, point)
		if o.ObserveMemory != nil {
			o.ObserveMemory(point)
		}
	}
	event := 0
	var runErr error
running:
	for {
		select {
		case runErr = <-sent:
			break running
		case <-ctx.Done():
			cancel()
			runErr = <-sent
			break running
		case <-samples.C:
			sample()
		case <-eventC:
			// Leave a complete measurement window before the scheduled hangup.
			if time.Since(start)+c.rec.freshnessPolicy.RecoveryLimit+c.rec.freshnessPolicy.StabilityWindow+contentSettleWindow >= o.Duration {
				eventC = nil
				continue
			}
			if err := o.Event(run, event); err != nil {
				cancel()
				runErr = errors.Join(err, <-sent)
				break running
			}
			event++
		}
	}
	sample()
	hangup, cancelHangup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelHangup()
	var err error
	rep.Call, err = c.Hangup(hangup)
	return rep, errors.Join(runErr, err)
}
