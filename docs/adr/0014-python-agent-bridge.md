# ADR 0014: Python bots behind a fenced agent bridge

Status: **Proposed**. Part of [#254](https://github.com/relaishq/relais/issues/254), aligned with [#255](https://github.com/relaishq/relais/issues/255). Code baseline: `7cc1e2f`; preceding proposal: `0898cb6`.

## What you are approving

“Decided” records Derek's answers through the coordinator on **2026-10-23**. Other rows are recommendations for approval. None authorizes implementation yet.

| Choice | Status | Recommended value and consequence |
| --- | --- | --- |
| Audience | Decided | Indie/startup teams self-hosting Pipecat first. LiveKit/Daily teams come later. |
| Benchmark headline | Decided | First, bot crash/redeploy without call drop versus SmallWebRTC. Second, conversation survives agent crash versus LiveKit Agents. |
| Package home/version line | Decided | `python/relais-agent` and `python/pipecat-relais` stay here. One tag versions Go binaries and both Python packages; coordinated releases need Python CI. |
| PyPI names | Decided | Publish `relais-agent` and `pipecat-relais`; `relais` is taken. |
| Token authority/pool | Decided | The owning worker issues/verifies session/epoch/bot-instance tokens. Pool per worker or relay; recommend per-worker pools. |
| Wire transport (#254 Q1) | Proposed | Private WebSocket with Opus and JSON. Simple setup requires explicit bounds/version rules. |
| Dispatch (#254 Q2) | Proposed | Warm runners register with every eligible worker. More sockets buy a defined reassignment/revocation path. |
| History (#254 Q3) | Proposed | Enable split checkpoints: 3,072 B total working, 8,192 B history. Keep four exchanges plus bounded summary; disclose truncation. |
| Destination (#254 Q4) | Proposed | Connect directly to the owner. Operators maintain private endpoints and a bootstrap directory. |
| Issuer (#254 Q5) | Decided | Current owning worker, including after transfer. Signal/control never mint or verify attach tokens. |
| Pipecat versions (#254 Q6) | Proposed | Start with one tested Pipecat/PyAV pair. Widen only through a tested matrix; no range is supported yet. |
| Recovery targets (#254 Q7) | Proposed | Local socket-close kill-to-attached p95 <1 s; SIGTERM handoff ≤2 s. Warm capacity and reachable dependencies are required. |
| Hang target | Proposed | With a 2 s detector, last-pong-to-attached p95 ≤3 s needs same-runner revocation confirmation. Otherwise wait for permit expiry, then attachment. |
| Bot-permit lifetime | Proposed | Pool-wide 5 s cap, refreshed only by successful existing lease renewals. Partitions cause shorter pauses, but store outages stop bot work sooner than media. |
| Output experiment | Proposed | Approve measurement first; choose batching or a streaming-output effect from its result. Require added output latency p95 ≤150 ms; no winner is selected. |
| Lost speech | Proposed | Save completed user text before generation. If no recoverable transcript exists, ask the caller to repeat; recordings need another design. |
| Tool safety/rollout | Proposed | Accept uncertain in-flight effects; require app idempotency/query support. One extra warm slot per simultaneous handoff permits rolling redeploys. |
| Host changes | Proposed | Add local publication/acknowledgement hooks, generation checks, stable exports and bridge-only playback completion. Keep public v2 signatures and other agents unchanged. |

## Owner summary

Keep WebRTC in Relais. A Python bot changes its transport and a few setup lines. Killing/replacing the bot keeps the caller connected. A replacement restores bounded text from a checkpoint: a successful fenced store write. A fence rejects replaced owners; an epoch identifies one ownership period. A generation identifies one bot attachment within that epoch.

Live media and bot replacement on a surviving owner do not need control. Worker moves/crash takeovers still do. Saved text can survive; unfinished speech, provider streams and arbitrary Python objects cannot. Tools are not exactly-once. These are proposed behaviors, not measured results.

## Recovery and state rules

The owner alone issues short-lived, single-use tokens. Warm runners supervise bots and reserve capacity. They register with every eligible worker, so a destination can issue a fresh assignment after a move/crash. Lost registry access can delay attachment; it never blocks media, Save or checkpoints. Worker-local tokens/reservations die with a crash, including halfway through attach.

Persist bot/runner identity and generation before enabling a new bot's provider/tool starts. A new instance needs fresh ownership verification and a successful fenced lineage checkpoint. Existing-lineage reconnects can preserve media through store faults. Never treat a recent ownership read as proof against a concurrent transfer.

**Every replacement needs revocation**, including replacement within the same epoch. First stop old-generation permit grants. Revoke that exact bot/epoch/generation through its runner. Advance a monotonic generation floor within each epoch; compare epoch/generation together across ownership changes. Match acknowledgements to the full old/new identities and request ID. Stale acknowledgements cannot authorize another replacement.

Without confirmation, same-owner replacement waits until the last possible old-generation permit deadline. Include queued replies and in-flight renewals; a reply cannot reset its request-start deadline. Higher-epoch takeover instead bounds expiry by transfer store-time plus the pool's maximum permit cap. Unknown bounds mean paused bot work and a typed unavailable outcome, not a second active bot. A live bot can reattach to the successor without restarting its process.

Socket closure alone is insufficient. Tag all queued bot work, pending candidates and callbacks with bot instance/epoch/generation. Validate those tags again when applying work. During replacement, atomically advance the floor, cancel obsolete tasks and invalidate old checkpoints, interruptions, audio, controls and notices. Exact moves must not revive them.

Serialize replacement with checkpoint capture/write settlement. Resolve uncertain writes before choosing the replacement's durable conversation and sequence baseline. A timeout is not proof of non-commit. Commit the new lineage against the settled tuple before `attached`; a superseded tuple requires reconciliation again. Preserve current media counters. New checkpoint numbering starts at the restored sequence + 1; old-scope acknowledgements cannot advance it.

**Permits add no store commands/bytes.** Refresh only from the existing successful lease `Renew` for that owner/epoch. Anchor the owner deadline at renewal-request start plus `min(5 s, lease TTL)`, never reply receipt. A runner's nonce request uses cached remaining time, anchored at runner-local request start. Late replies and repeated requests cannot extend it; failed/uncertain renewals grant nothing. Stop new provider/tool calls on revocation, owner-socket loss or expiry. Media retains ADR 0008's outage policy.

For takeover, derive transfer store-time from returned Transfer `ExpiresAt − applied TTL`. Use existing recovery clock observations, or conservatively wait 5 s after local Transfer confirmation. For same-epoch replacement, freeze grants and use the known maximum outstanding deadline, or wait the cap after grant issuance is disabled. Do not use the transfer formula without a transfer. Mixed pools use the maximum known cap; clock/identity uncertainty delays activation.

The relay fences caller audio, not external tools. Requests already sent can finish; cancellation can fail. Every provider/tool start must obey the permit guard. Apps that bypass it are outside the guarantee. Local timing targets assume confirmed revocation and warm capacity; permit expiry bounds permission, not total first-audio time.

On SIGTERM, reserve a replacement before release. Old bot continues until its own deadline. Full-fleet replacement needs one spare slot per active session; rolling batches need one per simultaneous handoff. At the 2 s handoff deadline, expose no-capacity/silence or prior-checkpoint/`OutcomeUnknown` fallback. Never report fresh-state success without its acknowledgement.

## Contract, budgets and release

Public `Agent.Handle/Save/Restore` stays serial and deadline-bounded. All Save/Restore and move-only callbacks use owned local state; none waits for Python, socket flush or remote acknowledgement. Exact export uses a stable local snapshot whose payload length matches the last accepted Save. Appendix A defines the cut and playback rule.

Use existing `Store`/`ComponentStore` interfaces. No Redis types or keys enter the bot protocol. Keep 6,144 B hot, 70,000 B/s, 40,960 B burst, existing transaction caps and reservations. Working text gets 3,072 B minus reserved bridge metadata. Oversized user text refuses before generation and requests shorter/repeated input.

Separate latest-wins working/history candidates wait outside the accepted bot pair. Keep each candidate coherent; dependent working updates wait with its history. Budget shortage reports deferred age, not permanent size refusal. Admission still reserves the full write, hot checkpoint and unresolved-write allowance. Pending data is bounded and can be lost on crash without durable acknowledgement. History cannot be promised durable every turn.

ADR 0012's maximum hot/forced pattern leaves 2,416 B/s for cold writes; `8,228/2,416 ≈3.4 s` is conditional arithmetic. [#214](https://github.com/relaishq/relais/issues/214) accepted 1.3–3.4% full-size headroom. Its ~3,155 B/s projection is not guaranteed spare capacity: any extra store I/O must be measured. Permit WebSocket bytes are separate bridge traffic. **Full-size bridge calls/primary: NOT-RUN**; the roughly 200-call echo planning ceiling is not bridge capacity.

Build/publish Go binaries and both Apache-2.0 Python packages from the same tag. Include bridge protocol and supported pairs in [#252's compatibility manifest](https://github.com/relaishq/relais/issues/252); CI/preflight refuse unsupported upgrade/rollback pairs. Matching release numbers alone prove no compatibility.

**Approve the experiment first.** Measure sustained speech, jitter, partial final batches and loaded renewal gaps before selecting output design. Five 20 ms packets exactly fill a 100 ms emission interval, leaving no catch-up margin. Streaming output is another candidate. The 150 ms threshold spans encoded packet ready in Python to host pacer admission, including batching/task waits. Provider and caller playout time are separate.

**NOT-RUN — implementation, safety/timing targets, Pipecat compatibility and bridge capacity/comparisons.**

## Appendix A: Host, task and export details

Register built-in `bridge` through `workerapp`. Amend ADR 0010 A/E's deployment/bridge boundary and Deferred streaming-input/PCM exclusion only for this bridge. Also amend playback completion only as specified below. No runtime Go plugins or subprocess supervision. Update README's limit when shipping.

Two internal hooks publish bounded owned records outside locks:

- After accepted-event publication, enqueue staged input, Control and gaps. Rollback/rejection publishes nothing. Queue overflow counts/reports gaps; no callback/UDP waits.
- After `ackSnapshot`, enqueue the exact acknowledged bridge pair, including settled uncertain commits. Never acknowledge a newer local pair for an older write.

Network readers cannot mutate accepted actor state. Use at most two active ReadOnly handlers per turn: `bridge.state/v1` carries checkpoints, turn/interruption requests and bot replies; `bridge.audio/v1` carries output. Host-to-bot Control, gaps and playback notices use the post-accept queue. ReadOnly work can repeat and incur charges. Chunk sequences obey `TaskRun.After`; audio references name immutable emitted bytes.

For the bridge only, exclude the strictly non-audio state task from playback's busy-task test. It cannot emit audio or hold audio results. Completion still requires an explicit audio end marker, final audio task, consumed audio results/refusals and an empty turn audio queue. The state channel can stay open after `playback_finished`; no next utterance is required. Completion proves local send, not caller hearing. Generic agent completion remains unchanged.

A new turn rotates tasks. Interruptions use `CancelBefore`/`ClearAudioBefore`; cancellation releases active slots, but receipts retain normal checkpoint/replay pruning. Start successors on later timer events; never from `Resumed`. Metadata pressure backpressures work; do not raise four active slots. Measurements compare five-packet batches, variable/partial batches and a host streaming-output prototype without relaxing callback limits or emission spacing.

Bridge queues use an **immutable accepted revision**. Save reports that revision's cached bytes and move-only length, without copying/hashing audio. Async writers read owned views. Delivery receipts/queue removal take effect only through a later accepted actor transaction; rejected transactions restore the prior revision. Pending candidates can be locally buffered without becoming the accepted durable bot pair. Unadmitted ingress is not export state and receives no success acknowledgement.

At exact export, stop admissions and drain mutations locally; freeze one owned revision after the final accepted Save. `SaveMoveOnly` serializes exactly that image and declared length; it is repeatable and non-destructive. It never queries Python or drains a socket. `RestoreMoveOnly` validates/copies local bytes before `Resumed`; no remote wait. Failed export leaves the source revision usable. Count retained images, in-flight writer copies and export scratch against session/worker move budgets.

Every queued item has instance/epoch/generation and a local revision. Check again at task application, candidate acceptance, post-accept delivery and export restoration. A replacement transaction invalidates all old-scope queues/tasks atomically. An exact worker move may explicitly remap a frozen revision only for the same authorized bot lineage. It cannot relabel an obsolete generation after bot replacement. Unfinished generation becomes `OutcomeUnknown`; never regenerate different audio under an old reference.

Checkpoint capture/write settlement keeps its existing serialization and fences. A confirmed old-generation write may supply recoverable conversation, but cannot authorize old queued effects. Resolve its exact pair before publishing new lineage/baseline; new lineage commit prevents late old CAS writes from replacing it. Unresolved store outcomes postpone attachment. Test simultaneous bot restart, delayed commit and worker takeover.

## Appendix B: Connections and protocol

Nothing phones home. Disable model downloads, proxy autodiscovery, tracing/exporters and update checks; supply VAD assets locally. DNS uses configured names (A/AAAA); TLS uses configured SNI/certificates. Reject browser Origins/untrusted redirects. Credentials never enter URLs/logs.

| Connection | Application fields |
| --- | --- |
| Runner ↔ eligible workers, registry WebSocket | TLS identity/Authorization; `hello(protocol,runner_id,bot_instance,agent_name,config_revision,capacity)`; `reserve(session_id,epoch,reservation_id,expires_ms)`; `reserved(reservation_id,accepted)`; `assign(session_id,epoch,bot_instance,generation,attach_token,expires_ms)`; `revoke(session_id,bot_instance,old_epoch,old_generation,new_epoch,new_generation,request_id)`; `revoked(session_id,bot_instance,old_epoch,old_generation,new_epoch,new_generation,request_id)`; `permit_request(session_id,epoch,generation,nonce)`; `permit(session_id,epoch,generation,nonce,remaining_ms)`; `ping/pong(nonce)`. |
| Bot → bootstrap worker, HTTPS lookup | Authentication; request `session_id`; reply `owner,epoch,worker_url` from operator directory. |
| Bot ↔ owner, session WebSocket | Authentication; `attach(protocol,session_id,epoch,bot_instance,generation,attach_token,last_checkpoint_seq)`; `attached(epoch,generation,resume,checkpoint_seq,checkpoint_age_ms,snapshot_age_ms,working,history,outcome_unknown,duplicate_windows,limits)`. Socket binds instance/epoch/generation; copy that scope into every internal queued item. |
| Binary | Input `type=1,input_ssrc:u32,input_seq:u64,rtp_ts:u32,duplicate:u8,opus`; output `type=2,turn:u64,seq:u64,opus`. Big-endian integers; remainder is payload. Validate lengths/durations/sequences. |
| Session JSON | `checkpoint(seq,version,input_ssrc,input_seq,working,history)`; `checkpointed(epoch,generation,seq)`; `superseded(seq)`; `deferred(seq,reason,age_ms)`; `refused(seq,reason)`; `clear_audio(before_turn)`; `cancel_before(turn)`; `audio_end(turn)`; `playback_finished(turn)`; `control(id,sequence,name,data)`; `moving(owner,epoch)` advisory; `release(final_checkpoint_seq)`; `ping/pong(nonce)`; `input_gap(first_seq,last_seq)`; `outcome_unknown(turn,reason)`. Bytes use base64; duplicate windows use SSRC/first/last. |
| Worker → store | Existing lease/clock/fenced component operations: session, owner/epoch, captured bytes/references, quota and acknowledgements. Permit refresh adds no Get/Clock, script fields or replies. New-instance admission is separate. Memory opens no connection; adapter owns wire encoding. |
| App providers | None in bridge packages. Existing STT/LLM/TTS/tools remain app-owned. Each demo adapter documents endpoints, auth/headers, model/settings and audio/text/context/tool fields before release. No provider calls here. |

Runner floors reject lower generations in the same epoch and lower epochs regardless of generation. Revocation retries reuse request ID and exact arguments. Acknowledgement echoes the full transition. Advancing a floor disables old permits, assignments and reconnects before acknowledgement. Keep the highest floor through retries; ordinary bot checkpoints cannot reduce it. A restarted runner requires authoritative reauthorization before work.

The owner records a grant-stop boundary before same-epoch replacement. Invalidate in-flight old-generation permit callbacks; retain the maximum possible deadline. If it cannot be established, wait the pool cap from that boundary with grants disabled. New generations cannot borrow an old generation's renewal callback. This is distinct from the transfer expiry proof.

Existing default renewal interval is 1 s; batches of at most 32 sessions and `renewals.Wait()` can delay observations under load. No permit-specific retry/query is added. Expired permits stop bot work even if media still flows. Mixed/unknown caps cannot shorten a wait.

`limits`: `working_bytes,history_bytes,json_bytes,binary_bytes,input_packets,move_bytes`; `resume`: fresh/planned_move/takeover/bot_restart. Working allowance subtracts worst encoded bridge metadata. Overflowed pending user text returns `user_text_too_large` before generation. Cap JSON at 32 KiB, binary at 1,200 B, input at 32 packets. Queues, pending slots and frozen images share move-memory caps; reserve control capacity. Reject unknown major/schema versions; negotiate optional fields. Never fall back to Echo.

Keep separate latest-wins working/history slots with coherent complete pairs. Retry token admission without slowing transport snapshots or bypassing forced intent checkpoints. History/summary truncation is explicit and byte-bounded; no automatic summarizer. Keep tool/result pairs intact; refuse an indivisible oversized exchange. Crashes lose uncheckpointed data and buffered audio. Exact moves carry only valid frozen revisions.

Local events `relais.bridge/v1`: `bot_attached,bot_detached,bot_takeover,bot_refused`. Fields: `event_version,event_id,observed_at,session_id,owner_epoch,generation,cause,outcome,duration_ms,checkpoint_age_ms`. Explicit nullable fields, local elapsed durations and bounded cause/outcome enums. No secrets, audio or history. Bounded stdout/file delivery counts drops. Optional advertisements/extensions/exporters time out without blocking media/state. Metrics never label sessions.

Python converts Opus to PCM (raw samples) and back using pinned PyAV; Go stays `CGO_ENABLED=0`. Codec state resets after replacement. Transport/context helper/runner setup retain the VAD/STT/LLM/TTS pipeline. Nonstandard context needs a serializer; video, DataChannel/RTVI and arbitrary tool-state migration remain excluded.

## Appendix C: Measured slices and evidence

1. **Measure first:** sustained 50 packets/s through the real actor/task/pacer path with full state, simultaneous checkpoints/control/gaps and rapid interruptions. Inject scheduling jitter, partial final batches and long speech. Report queue slope, latency p95, callback/Save CPU/p99, checkpoint age/bytes and caller gaps. Measure renewal request/success gaps, tails and permit expiries under loaded sessions/slow renewals. Verify zero additional store requests/reply bytes. Compare batching and streaming output locally and under race on a free runner; neither is preselected.
2. **Hear loopback:** Python input/output on one worker, static development token restricted to isolated loopback. Test blocked sockets, local Save/Restore, rollback and export lengths. Verify hooks and unchanged Echo/legacy/other-v2 behavior.
3. **Move authenticated loopback:** two-worker tokens/runner assignment, same-epoch and cross-epoch revocation, stale/duplicate/mismatched acknowledgements, runner partition and attach-time crash. Local provider ledger proves obsolete generations cannot start work; test expiry-bound fallback and delayed renewal/permit replies.
4. **Restore conversation:** both stores, exact acknowledgement, lineage gate and generation checks at application time. Queue old checkpoint/interruption before replacement, move it, and prove it cannot apply. Race restart with takeover and uncertain writes. Exercise history pressure/user-text overflow. Verify idle `playback_finished` with state channel open, partial final audio and no next utterance.
5. **Recover processes:** real bot kill/hang/SIGTERM and fleet redeploy; worker move/drain/crash with unchanged live bot PID. Test frozen export during socket draining, failed export retry, blocked Python and source rollback. Retain consent/decryption/gap gates; new short-test cost stays below 15 s.
6. **Adopt and compare:** pinned Pipecat hands-free demo/five-minute guide with Move/Drain/Kill worker/Kill bot/Redeploy. Verify transport-plus-setup migration and codec/context/config fixtures. Benchmark in decided order; publish hardware, versions, counts, p95, failures, gaps/first audio and context. Measure full-size churn/handoffs, renewal tails and safe capacity. Providers need separate authorization.

Source review at `0898cb6`: `agent_tasks.go:584–592` checks attempt/turn, not bot generation, and spaces emissions. `agent_playback.go:44–49` counts all unfinished turn tasks. `agent_v2.go:435–463` requires exact accepted move length. `worker.go:840–850` waits for renewal batches. Existing checkpoint serialization/settlement lives in `checkpoint_snapshot.go:177–194` and `checkpoint_components.go:34–98`. These support all four findings; none establishes runtime failure frequency.

Preserve ADRs 0002/0004/0008/0013 ownership/recovery limits. ADR 0012's admission reservations stay required. Reference correction only: [ADR 0012](0012-agent-state-store-schema.md) and [session-store guide](../dev/session-store.md) retain stale pending-validation text; #214 records acceptance, with split admission still off by default. Those files are unchanged.

Official sources checked 2026-10-23: [SmallWebRTC](https://docs.pipecat.ai/api-reference/server/services/transport/small-webrtc) describes direct peers; bot-death call loss is an inference to benchmark. Pipecat [frames](https://reference-server.pipecat.ai/en/latest/api/pipecat.frames.frames.html)/[context](https://reference-server.pipecat.ai/en/latest/api/pipecat.processors.aggregators.llm_context.html) support adapter seams. [PyAV](https://pyav.org/docs/stable/overview/installation.html) documents libopus wheels; validate pinned builds. LiveKit documents [~15 s redispatch](https://docs.livekit.io/agents/server/lifecycle/), [10 s shutdown hooks](https://docs.livekit.io/agents/server/job/) and [one-hour Cloud drain](https://docs.livekit.io/deploy/agents/managing-deployments/). Apps can persist earlier. No comparative runtime result is claimed.
