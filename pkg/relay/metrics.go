package relay

import "github.com/relaishq/relais/pkg/metrics"

func (r *Relay) metricSamples() []metrics.Sample {
	s := r.Stats()
	enabled := 0
	if r.buffer != nil {
		enabled = 1
	}
	return []metrics.Sample{
		metrics.Gauge("relais_relay_buffer_enabled", "Caller replay buffer enabled in the running relay configuration.", enabled),
		metrics.Counter("relais_relay_private_udp_bytes_out_total", "UDP payload bytes forwarded to workers, including relay headers.", r.callerBytes.Load()),
		metrics.Counter("relais_relay_public_udp_bytes_out_total", "UDP payload bytes forwarded to callers.", r.workerBytes.Load()),
		metrics.Gauge("relais_relay_buffer_sessions", "Sessions with retained caller replay metadata.", s.BufferedSessions),
		metrics.Gauge("relais_relay_buffer_packets", "Caller media packets currently in the replay ring.", s.BufferedPackets),
		metrics.Gauge("relais_relay_buffer_bytes", "Accounted ring bytes including headers and packet overhead.", s.BufferedBytes),
		metrics.Counter("relais_relay_buffer_drops_total", "Caller packets evicted or rejected at ring byte limits.", s.BufferDrops),
		metrics.Counter("relais_relay_buffer_expired_packets_total", "Caller ring packets evicted by age.", s.BufferExpired),
		metrics.Counter("relais_relay_buffer_untracked_total", "Caller streams rejected at ring metadata limits.", s.BufferUntracked),
		metrics.Counter("relais_relay_replays_total", "Replay gates drained to completion, including counted losses.", s.Replays),
		metrics.Counter("relais_relay_replay_packets_total", "Held packets successfully sent during caller replay.", s.ReplayPackets),
		metrics.Counter("relais_relay_replay_filtered_packets_total", "Caller replay media filtered by inbound floors.", s.ReplayFiltered),
		metrics.Counter("relais_relay_replay_drops_total", "Packets dropped by replay admission or discarded on expiry.", s.ReplayDrops),
		metrics.Counter("relais_relay_replay_send_failures_total", "Replay packets whose private UDP send failed.", s.ReplaySendFailures),
		metrics.Counter("relais_relay_replay_expiries_total", "Replay gates abandoned by timeout or relay shutdown.", s.ReplayExpiries),
		metrics.Counter("relais_relay_planned_releases_total", "Planned holds released against an exact export's consumed input.", s.PlannedReleases),
		metrics.Counter("relais_relay_planned_recovered_total", "Retained caller media an exact planned export had not consumed, sent on release.", s.PlannedRecovered),
		metrics.Counter("relais_relay_planned_filtered_total", "Held caller media an exact planned export had already consumed, not sent.", s.PlannedFiltered),
		metrics.Counter("relais_relay_planned_incomplete_total", "Planned export releases whose retained ring could not cover the receive window.", s.PlannedIncomplete),
		metrics.Counter("relais_relay_planned_release_lock_nanoseconds_total", "Time planned export releases held the forwarding lock.", uint64(s.PlannedReleaseLockTime)), //nolint:gosec // non-negative
		metrics.Gauge("relais_relay_planned_release_lock_max_nanoseconds", "Longest forwarding-lock hold by one planned export release.", int(s.PlannedReleaseLockMax)),
		metrics.Gauge("relais_relay_active_routes", "Confirmed caller routes currently retained.", s.Flows),
		metrics.Gauge("relais_relay_pending_routes", "Caller routes awaiting worker confirmation.", s.PendingFlows),
		metrics.Counter("relais_relay_caller_packets_total", "Caller packets successfully forwarded to workers.", s.CallerPackets),
		metrics.Counter("relais_relay_worker_packets_total", "Worker packets successfully forwarded to callers.", s.WorkerPackets),
		metrics.Gauge("relais_relay_holds", "Sessions currently held for moves or replay.", s.Holds),
		metrics.Gauge("relais_relay_held_packets", "Caller packets currently queued in move or replay holds.", s.HeldPackets),
		metrics.Gauge("relais_relay_held_bytes", "Accounted bytes currently queued in move or replay holds.", s.HeldBytes),
		metrics.Counter("relais_relay_hold_drops_total", "Caller packets dropped at move or replay hold limits.", s.HoldDrops),
		metrics.Counter("relais_relay_hold_expiries_total", "Move or replay holds expired after the coordination deadline.", s.HoldTimeouts),
		metrics.Counter("relais_relay_barrier_resends_total", "Repeated private drain markers sent before the original deadline.", s.BarrierResends),
		metrics.Counter("relais_relay_barrier_timeouts_total", "Pre-export drain barriers that timed out.", s.BarrierTimeouts),
		metrics.Counter("relais_relay_output_drains_total", "Post-export drain markers acknowledged before a planned route move.", s.OutputDrains),
		metrics.Counter("relais_relay_output_drain_timeouts_total", "Post-export drain markers that expired before a planned route move.", s.OutputDrainTimeouts),
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
