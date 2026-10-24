# ADR 0014: Python bots behind a fenced agent bridge

Status: **Proposed**. Part of [#254](https://github.com/relaishq/relais/issues/254); aligned with [#255](https://github.com/relaishq/relais/issues/255). Code baseline: `7cc1e2f`; preceding proposal: `fd5c88f`.

## Owner summary

Keep WebRTC in Relais. A Python bot changes its transport and a few setup lines. Killing or replacing that bot keeps the caller connected. A replacement restores bounded conversation text from the last checkpoint, meaning a successful fenced store write. A fence rejects work from a replaced owner. An epoch identifies one period of session ownership.

**Who this is for:** indie and startup teams self-hosting Pipecat first; LiveKit/Daily teams later. Established media and bot replacement on a live owner do not need control. Worker moves and crash takeovers still need control today. No new control-independent worker-recovery claim is made.

These are proposed behaviors, not measured results. Conversation recovery covers saved text, not arbitrary Python objects, unfinished speech or exactly-once tool execution.

## Owner decisions and recommendations

“Decided” records Derek's answers via the coordinator on **2026-10-23**. Other rows need owner approval. Recommendations retain each settled value.

| Decision | Status | Recommended value and main tradeoff |
| --- | --- | --- |
| Audience | Decided | Self-hosting Pipecat indie/startup teams first; defer LiveKit/Daily migration work. |
| Benchmark headline | Decided | First: bot crash/redeploy without call drop versus Pipecat SmallWebRTC. Second: conversation survives agent crash versus LiveKit Agents. Compare distinct outcomes. |
| Package home/version line | Decided | `python/relais-agent`, `python/pipecat-relais` here; one tag versions Go binaries and both Python packages. PyPI names `relais-agent`, `pipecat-relais` (`relais` is taken). Coordinated releases cost cross-language CI. |
| Token authority/pool boundary | Decided | Owning worker issues/verifies tokens bound to session, epoch, bot instance. Pool per worker or relay; recommend per-worker pools. No control-only registry. |
| Wire transport (#254 Q1) | Proposed | Private WebSocket, binary Opus and JSON control; simpler setup, but explicit framing/version rules. |
| Dispatch (#254 Q2) | Proposed | Warm processes; runners register with every eligible worker. More sockets than partial registration, but destination assignment and revocation have a defined path. |
| History (#254 Q3) | Proposed | Require split checkpoints; 3,072 B total working, 8,192 B history. Last four exchanges plus bounded summary; visible truncation limits memory/cost. Keep global defaults/ceilings. |
| Destination (#254 Q4) | Proposed | Direct owner connection, static bootstrap directory and store-backed owner lookup. Operators maintain private endpoints. |
| Issuer (#254 Q5) | Decided | Owning worker, including after transfer; never signal/control. |
| Pipecat versions (#254 Q6) | Proposed | One exact conformance-tested Pipecat/PyAV pair initially; widen a tested matrix. No supported range yet. |
| Recovery targets (#254 Q7) | Proposed | Local socket-close kill-to-attached p95 <1 s; hung bot last-application-pong-to-attached p95 ≤3 s with 2 s timeout; SIGTERM handoff ≤2 s. Require warm capacity/reachable dependencies; report first audio separately. |
| Batching latency | Proposed | Added bridge output latency p95 ≤150 ms, measured from encoded packet ready in Python to host pacer admission. Includes batching/task waits; excludes providers/network playout. A host streaming-output effect needs separate approval if batches fail. |
| Lost speech | Proposed | Save completed user text before generation. If speech is lost before a recoverable transcript, ask the caller to repeat. Durable recordings require another design. |
| Tool safety/rollout | Proposed | Runner revocation and bounded permissions below; accept uncertain in-flight effects. Roll bots in batches with one spare session slot per simultaneous handoff; a full-fleet redeploy needs replacement capacity for every active session. |

The release workflow builds and publishes both Python packages and Go binaries from the same tag, with shared fixtures and Apache-2.0 licensing. Add bridge protocol versions and supported tagged Go/Python pairs to [#252's compatibility manifest](https://github.com/relaishq/relais/issues/252). CI detects version drift; preflight refuses unsupported upgrades/rollbacks. Matching release numbers alone prove no compatibility.

## Actor, host additions and task layout

Register built-in `bridge` through `pkg/workerapp`. The actor runs callbacks one at a time. Public `Agent.Handle/Save/Restore`, deadlines and effects stay unchanged. ADR 0010 A/E's deployment/bridge boundary and Deferred streaming-input/PCM exclusion are amended only for this built-in bridge. README's no-runtime-plugin/subprocess limit changes when shipping; arbitrary plugins/subprocess supervision remain excluded.

Two **new host-internal hooks**, absent today, are required:

- **After accepted event:** after `acceptedEvents.accept` publishes state/progress and releases locks, transfer staged input, Control and input-gap notices to a bounded outbound queue. Rejected/rolled-back events publish nothing. A full queue counts/reports a gap; sockets never block callbacks or UDP.
- **After checkpoint acknowledgement:** after `ackSnapshot`, copy the exact captured bridge pair to a bounded acknowledgement queue. Include confirmed uncertain commits. Never acknowledge a newer local pair for an older write. Neither hook does network I/O under host locks.

Network readers cannot mutate actor state. `Save` returns cached accepted checkpoint bytes without contacting Python. `Restore` validates them locally. Test both hooks for isolation, rollback, deadlines and unchanged behavior of Echo, legacy and other v2 agents.

A task runs slow work outside callbacks; ReadOnly work can repeat and incur charges. Use at most two active ReadOnly task handlers per logical turn:

- `bridge.state/v1`: bot checkpoints, turn-start and interruption requests; remains available between utterances. Host-to-bot Control/input-gap/playback notices use the post-accept outbound queue, not another task. Replies from Python use this state task.
- `bridge.audio/v1`: current-turn output, five 20 ms Opus packets per chunk. Each `TaskMessage` returns `Effects.Audio` references to that exact immutable chunk. Never make a task per packet or regenerate different bytes under an old reference.

Each new turn rotates the state task and ends/cancels old tasks. Interruption advances `CancelBefore` and `ClearAudioBefore`; canceled handlers release active slots immediately. Finished-task records remain until normal checkpoint/replay pruning permits removal. Start successors on a later timer event; `Resumed` starts no tasks. Retained metadata pressure backpressures new work; do not raise the four-task cap. Tasks honor cancellation and existing move/retry/unknown-result rules. Exact moves preserve accepted audio and queues through existing `MoveOnlyState`; Save returns only payload-presence/length metadata. Pending candidates share that budget. A crash loses buffered audio.

**First measurement, before building the bridge:** exercise this complete task layout with 50 packets/s, simultaneous checkpoints/control/gaps, rapid interruptions, silent input, full-size state and task-slot pressure. Emissions currently share a 100 ms interval **per task**; 50 separate emits/s cannot work unchanged. Measure the 150 ms proposal, callback/Save CPU and p99 wall time, queue growth, encoded sizes, checkpoint age and caller gaps locally and under race on a free runner. Compare batching with a prototype host streaming-output effect; keep existing callback/media gates.

Python decodes Opus to PCM (raw audio samples), and encodes output to 20 ms Opus with pinned PyAV wheels. Go stays `CGO_ENABLED=0`. Codec state resets after bot replacement. `RelaisTransport`, context helper and runner setup retain the VAD/STT/LLM/TTS pipeline. Nonstandard context needs a serializer; video, DataChannel/RTVI and arbitrary tool-state migration remain excluded.

## Attach, partition and redeploy rules

A runner supervises bots and owns capacity reservations. It registers with every configured worker eligible to own these calls, retaining sockets across session moves. An eligible destination needs a live authenticated registry path; otherwise attachment waits with a typed unavailable result while media continues. This costs more connections than owner-side token reissue; no new reissue endpoint is proposed.

Use operator-provisioned credentials and TLS; allow plaintext only in isolated loopback tests. Reject browser Origins, oversized requests and untrusted redirects. The owner stores a short-lived, random, single-use token bound to session/epoch/bot instance/generation/protocol/expiry. Generation identifies one attachment. Increment it on replacement; revoke predecessor tokens/sockets. Tokens/reservations in a crashed worker die with it, including a crash halfway through attach. The destination re-reserves and issues a fresh `assign` over its registry socket. Runners release unconsumed reservations on registry close; active capacity frees only after stopping/revoking the bot.

Persist runner identity and bot lineage (the permitted instance and generation) before enabling a newly assigned bot's provider/tool starts. A new instance requires a fresh `Store.Get` confirming this owner/epoch, plus successful fenced checkpoint admission of that lineage. Lease observations must be younger than one configured worker lease TTL (34 s default), measured locally; TTL is not proof against a concurrent transfer. Existing lineage reconnects can keep media through store faults; a new instance waits if ownership/commit cannot be verified. No per-packet store call is added.

**Higher-epoch takeover:** after store-authorized adoption, B sends `revoke(session, old_epoch, new_epoch)` to the persisted runner. The runner records a monotonic epoch floor, blocks older assignments/reconnects, stops the old bot's new provider/tool starts, cancels its requests best effort, and acknowledges revocation before B enables bot work. If that bot is alive, reuse its process and reattach to B. A cannot keep it active merely by keeping its socket open. Fence old epochs across every runner listed in committed active lineage before enabling a different runner.

A runner requests provider/tool permission with a nonce and records local request-start time. The owner reads the lease and computes remaining lifetime with the store clock, subtracting I/O time; clock inversion refuses permission. The runner expires permission at request-start plus that lifetime, capped at one TTL; delayed replies cannot extend it. It renews only after another successful ownership observation. On revocation, owner-socket loss or expiry, bots stop new provider/tool calls until reattached and permitted. If revocation cannot be confirmed, B keeps media alive and bot work paused; do not start a second bot blindly. A different runner can proceed only after confirmed revocation or a proven upper bound on old permission expiry. Without either, return typed unavailable and retry; no guessed cross-host deadline. Socket-close timing targets assume this path is reachable.

ADR 0008 deliberately lets partitioned A keep its media session. The relay fence protects caller audio, **not external tools**. A request already sent can finish after revocation; cancellation can fail. Duplicate generation/charges or an uncertain tool effect remain possible around transfer/partition. Every provider/tool start must use the permission guard; bypassing code is outside this guarantee. Apps must use provider idempotency/query semantics. No exactly-once external-effect claim is made. These bot safety waits do not fence live media on a transient store error.

| Event | Continuation |
| --- | --- |
| Bot closes/killed | Revoke generation; keep consent/call. Warm replacement restores last checkpoint, reports `OutcomeUnknown` and restarts recoverable text work under a new turn. Measure kill-to-close and close-to-attached separately. |
| Bot hangs | Application ping every 500 ms; 2 s without a matching pipeline pong revokes permission/attachment. Library pong alone proves insufficient. Report detection and attach times separately. |
| Bot SIGTERM | Confirm a replacement reservation **before release**. Old bot continues until its own deadline while seeking capacity. Within the whole 2 s handoff budget, finish/truncate speech and commit final state; on expiry, stop and expose silence/no-capacity or prior-checkpoint/`OutcomeUnknown` fallback. No fresh-state or successful-handoff claim without evidence. |
| Worker move/drain | Exact export; same bot holds bounded output, receives new owner assignment and token. Failed move can return to source with a fresh generation. |
| Worker crash | Control-driven takeover still passes ADR 0002 age/margin guards. Runner revokes old epoch; same live bot reattaches without restart. Control down delays this takeover. |

`attached` supplies the restored checkpoint sequence. Python discards speculative later state and restarts checkpoint numbering at restored sequence + 1. Acknowledgements identify **(epoch, generation, seq)**; old-socket acknowledgements cannot advance the new bot. Task chunk sequences remain separately governed by `TaskRun.After`.

## Checkpoints, history and capacity

Working bytes contain bridge schema/config revision, lineage, checkpoint sequence, input cursor, turn/phase and pending user text. History contains messages/summary; prompt/tools come from matching config. Never persist credentials, sockets, recordings or codec buffers.

Advertise `limits.working_bytes = 3,072 − reserved bridge metadata bytes`; measure and reserve metadata's worst encoded width. It is not 3,072 B of user text. Oversized pending user text returns typed `user_text_too_large` before generation; preserve prior state and request a shorter/repeated utterance. Never silently clip it.

Keep separate bounded latest-wins pending slots for working-only and history-changing candidates. Each history slot retains a coherent complete working/history pair; dependent working updates wait with it. Never combine unrelated revisions. Superseded candidates get explicit notices. Coalesce working pushes to the 100 ms Save cadence. History candidates wait outside accepted state for write tokens; advertise deferred status and age. Retry admission on quota refill/acknowledgement, including any admission race, without blocking callbacks or changing periodic cadence. Only oversize is a permanent capacity refusal for an otherwise valid candidate; auth/schema/conflict errors still refuse. Store errors are not app Capacity.

Admission checks size and reserved write budget before accepting state. This does **not** remove `admitHistoryUpdate` checks: full transaction/hot/in-flight reservations still precede acceptance. Accepted pairs retain their write reservations. Pending slots consume bounded move memory; exact exports carry them, crashes lose them without a durable acknowledgement. Checkpoint completed user text before generation when automatic restart is required. In-flight LLM/TTS work becomes `OutcomeUnknown`; speech without recoverable text requires repetition.

Use existing `sessionstore.Store`/`ComponentStore` through the checkpoint module, with no Redis types or keys in bridge APIs. Preserve **6,144 B hot**, **70,000 B/s**, **40,960 B burst**, cold/transaction caps, carried quota and forced side-effect barriers. ADR 0012's maximum hot/forced pattern leaves **2,416 B/s** for cold writes; `8,228/2,416 ≈3.4 s` is a full-history estimate before competing costs, not a fixed interval. Burst/actual load change availability; the bridge cannot promise a durable history update each turn. Last-four-plus-summary truncation is explicit, local and byte-bounded; no automatic summarizer call. Keep tool/result pairs together; refuse an indivisible oversized exchange.

[#214](https://github.com/relaishq/relais/issues/214) accepted only 1.3–3.4% full-size headroom. ADR 0011's roughly 200 calls/primary planning ceiling derives from echo evidence (ADR 0012 §5). **Bridged full-size capacity: NOT-RUN.** Slice 1 measures per-call overhead; slice 6 measures full-size churn, simultaneous handoffs, store traffic/CPU and safe calls/primary. Do not publish 200 as bridge capacity or raise ceilings.

## Connections, protocol and local events

Nothing phones home. Runtime connections are allowlisted below. Disable model downloads, proxy autodiscovery, tracing/exporters and update checks; supply VAD assets locally. DNS queries only configured names (A/AAAA); TLS uses configured SNI/certificates. Test denied egress.

| Connection | Application fields |
| --- | --- |
| Runner ↔ every eligible worker, registry WebSocket | TLS identity/Authorization; `hello(protocol, runner_id, bot_instance, agent_name, config_revision, capacity)`; `reserve(session_id, epoch, reservation_id, expires_ms)`; `reserved(reservation_id, accepted)`; `assign(session_id, epoch, attach_token, expires_ms)`; `revoke(session_id, old_epoch, new_epoch)`; `revoked(session_id, new_epoch)`; `permit_request(session_id,epoch,generation,nonce)`; `permit(session_id,epoch,generation,nonce,remaining_ms)`; `ping/pong(nonce)`. |
| Bot → bootstrap worker, HTTPS lookup | Authentication; request `session_id`; reply `owner, epoch, worker_url` from the static operator directory. |
| Bot ↔ owner, session WebSocket | Authentication; `attach(protocol, session_id, epoch, bot_instance, attach_token, last_checkpoint_seq)`; `attached(epoch, generation, resume, checkpoint_seq, checkpoint_age_ms, snapshot_age_ms, working, history, outcome_unknown, duplicate_windows, limits)`. Socket scopes all later messages to epoch/generation. |
| Binary frames | Input `type=1, input_ssrc:u32, input_seq:u64, rtp_ts:u32, duplicate:u8, opus`; output `type=2, turn:u64, seq:u64, opus`. Big-endian integers; remainder is payload. Validate duration/length/sequence. |
| Session JSON | `checkpoint(seq, version, input_ssrc, input_seq, working, history)`; `checkpointed(epoch,generation,seq)`; `superseded(seq)`; `deferred(seq,reason,age_ms)`; `refused(seq,reason)`; `clear_audio(before_turn)`; `cancel_before(turn)`; `playback_finished(turn)`; `control(id,sequence,name,data)`; `moving(owner,epoch)` advisory; `release(final_checkpoint_seq)`; `ping/pong(nonce)`; `input_gap(first_seq,last_seq)`; `outcome_unknown(turn,reason)`. Bytes: base64; duplicate windows: SSRC/first/last. |
| Worker → configured store | Existing lease/clock/fenced component operations: session, owner/epoch, captured bytes/references, quota and acknowledgements. Memory opens no connection; adapter owns wire encoding. |
| Application providers | None in bridge packages. Existing STT/LLM/TTS/tools remain app-owned; each demo adapter documents endpoint, headers/auth, model/settings and audio/text/context/tool fields before release. No provider calls here. |

`limits`: `working_bytes,history_bytes,json_bytes,binary_bytes,input_packets,move_bytes`; `resume`: fresh/planned_move/takeover/bot_restart. Cap JSON at 32 KiB, binary at 1,200 B, input queue at 32 packets; other queues/pending slots share existing move-memory caps. Reserve control capacity. Output waits with cancellation. Reject unknown major/schema versions; negotiate optional fields. Never fall back to Echo.

Stable local events `relais.bridge/v1`: `bot_attached,bot_detached,bot_takeover,bot_refused`. Fields: `event_version,event_id,observed_at,session_id,owner_epoch,generation,cause,outcome,duration_ms,checkpoint_age_ms`. Nullable fields remain explicit; durations use local elapsed time; causes/outcomes are bounded enums. No tokens, audio, history or credentials. Bounded stdout/file delivery counts drops. Optional registration advertisements/extensions/exporters time out without blocking media, Save or checkpoints. Safety-required attachment/revocation can wait. Metrics never label sessions.

## Verifiable implementation slices

1. **Measure task layout:** synthetic audio/state/control/gap path and interruption/slot pressure above; publish raw latency/size/CPU/age evidence. Decide batching before product code.
2. **Hear loopback:** one worker plus Python bot, input/output audio end to end with a static development token. Local-only test mode, refused on non-loopback; no public authentication claim. Test queue limits, post-accept hook and unchanged other-agent behavior.
3. **Move authenticated loopback:** worker-owned tokens, all-worker runner registration, fresh assignments and revocation. Exercise two workers, partitioned A, attach-time crash, stale credentials/epochs and no-capacity; verify blocked old provider/tool starts with a local ledger.
4. **Restore conversation:** checkpointed loopback bot remembers text after bot replacement. Add exact acknowledgement hook, lineage gate, sequence reconciliation, pending-history/working overflow, lost replies and both stores. Test fast turns plus concurrent output; no false durable acknowledgement.
5. **Recover processes:** real bot kill/hang/SIGTERM, fleet redeploy, worker move/drain/crash with unchanged live bot PID. Retain consent/decryption/gap gates; cover unavailable runner and uncertain tools. New short-test cost stays below 15 s.
6. **Adopt and compare:** pinned Pipecat hands-free demo and five-minute guide with Move/Drain/Kill worker/Kill bot/Redeploy. Test transport-plus-setup migration, codec/context/config/truncation fixtures. Benchmark in owner-decided order; publish versions, hardware, counts, p95, gaps/first audio, context and failures. Add full-size capacity/traffic tests; providers need separate authorization.

## Evidence and reference limits

Code checked: `accepted_event.go:265–283` has no post-accept bridge hook; `checkpoint_snapshot.go:386–424` has no bridge acknowledgement observer. `agent_tasks.go:572–599` spaces emissions; `agent_limits.go:239` caps active tasks at four. `accepted_event_tasks.go:172–191` releases canceled slots; `pruneTasks` separately retains receipts. `checkpoint_admission.go:126–155` requires history reservations. `TestPartitionedWorkerStoreTransferHasExclusiveCallerMedia` proves media exclusion while A retains its session, not bot/tool exclusion. Preserve ADRs 0002/0004/0008/0013 ownership/recovery bounds.

Reference correction only: [ADR 0012](0012-agent-state-store-schema.md) and [session-store guide](../dev/session-store.md) retain stale pending-validation wording; #214 records owner acceptance, with new split admission still off by default. Those files are unchanged.

Official sources checked 2026-10-23: [SmallWebRTC](https://docs.pipecat.ai/api-reference/server/services/transport/small-webrtc) describes direct peers; bot-death call loss is an inference to benchmark. Pipecat [frames](https://reference-server.pipecat.ai/en/latest/api/pipecat.frames.frames.html) and [context](https://reference-server.pipecat.ai/en/latest/api/pipecat.processors.aggregators.llm_context.html) support the adapter seams; [PyAV](https://pyav.org/docs/stable/overview/installation.html) documents libopus wheels, requiring pinned validation. LiveKit documents [~15 s redispatch](https://docs.livekit.io/agents/server/lifecycle/), [10 s shutdown hooks](https://docs.livekit.io/agents/server/job/) and [one-hour Cloud drain](https://docs.livekit.io/deploy/agents/managing-deployments/). Apps can persist context before shutdown. No comparative runtime result is claimed.

**NOT-RUN — implementation, recovery/latency targets, Pipecat compatibility and bridge capacity/benchmarks.**
