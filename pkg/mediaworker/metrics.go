package mediaworker

import (
	"sync/atomic"

	"github.com/relais/pkg/metrics"
)

// Lifetime observations survive local session removal and are never restored
// from snapshots. This prevents counting a moved session's packets twice.
type workerMetrics struct {
	packetsIn, packetsOut, decryptFailures atomic.Uint64
	handovers, takeovers                   atomic.Uint64
	handoverErrors, takeoverErrors         atomic.Uint64
}

func (w *Worker) metricSamples() []metrics.Sample {
	return []metrics.Sample{
		metrics.Gauge("relais_worker_active_sessions", "Sessions currently registered on this worker.", w.SessionCount()),
		metrics.Counter("relais_worker_packets_in_total", "Caller datagrams read by this worker, excluding private relay barriers.", w.metrics.packetsIn.Load()),
		metrics.Counter("relais_worker_packets_out_total", "Caller datagrams successfully sent by this worker.", w.metrics.packetsOut.Load()),
		metrics.Counter("relais_worker_decryption_failures_total", "Caller SRTP and SRTCP decryption failures seen by this worker.", w.metrics.decryptFailures.Load()),
		metrics.Counter("relais_worker_handovers_resumed_total", "New session adoptions with zero crash sequence margin, including rollback.", w.metrics.handovers.Load()),
		metrics.Counter("relais_worker_takeovers_resumed_total", "New session adoptions with a crash sequence margin.", w.metrics.takeovers.Load()),
		metrics.Counter("relais_worker_handover_resume_errors_total", "Failed zero-margin resume requests; retries are separate attempts.", w.metrics.handoverErrors.Load()),
		metrics.Counter("relais_worker_takeover_resume_errors_total", "Failed crash-margin resume requests; retries are separate attempts.", w.metrics.takeoverErrors.Load()),
	}
}
