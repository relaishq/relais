package controlplane

import (
	"sync/atomic"

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
	p.mu.Unlock()
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
