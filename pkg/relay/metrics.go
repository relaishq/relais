package relay

import "github.com/relais/pkg/metrics"

func (r *Relay) metricSamples() []metrics.Sample {
	s := r.Stats()
	return []metrics.Sample{
		metrics.Gauge("relais_relay_active_routes", "Confirmed caller routes currently retained.", s.Flows),
		metrics.Gauge("relais_relay_pending_routes", "Caller routes awaiting worker confirmation.", s.PendingFlows),
		metrics.Counter("relais_relay_caller_packets_total", "Caller packets successfully forwarded to workers.", s.CallerPackets),
		metrics.Counter("relais_relay_worker_packets_total", "Worker packets successfully forwarded to callers.", s.WorkerPackets),
		metrics.Gauge("relais_relay_holds", "Sessions currently held during planned moves.", s.Holds),
		metrics.Gauge("relais_relay_held_packets", "Caller packets currently queued in planned holds.", s.HeldPackets),
		metrics.Gauge("relais_relay_held_bytes", "Bytes currently queued in planned holds, including relay headers.", s.HeldBytes),
		metrics.Counter("relais_relay_hold_drops_total", "Caller packets dropped because planned hold limits were reached.", s.HoldDrops),
		metrics.Counter("relais_relay_hold_expiries_total", "Planned holds released after the post-barrier deadline.", s.HoldTimeouts),
		metrics.Counter("relais_relay_barrier_timeouts_total", "Pre-export drain barriers that timed out.", s.BarrierTimeouts),
		metrics.Counter("relais_relay_hold_send_failures_total", "Held packets that could not be forwarded on release.", s.HoldSendFailures),
		metrics.Counter("relais_relay_route_writes_total", "Successful persisted route writes.", s.RouteWrites),
		metrics.Counter("relais_relay_route_write_drops_total", "Route writes dropped at the bounded persistence queue.", s.RouteWritesDropped),
		metrics.Counter("relais_relay_route_write_failures_total", "Failed route persistence operations.", s.RouteWritesFailed),
		metrics.Counter("relais_relay_routes_restored_total", "Persisted routes restored at process startup.", s.RoutesRestored),
		metrics.Counter("relais_relay_route_restore_skips_total", "Known route candidates skipped at process startup.", s.RoutesRestoreSkipped),
		metrics.Counter("relais_relay_route_restore_failures_total", "Startup restore attempts with partial or failed reads.", s.RoutesRestoreFailed),
		metrics.Counter("relais_relay_stun_routed_total", "STUN binding requests forwarded to session owners.", s.STUNRouted),
		metrics.Counter("relais_relay_unknown_session_drops_total", "Binding requests dropped for unknown sessions.", s.UnknownSession),
		metrics.Counter("relais_relay_lookup_drops_total", "Binding requests dropped at the bounded lookup queue.", s.LookupsDropped),
		metrics.Counter("relais_relay_lookup_failures_total", "Binding requests dropped after failed owner lookups.", s.LookupsFailed),
		metrics.Counter("relais_relay_flow_rejections_total", "Routes rejected by capacity or stickiness rules.", s.FlowsRejected),
		metrics.Counter("relais_relay_unroutable_drops_total", "Caller packets dropped without a confirmed route.", s.Unroutable),
		metrics.Counter("relais_relay_unknown_worker_drops_total", "Packets dropped from unregistered workers.", s.UnknownWorker),
		metrics.Counter("relais_relay_worker_no_flow_drops_total", "Worker packets dropped without a matching caller route.", s.WorkerNoFlow),
		metrics.Counter("relais_relay_malformed_drops_total", "Worker packets dropped for malformed relay headers.", s.Malformed),
		metrics.Counter("relais_relay_routes_promoted_total", "Caller route candidates confirmed by workers.", s.FlowsPromoted),
		metrics.Counter("relais_relay_routes_evicted_total", "Pending caller routes evicted by bounded limits.", s.FlowsEvicted),
	}
}
