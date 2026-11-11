package operatorgates

import (
	"time"

	"github.com/relaishq/relais/internal/operatorgates/clocks"
	"github.com/relaishq/relais/internal/operatorgates/inventory"
	"github.com/relaishq/relais/pkg/operator"
)

// ClockTiming states the hosts' clock poll period (P) and the p99 latency of
// one host read action through the configured transport. The zero value is
// the reference timing: a one-second poll and local 400 ms reads.
type ClockTiming struct {
	PollSeconds       int64 `json:"pollSeconds"`
	HostReadLatencyMS int64 `json:"hostReadLatencyMs"`
}

// timingBudget derives every lease-read and drain time from one poll period
// and one host read latency. A session keeps one budget for its lifetime.
type timingBudget struct{ poll, read time.Duration }

// Warm local reads measured 300–390 ms; four sequential reads 1.2–1.57 s.
var referenceTiming = timingBudget{poll: time.Second, read: 400 * time.Millisecond}

// A first read also starts its host process: 526 ms was the local maximum.
const leaseProcessStart = 150 * time.Millisecond

// The drain and the judge share PoolReserve. The drain never has less than
// ten seconds; the judge and every gate refresh never have less than a minute.
const drainMinimum = 10 * time.Second
const judgeReserveMinimum = time.Minute

func hostReads() time.Duration { return time.Duration(len(inventory.Slots)) }

// leaseReadMinimum is the time one serial lease read needs after queueing.
func (b timingBudget) leaseReadMinimum() time.Duration { return b.read + leaseProcessStart }

// leaseBatchMinimum charges a batch the service time of its four host reads.
func (b timingBudget) leaseBatchMinimum() time.Duration { return hostReads() * b.read }

// leaseAttempt bounds a whole observation (serial or batch), including
// send-lock queueing. Two attempts and their polls fit inside the 10P refresh.
func (b timingBudget) leaseAttempt() time.Duration { return hostReads()*b.read + b.poll/5 }

// drainLimit lets the trailing coverage read wait two poll periods for the
// closing rows and then read every host once.
func (b timingBudget) drainLimit() time.Duration {
	return max(drainMinimum, 2*b.poll+hostReads()*b.read)
}

// judgeReserve is the part of PoolReserve the drain never uses.
func (b timingBudget) judgeReserve() time.Duration { return PoolReserve - b.drainLimit() }

// budget refuses a configuration that cannot fit the existing deadlines.
// restoreReads is the number of host read actions before a restoration
// command: one batch or four serial reads for inventory, and the same for clocks.
func (t ClockTiming) budget(restoreReads int) (timingBudget, error) {
	if t == (ClockTiming{}) {
		return referenceTiming, nil
	}
	refuse := func(cause string) (timingBudget, error) {
		return timingBudget{}, &clocks.BindingError{Cause: cause}
	}
	if t.PollSeconds <= 0 || t.HostReadLatencyMS <= 0 || t.PollSeconds > int64(PoolReserve/time.Second) || t.HostReadLatencyMS > PoolReserve.Milliseconds() {
		return refuse("clock-timing-invalid")
	}
	b := timingBudget{poll: time.Duration(t.PollSeconds) * time.Second, read: time.Duration(t.HostReadLatencyMS) * time.Millisecond}
	// A lease is valid for five poll periods. It must outlast five reads, or a
	// read and its retry leave no validity for the fault they protect.
	if validity := time.Duration(clocks.LeaseValidityPolls) * b.poll; validity < 5*b.read {
		return refuse("clock-timing-lease-validity")
	}
	if b.judgeReserve() < judgeReserveMinimum {
		return refuse("clock-timing-judge-reserve")
	}
	// RestoreReserve holds these reads, then the host submission floor plus one
	// second for rounding and transit.
	if time.Duration(restoreReads)*b.read+operator.HostSubmissionMinimum+time.Second > RestoreReserve {
		return refuse("clock-timing-restore-reserve")
	}
	return b, nil
}

// CheckClockTiming is the check before a session starts. Capabilities are not
// known yet, so it assumes batch reads; Run checks again with the session's own.
func (c ResourceConfig) CheckClockTiming() error {
	_, err := c.ClockTiming.budget(2)
	return err
}

func (s *Session) restoreReads() int {
	reads := 2 * len(inventory.Slots)
	for _, op := range []operator.Operation{operator.HostReadBatch, operator.HostLeaseReadBatch} {
		if s.capabilities[op] {
			reads -= len(inventory.Slots) - 1
		}
	}
	return reads
}
