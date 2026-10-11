package callharness

import "time"

// framePacer follows delivered ticker deadlines; dropped deadlines are
// counted separately instead of turning into permanent send lateness.
type framePacer struct {
	period           time.Duration
	start, scheduled time.Time
	sent, skipped    uint64
	kind             string
	observe          func(string, uint64, uint64)
}

func (p *framePacer) sentFrame() {
	p.sent++
	if p.observe != nil {
		p.observe(p.kind, 1, 0)
	}
}

func (p *framePacer) next(tick time.Time) {
	// Ticker deadlines are multiples of period. Round away the tiny difference
	// between ticker creation and the immediate first sample's timestamp.
	dropped := max(int64((tick.Sub(p.scheduled)+p.period/2)/p.period)-1, 0)
	p.skipped += uint64(dropped)
	if p.observe != nil && dropped > 0 {
		p.observe(p.kind, 0, uint64(dropped))
	}
	p.scheduled = tick
}

func (p *framePacer) finish(end time.Time) {
	if p.observe == nil {
		return
	}
	// Also count a stall that lasts through the media deadline, before another
	// delivered tick can expose it. Cancelled runs count only elapsed time.
	at := time.Now()
	if end.Before(at) {
		at = end
	}
	elapsed := max(at.Sub(p.start), 0)
	expected := uint64((elapsed + p.period - 1) / p.period)
	if expected > p.sent+p.skipped {
		p.observe(p.kind, 0, expected-p.sent-p.skipped)
	}
}
