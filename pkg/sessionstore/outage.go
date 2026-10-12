package sessionstore

import "time"

// DefaultStoreOutageBudget is retention headroom for established live calls.
// It grants neither ownership nor permission to resume an old checkpoint.
const DefaultStoreOutageBudget = 30 * time.Second

// Headroom covers renewal phase, one in-flight command deadline and recovery margin.
const WorkerLeaseHeadroom = DefaultWorkerRenewInterval + transitionCommandLimit + time.Second
const DefaultWorkerLeaseTTL = WorkerLeaseHeadroom + DefaultStoreOutageBudget
const DefaultWorkerRenewInterval = time.Second

// StoreOutageBudget selects the default for zero or negative values.
func StoreOutageBudget(budget time.Duration) time.Duration {
	if budget <= 0 {
		return DefaultStoreOutageBudget
	}
	return budget
}

// WorkerLeaseTTL leaves the normal lease lifetime as recovery headroom above
// the outage budget. Renewal cadence remains independent of that lifetime.
func WorkerLeaseTTL(budget time.Duration) time.Duration {
	return WorkerLeaseHeadroom + StoreOutageBudget(budget)
}
