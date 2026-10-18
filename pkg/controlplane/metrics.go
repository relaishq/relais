package controlplane

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/relaishq/relais/pkg/metrics"
)

type planeMetrics struct {
	calls, callErrors, moves, moveErrors           atomic.Uint64
	takeovers, takeoverErrors, drains, drainErrors atomic.Uint64
	detections, uncertainMoveResumes               atomic.Uint64
}

func (p *Plane) metricSamples() []metrics.Sample {
	p.mu.Lock()
	calls := len(p.calls)
	for _, c := range p.calls {
		if c.cleanupPending {
			calls--
		}
	}
	losses := make([]metrics.Sample, 0, 3)
	for _, cause := range []string{"ownership_expired", "checkpoint_unsafe", "checkpoint_missing", "recovery_failed"} {
		sample := metrics.Counter("relais_control_losses_total", "Definitively lost calls by terminal cause.", p.lossesByCause[cause])
		sample.Labels = prometheus.Labels{"cause": cause}
		losses = append(losses, sample)
	}
	pool := p.poolLocked(time.Now())
	p.mu.Unlock()
	losses = append(losses, poolMetrics(pool)...)
	return append(losses, []metrics.Sample{
		metrics.Gauge("relais_control_active_calls", "Calls currently tracked by the control plane.", calls),
		metrics.Counter("relais_control_calls_total", "Successfully created calls.", p.metrics.calls.Load()),
		metrics.Counter("relais_control_call_errors_total", "Failed call creation requests.", p.metrics.callErrors.Load()),
		metrics.Counter("relais_control_moves_total", "Successfully completed planned moves, including drains and retained retries.", p.metrics.moves.Load()),
		metrics.Counter("relais_control_uncertain_move_resumes_total", "Planned move target-resume attempts observed with uncertain outcomes; not a count of injected faults or confirmed commits.", p.metrics.uncertainMoveResumes.Load()),
		metrics.Counter("relais_control_move_errors_total", "Failed planned coordination attempts after target selection, including drain moves.", p.metrics.moveErrors.Load()),
		metrics.Counter("relais_control_takeovers_total", "Successfully completed crash takeovers.", p.metrics.takeovers.Load()),
		metrics.Counter("relais_control_takeover_errors_total", "Terminal failed crash takeovers; transient retries are excluded.", p.metrics.takeoverErrors.Load()),
		metrics.Counter("relais_control_drains_total", "Successfully completed worker drain requests.", p.metrics.drains.Load()),
		metrics.Counter("relais_control_drain_errors_total", "Failed or partially failed worker drain requests.", p.metrics.drainErrors.Load()),
		metrics.Counter("relais_control_detection_events_total", "Worker transitions from live to declared dead; recovery retries are excluded.", p.metrics.detections.Load()),
	}...)
}

func poolMetrics(pool PoolSample) []metrics.Sample {
	valid, stale := 0, 0
	if pool.Demand != nil {
		valid = 1
	}
	if pool.Stale {
		stale = 1
	}
	samples := []metrics.Sample{
		metrics.Gauge("relais_control_pool_sample_valid", "Whether a pool observation exists; missing demand is never zero.", valid),
		metrics.Gauge("relais_control_pool_sample_stale", "Whether the pool observation is missing, failed to refresh or too old.", stale),
	}
	samples = append(samples, metrics.Gauge("relais_control_worker_call_cap", "Configured per-worker call budget; zero means unlimited.", pool.Cap))
	for _, reason := range []string{"no_target", "at_capacity"} {
		sample := metrics.Counter("relais_control_refusals_total", "Definite call admission refusals by reason.", pool.Refusals[reason])
		sample.Labels = prometheus.Labels{"reason": reason}
		samples = append(samples, sample)
	}
	// Until an observation exists these values are unavailable, not zero.
	if pool.SampledAt.IsZero() {
		return samples
	}
	if pool.Demand != nil {
		samples = append(samples, metrics.Gauge("relais_control_pool_demand", "Observed calls plus incoming reservations.", *pool.Demand))
	}
	samples = append(samples,
		metrics.Gauge("relais_control_pool_reservations", "Observed incoming placement reservations.", pool.Reservations),
		metrics.Gauge("relais_control_pool_over_cap_calls", "Observed live calls exceeding per-worker budgets after recovery.", pool.OverCapCalls),
		metrics.Gauge("relais_control_pool_never_answered_workers", "Configured workers whose address is still unknown, making aggregate demand unavailable.", pool.NeverAnswered),
	)
	timestamp := metrics.Gauge("relais_control_pool_sample_timestamp_seconds", "Unix timestamp of the pool observation.", 0)
	timestamp.Value = float64(pool.SampledAt.UnixNano()) / 1e9
	samples = append(samples, timestamp)
	age := metrics.Gauge("relais_control_pool_sample_age_seconds", "Seconds since the pool observation.", 0)
	age.Value = pool.AgeSeconds
	samples = append(samples, age)
	for state, n := range map[string]int{"ready": pool.Ready, "draining": pool.Draining, "unreachable": pool.Unreachable, "pending": pool.Pending} {
		sample := metrics.Gauge("relais_control_pool_workers", "Observed workers by placement state.", n)
		sample.Labels = prometheus.Labels{"state": state}
		samples = append(samples, sample)
	}
	return samples
}
