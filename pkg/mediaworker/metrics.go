package mediaworker

import (
	"sync/atomic"

	"github.com/relaishq/relais/pkg/metrics"
)

// Lifetime observations survive local session removal and are never restored
// from snapshots. This prevents counting a moved session's packets twice.
type workerMetrics struct {
	agentEvents                                                         atomic.Uint64
	saveCalls, saveWallNS, saveCPUNS, saveCPUUnknown                    atomic.Uint64
	forcedAttempts, forcedSuccesses, forcedFailures, snapshotDebitBytes atomic.Uint64
	exportHTTPPeakBytes                                                 atomic.Uint64
	packetsIn, packetsOut, decryptFailures                              atomic.Uint64
	replayRejections                                                    atomic.Uint64
	earlyPacketDrops                                                    atomic.Uint64
	bytesIn, bytesOut                                                   atomic.Uint64
	handovers, takeovers                                                atomic.Uint64
	handoverErrors, takeoverErrors                                      atomic.Uint64
}

func (w *Worker) metricSamples() []metrics.Sample {
	a := w.AgentStats()
	return []metrics.Sample{
		metrics.Counter("relais_worker_agent_events_total", "V2 Handle invocations including refused callbacks.", w.metrics.agentEvents.Load()),
		metrics.Counter("relais_worker_agent_save_calls_total", "V2 Save invocations, including initial and rejected saves.", w.metrics.saveCalls.Load()),
		metrics.Counter("relais_worker_agent_save_wall_nanoseconds_total", "Wall time inside V2 Save, excluding host serialization.", w.metrics.saveWallNS.Load()),
		metrics.Counter("relais_worker_agent_save_cpu_nanoseconds_total", "Linux thread CPU inside V2 Save, excluding other goroutines and host serialization.", w.metrics.saveCPUNS.Load()),
		metrics.Counter("relais_worker_agent_save_cpu_unavailable_total", "V2 saves without thread CPU evidence; zero CPU is not a measurement on unsupported systems.", w.metrics.saveCPUUnknown.Load()),
		metrics.Counter("relais_worker_agent_forced_checkpoint_attempts_total", "Forced V2 intent checkpoint attempts including failed clock calls.", w.metrics.forcedAttempts.Load()),
		metrics.Counter("relais_worker_agent_forced_checkpoint_successes_total", "Acknowledged forced V2 intent checkpoints.", w.metrics.forcedSuccesses.Load()),
		metrics.Counter("relais_worker_agent_forced_checkpoint_failures_total", "Failed forced V2 intent checkpoints.", w.metrics.forcedFailures.Load()),
		metrics.Counter("relais_worker_agent_snapshot_debit_bytes_total", "Successful V2 snapshot bytes debited; failed writes debit zero.", w.metrics.snapshotDebitBytes.Load()),
		metrics.Gauge("relais_worker_export_http_peak_bytes", "Largest actual encoded export HTTP response body in this worker lifetime.", int(w.metrics.exportHTTPPeakBytes.Load())),
		metrics.Counter("relais_worker_udp_bytes_in_total", "UDP payload bytes received, excluding relay headers.", w.metrics.bytesIn.Load()),
		metrics.Counter("relais_worker_udp_bytes_out_total", "UDP payload bytes sent, excluding relay headers.", w.metrics.bytesOut.Load()),
		metrics.Counter("relais_worker_agent_input_drops_total", "Agent audio inputs dropped by bounds or deadlines.", a.InputDrops),
		metrics.Counter("relais_worker_agent_callback_deadlines_total", "Agent callbacks exceeding their deadline.", a.CallbackDeadlines),
		metrics.Counter("relais_worker_agent_oversized_states_total", "Agent states exceeding their size limit.", a.OversizedStates),
		metrics.Counter("relais_worker_agent_save_failures_total", "Agent serialization failures, excluding checkpoint store failures.", a.SaveFailures),
		metrics.Counter("relais_worker_agent_unknown_versions_total", "Agent states rejected for unknown versions.", a.UnknownVersions),
		metrics.Counter("relais_worker_agent_restore_failures_total", "Agent restore failures.", a.RestoreFailures),
		metrics.Counter("relais_worker_agent_callback_failures_total", "Other terminal agent callback failures.", a.CallbackFailures),
		metrics.Gauge("relais_worker_active_sessions", "Sessions currently registered on this worker.", w.SessionCount()),
		metrics.Counter("relais_worker_packets_in_total", "Caller datagrams read by this worker, excluding private relay barriers.", w.metrics.packetsIn.Load()),
		metrics.Counter("relais_worker_packets_out_total", "Caller datagrams successfully sent by this worker.", w.metrics.packetsOut.Load()),
		metrics.Counter("relais_worker_decryption_failures_total", "Caller SRTP and SRTCP authentication or decryption failures seen by this worker.", w.metrics.decryptFailures.Load()),
		metrics.Counter("relais_worker_replay_rejections_total", "Caller SRTP and SRTCP packets rejected by the replay window as duplicate or too old.", w.metrics.replayRejections.Load()),
		metrics.Counter("relais_worker_early_packet_drops_total", "Oldest caller SRTP and SRTCP packets dropped when the pre-key queue is full.", w.metrics.earlyPacketDrops.Load()),
		metrics.Counter("relais_worker_handovers_resumed_total", "New session adoptions with zero crash sequence margin, including zero-margin rollback.", w.metrics.handovers.Load()),
		metrics.Counter("relais_worker_takeovers_resumed_total", "New session adoptions with a crash sequence margin.", w.metrics.takeovers.Load()),
		metrics.Counter("relais_worker_handover_resume_errors_total", "Failed zero-margin resume requests; retries are separate attempts.", w.metrics.handoverErrors.Load()),
		metrics.Counter("relais_worker_takeover_resume_errors_total", "Failed crash-margin resume requests; retries are separate attempts.", w.metrics.takeoverErrors.Load()),
	}
}
