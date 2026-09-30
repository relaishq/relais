# Relais: Recent Changes and Engineering Notes

This document records detailed changes and important notes as we evolve the distributed WebRTC media server. It is intended to be updated continuously.

Last updated: 2025-08-20 13:13:49 -0700

## Summary of Additions

- __Per-Track Metadata in Frames__
  - `pkg/storage/storage.go`: `Frame` now includes `TrackID`, `SSRC`, `ClockRate`, `RTPTime`.
  - Populated at ingress from WebRTC tracks; used for routing at egress.

- __Chronological Merge Ordering__
  - `pkg/storage/storage.go`: `Frame` now includes `IngestTime` (server-side timestamp)
  - `pkg/server/signaling.go`: On ingress we set `IngestTime`; on egress (per-track Streams) we merge frames by `IngestTime` to preserve A/V ordering

- __Per-Track Storage Keys in Redis__
  - `pkg/storage/redis.go`: Frames written to per-session and per-track Redis Lists.
  - `session_tracks:{sessionID}` Set maintains known TrackIDs.

- __Efficient Cursored Reads__
  - `ListFramesSince(ctx, sessionID, fromIndex, limit)` in storage interface and backends.
  - Used by egress as polling fallback.

- __Track Subscription and Discovery__
  - `pkg/server/signaling.go`: signaling messages
    - `select_tracks` to subscribe to specific `video`/`audio` TrackIDs
    - `list_tracks` to fetch known TrackIDs
    - Emits `track` events on first inbound RTP for discovery

- __Redis Streams (Low-Latency Tailing)__
  - `pkg/storage/redis.go`
    - Optional Streams write path; session stream `stream:frames:{sessionID}`
    - Per-track streams `stream:frames:{sessionID}:{trackID}`
    - `EnableStreams(enable bool, maxLen int64)` to toggle and trim (approximate MAXLEN)
    - `StreamsEnabled()` to query toggle
    - `ReadStream(ctx, sessionID, lastID, count, block)` blocking `XREAD`
    - `ReadTrackStream(ctx, sessionID, trackID, lastID, count, block)` blocking per-track `XREAD`
  - `pkg/server/signaling.go`
    - Egress tailer prefers Streams when enabled
    - Uses per-track Streams when tracks are selected; otherwise session stream
    - Falls back to `ListFramesSince` when Streams disabled or non-Redis backend
    - Persists and resumes stream cursors per `sessionID`/`participantID` to continue after reconnects
    - Merges per-track batches by `IngestTime` for chronological output

 - __Redis Streams Consumer Groups__
   - `pkg/config/config.go`
     - New config toggles:
       - `storage.redis_streams_group_enable` (bool)
       - `storage.redis_streams_group` (string, default "relais")
       - `storage.redis_streams_consumer` (string)
     - Env equivalents:
       - `RELAIS_STORAGE_REDIS_STREAMS_GROUP_ENABLE`
       - `RELAIS_STORAGE_REDIS_STREAMS_GROUP`
       - `RELAIS_STORAGE_REDIS_STREAMS_CONSUMER`
   - `pkg/storage/redis.go`
     - Lazily ensures group via `XGROUP CREATE MKSTREAM`
     - Reads with `XREADGROUP` and acknowledges with `XACK`
     - Falls back to `XREAD` when group mode disabled
   - `pkg/server/signaling.go`
     - Egress tailer uses group reads when enabled and acks after processing
     - Backward-compatible fallback when disabled
   - `cmd/relais-core/main.go`
     - Wires the above config to storage to enable group mode
- __Core wiring and env toggles__
  - `cmd/relais-core/main.go`
    - `signalingServer.SetStorage(store)`
    - Config-driven Streams and retention (via `pkg/config` + Viper)
      - Storage config fields:
        - `storage.redis_streams_enable` (bool)
        - `storage.redis_streams_maxlen` (int64)
        - `storage.retention_session` (int)
        - `storage.retention_track` (int)
      - Env equivalents (with `RELAIS_` prefix):
        - `RELAIS_STORAGE_REDIS_STREAMS_ENABLE`
        - `RELAIS_STORAGE_REDIS_STREAMS_MAXLEN`
        - `RELAIS_STORAGE_RETENTION_SESSION`
        - `RELAIS_STORAGE_RETENTION_TRACK`

- __Metrics and /metrics endpoint__
  - `pkg/metrics/metrics.go`
    - Prometheus counters/histogram for egress tailing:
      - `relais_egress_reads_total{mode=session|track}`
      - `relais_egress_empty_polls_total`
      - `relais_egress_errors_total{stage}`
      - `relais_frames_forwarded_total{media=video|audio}`
      - `relais_egress_read_seconds` (histogram)
    - Redis operation metrics (storage reliability):
      - `relais_redis_op_seconds{op}` (histogram)
      - `relais_redis_errors_total{op,type}`
      - `relais_redis_retries_total{op}`
  - `cmd/relais-core/main.go`
    - Exposes `GET /metrics` via `promhttp.Handler()`
  - `pkg/server/signaling.go`
    - Instruments stream reads and frame forwarding with the above metrics

- __Structured Logging & Runner Bootstrap__
  - `cmd/*-runner/main.go`
    - Replaced stdlib `log` with structured `pkg/logging` (logrus JSON) across ingress, egress, transform runners
    - All fatal/error logs now include `.WithError(err)` and structured fields (e.g., `plugin_type`)
    - Emit startup info log with fields: `process`, `plugin_type`, `storage.type`, `storage.cluster`, `log.level`
  - `pkg/util/bootstrap/bootstrap.go`
    - New helper `bootstrap.Init(ctx, process, pluginType)` centralizes config loading, logger creation, storage init
    - Prefers Redis Cluster when enabled; falls back to single-node or memory
    - Returns `(logger, store, cfg)` and logs a structured startup event
  - `pkg/buildinfo`
    - Startup log includes `build.version`, `build.commit`, `build.date` when set via `-ldflags`

- __Redis Cluster Support (initial)__
  - `pkg/config/config.go`
    - Added cluster fields: `storage.redis_cluster_enable` (bool), `storage.redis_cluster_addrs` ([]string),
      `storage.redis_password` (string), `storage.redis_db` (int), `storage.redis_prefix` (string)
  - `pkg/storage/redis.go`
    - Switched to `redis.UniversalClient` to support single-node and cluster clients
    - `RedisConfig` extended with `Addrs`, `Cluster` flags
    - Added key hash tags using `{sess:<sessionID>}` to co-locate per-session keys in the same slot
  - `cmd/relais-core/main.go`
    - Builds `storage.RedisConfig` from config and initializes storage accordingly
  - `pkg/storage/redis_cluster_integration_test.go`
    - Guarded integration tests (require `RELAIS_TEST_REDIS_CLUSTER_ADDRS`):
      - Slot affinity via `CLUSTER KEYSLOT` for session-scoped keys
      - Cursor persistence and resume across reads
      - Per-track stream reads only return frames for the selected track

- __Telemetry and Tailing Hardening__
  - `pkg/server/signaling.go`
    - Added lightweight counters and 5s periodic summaries for egress tailer
      - reads, empty polls, frames forwarded, error count
    - Fixed polling fallback loop; preserves context-aware shutdown
  - `pkg/storage/redis.go`
    - Centralized reliability helper `withRetry()` with capped backoff + jitter and Prometheus metrics
    - `PutFrame()` refactored to build a fresh pipeline per attempt and use `withRetry()`
    - XREAD paths instrumented with `relais_redis_op_seconds` and error classification labels

- __Config Validation__
  - `pkg/config/config.go`
    - If Streams enabled and `redis_streams_maxlen <= 0`, default to `10000`
    - Clamp negative `retention_session`/`retention_track` to `0`
    - When `redis_cluster_enable=true` and no addrs provided, fall back to `storage.redis_url`

## Storage Schema (Redis)

- __Lists__
  - Session list: `frames:{sessionID}`
  - Per-track list: `frames:{sessionID}:{trackID}`
  - Trimmed by configurable retention (implementation-level)

- __Sets__
  - Track set per session: `session_tracks:{sessionID}`

- __Streams__
  - Session stream: `stream:frames:{sessionID}`
  - Per-track stream: `stream:frames:{sessionID}:{trackID}`
  - Entries contain JSON field `data` with serialized `Frame`

## Signaling Messages

- __select_tracks__
  - Payload: `{ "video": "<TrackID|empty>", "audio": "<TrackID|empty>" }`
  - Switches egress routing to selected TrackIDs.

- __list_tracks__
  - Payload: `{}`
  - Response: `{ "type": "tracks", "payload": { "tracks": ["<TrackID>", ...] } }`

- __track__ (server → client event)
  - Emitted when a new inbound track starts producing RTP
  - Payload includes discovered `TrackID` and basic metadata

## Runtime Configuration

- __Environment variables__
  - `RELAIS_STORAGE_REDIS_STREAMS_ENABLE`: `1|true|TRUE` enables Redis Streams tailing
  - `RELAIS_STORAGE_REDIS_STREAMS_MAXLEN`: integer approx MAXLEN for stream trimming (default 10000)
  - `RELAIS_STORAGE_RETENTION_SESSION`: integer trim length for session list
  - `RELAIS_STORAGE_RETENTION_TRACK`: integer trim length for per-track list
  - `RELAIS_STORAGE_REDIS_CLUSTER_ENABLE`: `1|true|TRUE` enables Redis Cluster client
  - `RELAIS_STORAGE_REDIS_CLUSTER_ADDRS`: comma-separated `host:port` list of cluster nodes
  - `RELAIS_STORAGE_REDIS_PASSWORD`: optional password
  - `RELAIS_STORAGE_REDIS_DB`: DB index for single-node client (ignored in cluster mode)
  - `RELAIS_STORAGE_REDIS_PREFIX`: optional key prefix

  - `RELAIS_STORAGE_REDIS_STREAMS_GROUP_ENABLE`: enables Streams consumer groups
  - `RELAIS_STORAGE_REDIS_STREAMS_GROUP`: consumer group name (default "relais")
  - `RELAIS_STORAGE_REDIS_STREAMS_CONSUMER`: consumer name (optional)
- __Test guard__
  - `RELAIS_TEST_REDIS_CLUSTER_ADDRS`: enables cluster integration tests when set (comma-separated addrs)

- __Notes__
  - If Streams disabled or storage is not Redis, egress uses `ListFramesSince` polling.
  - Cluster tests are skipped when the test guard variable is unset.
  - Retention configuration is implementation-level; may be surfaced later via config.

## Operational Notes

- Streams-based egress uses blocking `XREAD` to reduce tail latency and CPU.
- Per-track Stream reads are used when the client selected tracks; otherwise the session stream is tailed.
- Tailer now supports clean shutdown via context cancellation when the PeerConnection closes/fails.
- Stream cursors are persisted in Redis keys `stream_cursor:{sessionID}:{participantID}:{name}` for resume.
- Current error handling: minimal backoff and loop continuation; structured logging improvements are recommended (see Next Actions).
 - Dev: local Redis Cluster compose at `dev/redis-cluster/docker-compose.yml` and guide `docs/dev/redis-cluster.md`.

## Known Limitations / Open Items

- Consumer groups supported behind config; dedicated tests added. Still consider per-connection consumer naming strategy and operational metrics.
- Media plane still basic; samples written directly (consider RTP packetization and pacing by timestamps).
- Security and ops (auth, TURN) pending.
- Retry/backoff and error classification for Redis ops can be centralized and improved.
- Structured logging (zap/slog) and expanded metrics (latencies, errors) are planned.

## Next Actions (Planned)

1. __Consumer groups__
   - Add integration tests, ack/processing metrics, and operational docs; consider per-participant consumer naming.
2. __Redis reliability__
   - Centralize retries/backoff and error classification; surface MOVED/ASK telemetry.
3. __Observability__
   - Structured logging (zap/slog) and expanded metrics (error types, tail latency per mode).
4. __Testing & E2E__
   - Expand signaling/E2E coverage, failure injection, and reconnection paths.
4. __Media plane & control plane__
   - Add SFU features (RTP routing, simulcast, NACK/PLI/TWCC, pacing) and control plane APIs.
5. __Consumer groups__
   - Add integration tests, ack/processing metrics, and operational docs; consider per-participant consumer naming.

## Change Log (chronological)
 
 - 2025-08-20
   - Added Redis Streams consumer group edge-case integration tests (XCLAIM MinIdle; auto group creation) and fixed a test hang with short timeouts/blocks.
   - Updated README with Streams + Consumer Groups configuration and test instructions.
   - Added Makefile shortcuts for running Streams/Groups and Cluster tests.
   - Added Prometheus metrics for Redis Streams consumer group operations in `pkg/metrics/metrics.go` and instrumentation in `pkg/storage/redis.go`:
     - `relais_redis_group_reads_total`, `relais_redis_group_read_messages_total`
     - `relais_redis_group_acks_total`, `relais_redis_group_ack_messages_total`
     - `relais_redis_group_ensure_total{result}`
     - `relais_redis_group_claims_total{result}`, `relais_redis_group_claim_messages_total`
   - Added `ClaimPending(ctx, sessionID, consumer, minIdle, ids...)` helper wrapping `XCLAIM` with metrics.
   - CI: setup-go now uses `go-version-file: go.mod`; added gated self-hosted Redis Cluster test job conditioned on `RELAIS_TEST_REDIS_CLUSTER_ADDRS`.
   - README: Added a concise Metrics Guide with endpoint, key metric families, and a sample Prometheus scrape config.
  - README: Added links to Grafana dashboard and Prometheus alert rules under Metrics Guide.
  - Observability: Added Grafana dashboard JSON for Redis Streams consumer groups at `docs/observability/grafana/redis-groups.json`.
  - Observability: Added Prometheus alert rules at `docs/observability/alerts/redis-groups.yml` (ensure group errors, claim errors, elevated empty claims).
  - Middleware: Fixed HTTP metrics middleware panic handling to ensure 500 is recorded via the status recorder and removed variable shadowing (`pkg/metrics/http.go`).
  - Tests: Refactored Streams consumer group integration tests to use `RedisStorage.ClaimPending()` instead of direct `XCLAIM` calls.
  - CI: Updated `cluster-tests` job gating to use a job-level `env` and `if: ${{ env.RELAIS_TEST_REDIS_CLUSTER_ADDRS != '' }}` to satisfy Actions context linting while preserving behavior.
  - Docs: Added observability setup guide at `docs/observability/README.md` and linked from main `README.md` under Metrics Guide.
 - 2025-08-17
  - Added Redis Streams consumer group support (config: group enable/name/consumer; storage: XGROUP/XREADGROUP/XACK; signaling: acks; core wiring).
  - Backward-compatible fallback to non-group streams remains enabled when group mode is off.
- 2025-08-12
  - Added Prometheus metrics package and instrumentation; exposed `/metrics` endpoint.
  - Expanded Redis Cluster integration tests: slot affinity, cursor resume, per-track reads.
  - Added local Redis Cluster docker-compose and dev guide for running tests.
  - Egress tailer telemetry and minimal backoff improvements.
  - Storage reliability: centralized retry wrapper with metrics; instrumented XREAD and pipeline exec durations/errors.
- 2025-08-10
  - Added Redis Streams write/read support and egress integration.
  - Added per-track Streams read path when tracks are selected.
  - Wired storage into signaling; added env toggles in core.
  - Added context-aware shutdown for egress tailer.
  - Added stream cursor persistence and resume.
