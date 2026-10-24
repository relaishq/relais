# ADR 0014: Python bots behind a fenced agent bridge

Status: **Proposed**. **Amends ADR 0010**. Part of [#254](https://github.com/relaishq/relais/issues/254), aligned with [#255](https://github.com/relaishq/relais/issues/255). Code baseline: `7cc1e2f`; preceding proposal: `db148f3`.

## Approve now vs decide later

**Approved now:** the decided rows, including the 2026-10-23 choices. On **2026-10-24**, Derek chose streaming through its own bridge-only host output path and injected-stall grading. Slice 1 selected the design; next build and measure that path before loopback. Other Proposed rows and new output defaults need validation.

## What you are approving

“Decided” records Derek's answers through the coordinator; later dates are explicit. This ADR stays Proposed.

| Choice | Status | Recommended value and consequence |
| --- | --- | --- |
| Audience | Decided | Indie/startup teams self-hosting Pipecat first. LiveKit/Daily teams come later. |
| Benchmark headline | Decided | First, bot crash/redeploy without call drop versus SmallWebRTC. Second, conversation survives agent crash versus LiveKit Agents. |
| Package home/version line | Decided | `python/relais-agent` and `python/pipecat-relais` stay here. One tag versions Go binaries and both Python packages; coordinated releases need Python CI. |
| PyPI names | Decided | Publish `relais-agent` and `pipecat-relais`; `relais` is taken. |
| Token authority/pool | Decided | The owning worker issues/verifies session/epoch/bot-instance tokens. Pool per worker or relay; recommend per-worker pools. |
| Wire transport (#254 Q1) | Decided | Private WebSocket with Opus and JSON. Simple setup requires explicit bounds/version rules. |
| Dispatch (#254 Q2) | Decided | Warm runners register with every eligible worker. More sockets buy a defined reassignment/revocation path. |
| History (#254 Q3) | Decided | Enable split checkpoints: 3,072 B total working, 8,192 B history. Keep four exchanges plus bounded summary; disclose truncation. |
| Destination (#254 Q4) | Proposed | Connect directly to the owner. Operators maintain private endpoints and a bootstrap directory. |
| Issuer (#254 Q5) | Decided | Current owning worker, including after transfer. Signal/control never mint or verify attach tokens. |
| Pipecat versions (#254 Q6) | Proposed | Start with one tested Pipecat/PyAV pair. Widen only through a tested matrix; no range is supported yet. |
| Recovery targets (#254 Q7) | Proposed | Socket closed → kill-to-attached p95 <1 s; SIGTERM handoff ≤2 s. Warm capacity and reachable dependencies are required. |
| Hang target | Proposed | A hang means the bot's **pipeline** stops answering, even if its connection still answers pings. With a 2 s detector, last-pipeline-pong-to-attached p95 ≤3 s needs same-runner revocation confirmation. Without confirmation, worst-case permission wait is permit expiry, about 5 s; attachment follows. |
| Bot-permit lifetime | Proposed | Pool-wide 5 s cap, refreshed only by successful existing lease renewals. Partitions cause shorter pauses, but store outages stop new provider/tool starts sooner than media. |
| Output experiment | Decided (2026-10-24) | Use streaming through a bridge-only host output path; reject catch-up batching because its 100 ms emission floor leaves little of the 150 ms budget and it passed 9/10 runner trials versus streaming's 10/10. |
| Injected stall grading | Decided | Decided 2026-10-24. The p95 ≤150 ms gate stays on every packet, including packets held by a deliberate stall. Fault-injection runs also need bridge-queue recovery ≤500 ms after each stall ends, explicit pacer-debt accounting and a full drain. The stall cohort's direct wait is reported separately. |
| Lost speech | Decided | Save completed user text before generation. If no recoverable transcript exists, ask the caller to repeat; recordings need another design. |
| Tool safety/rollout | Decided | Accept uncertain in-flight effects; require app idempotency/query support. One extra warm slot per simultaneous handoff permits rolling redeploys. |
| Host changes | Proposed; output path decided | Admit output outside the actor; the host playback module owns its queue, floors and export state. First audio does not wait for a lifetime task. Public v2 signatures and other agents stay unchanged. |
| Late output packet | Proposed | Refuse that packet, count a gap and continue the turn. Report a gapped outcome; preserve the rest of the sentence. |

## Owner summary

Keep WebRTC in Relais. A Python bot changes its transport and a few setup lines. Scope limits: audio only (no video), no DataChannel or Pipecat RTVI client protocol, and a context that is not Pipecat's standard message list needs its own serializer. Killing/replacing the bot keeps the caller connected. A replacement restores bounded text from a checkpoint: a successful fenced store write. A fence rejects replaced owners; an epoch identifies one ownership period. A generation identifies one bot assignment. A continuing bot keeps it across worker epochs; replacement advances it.

Live media and bot replacement on a surviving owner do not need control. Worker moves/crash takeovers still do. Saved text can survive; unfinished speech, provider streams and arbitrary Python objects cannot. Tools are not exactly-once. These behaviors remain unmeasured.

## Recovery and state rules

**Fail open covers optional dependencies only.** ADR 0008's **30 s store-outage rule is the deliberate exception**: the store remains required for ownership/fencing, with bounded outage retention rather than unlimited continuation.

Open core ships a simple reference bootstrap/operator directory with fixed, manually sized warm-bot capacity. Autoscaling is optional, never a prerequisite.

The owner alone issues short-lived, single-use tokens. Warm runners supervise bots and reserve capacity. They register with every eligible worker, so a destination can issue a fresh assignment after a move/crash. Lost registry access can delay attachment; it never blocks media, Save or checkpoints. Worker-local tokens/reservations die with a crash, including halfway through attach.

Persist bot/runner identity and generation before enabling a new bot's provider/tool starts. A new instance needs fresh ownership verification and a successful fenced lineage checkpoint. Existing-lineage reconnects can preserve media through store faults. Never treat a recent ownership read as proof against a concurrent transfer.

**Every replacement needs revocation**, including replacement within the same epoch. First stop old-generation permit grants. Revoke that exact bot/epoch/generation through its runner. Advance a monotonic generation floor within each epoch; compare epoch/generation together across ownership changes. Match acknowledgements to the full old/new identities and request ID. Stale acknowledgements cannot authorize another replacement.

Without confirmation, same-owner replacement waits until the last possible old-generation permit deadline. Include queued replies and in-flight renewals; a reply cannot reset its request-start deadline. Higher-epoch takeover instead bounds expiry by transfer store-time plus the pool's maximum permit cap. Unknown bounds mean paused bot work and a typed unavailable outcome, not a second active bot. A live bot can reattach to the successor without restarting its process.

**Permits add no store commands/bytes.** Refresh only from the existing successful lease `Renew` for that owner/epoch. Anchor the owner deadline at renewal-request start plus `min(5 s, lease TTL)`, never reply receipt. A runner's nonce request uses cached remaining time, anchored at runner-local request start. Late replies and repeated requests cannot extend it; failed/uncertain renewals grant nothing. Stop new provider/tool calls on revocation, owner-socket loss or expiry. Media retains ADR 0008's outage policy.

The relay fences caller audio, not external tools. Requests already sent can finish; cancellation can fail. Every provider/tool start must obey the permit guard. Apps that bypass it are outside the guarantee. Local timing targets assume confirmed revocation and warm capacity; permit expiry bounds permission, not total first-audio time.

On SIGTERM, reserve a replacement before release. Old bot continues until its own deadline. Full-fleet replacement needs one spare slot per active session; rolling batches need one per simultaneous handoff. At the 2 s handoff deadline, expose no-capacity/silence or prior-checkpoint/`OutcomeUnknown` fallback. Never report fresh-state success without its acknowledgement.

## Contract, budgets and release

Public `Agent.Handle/Save/Restore` stays serial and deadline-bounded. All Save/Restore and move-only callbacks use owned local state; none waits for Python, socket flush or remote acknowledgement. Exact export uses a stable local snapshot whose payload length matches the last accepted Save. Appendix A defines the cut and playback rule.

The bridge **requires split checkpoints**, off by default today; explicitly enable them in its pool. Use existing `Store`/`ComponentStore` interfaces. No Redis types or keys enter the bot protocol. Keep 6,144 B hot, 70,000 B/s, 40,960 B burst, existing transaction caps and reservations. Working text gets 3,072 B minus reserved bridge metadata. Oversized user text refuses before generation and requests shorter/repeated input.

Separate latest-wins working/history candidates wait outside the accepted bot pair. Keep each candidate coherent; dependent working updates wait with its history. Budget shortage reports deferred age, not permanent size refusal. Admission still reserves the full write, hot checkpoint and unresolved-write allowance. Pending data is bounded and can be lost on crash without durable acknowledgement. History cannot be promised durable every turn.

ADR 0012's maximum hot/forced pattern leaves 2,416 B/s for cold writes; `8,228/2,416 ≈3.4 s` is conditional arithmetic. [#214](https://github.com/relaishq/relais/issues/214) measured only 1.3–3.4% headroom against **131,072 B request+reply per call-second**. Full-size churn measured 126,652–129,424 B/call-second before bridge overhead. Its ~3,155 B/s projection is not guaranteed spare capacity: any extra store I/O must be measured. Permit WebSocket bytes are separate bridge traffic. **Full-size bridge calls/primary: NOT-RUN**. Slices 1/2/7 measure per-call traffic, renewal tails, CPU and safe calls/primary; the roughly 200-call echo ceiling is not bridge capacity.

Build/publish Go binaries and both Apache-2.0 Python packages from the same tag. Include bridge protocol and supported pairs in [#252's compatibility manifest](https://github.com/relaishq/relais/issues/252); CI/preflight refuse unsupported upgrade/rollback pairs. Matching release numbers alone prove no compatibility. Reserve the PyPI names `relais-agent` and `pipecat-relais` when the packages are first published (**owner action**); this is not a precondition for accepting this ADR.

**Build the selected output path next** (Appendix A). The prototype is not for merge. Measure encoded-packet-ready in Python to host pacer admission, including all waits. Provider and caller playout time are separate. Keep raw p95 ≤150 ms with every packet included, in every phase. Injected stalls also need ≤500 ms bridge-queue recovery, pacer-debt accounting and full drain.

## Operability

Change parameters and redeploy; no hard-coded operational values. The control plane is nearly invisible; moves/takeovers still require it. Proposed defaults below are not existing flags. Validate profiles/fencing/format bounds; changing targets proves nothing.

| Tunable parameter | Default |
| --- | --- |
| Permit cap; lease renewal; outage/TTL | 5 s; 1 s nominal; 30/34 s |
| Pipeline ping; pong timeout | 500 ms; 2 s, even if socket pongs continue |
| Attach-token TTL; reservation expiry | 5 s each |
| Streaming unit; fixture packet duration | One packet; 20 ms |
| Output admission deadline | **Proposed: 50 ms** from host ingress, including queue wait; leaves latency budget for Python/IPC/scheduling |
| Pending output depth/payload bytes | **Proposed: 32 packets / 38,400 B** per session, including bot/in-flight/pre-admission packets; absorbs short bursts with bounded memory (32 × 1,200 B) |
| Crash reference prefix; host metadata | Existing 300 ms / 4 KiB of referenced payload, not a queue cap; 2,048 B total metadata |
| Bridge mailbox; retained audio | Existing 1 MiB shared mailbox; **Proposed: 256 KiB / 30 s** audio, including replay retention; leaves room for other move state and bounds long replies |
| Receipt exceptions | **Proposed: eight ranges / 512 encoded B**, inside the 2,048 B metadata cap; adjacent gaps share a range, leaving room for other host state |
| Bot interruption target | **Proposed: p95 ≤50 ms**, request-ready to committed floor; avoids the 100 ms task-emission delay |
| Planned-move audio gap | Existing pooled p95 ≤150 ms and baseline-margin/uncertainty policy; no new per-move maximum |
| Burst fixture | **Proposed: 200 packets/s for 2 s**, using the existing 160 B packets; tests four-times-real-time TTS, not maximum-packet capacity |
| Store-stall fixture | **Proposed: 29 s**; approaches the 30 s outage budget and exceeds the old payload-retention limit |
| Private exact-export wire cap | Existing 4 MiB, checked on actual encoded export/resume |
| Snapshot/Save cadence; callback; task emission | 100 ms each |
| Input queue; JSON; binary bounds | 32 packets; 32 KiB; 1,200 B |
| Move-memory session/worker | 1 MiB / 64 MiB, including retained copies |
| Bridge/host active tasks; renewal concurrency | 2/4; 32 |
| Working/history; retained exchanges | 3,072/8,192 B; four plus summary |
| Hot cap; rate; burst; store traffic gate | 6,144 B; 70,000 B/s; 40,960 B; 131,072 B/call-second |
| Setup/history/transaction caps | 4,096/8,448/20,480 B |
| Kill/hang/redeploy targets; output threshold; stall recovery | <1/≤3/≤2 s; p95 ≤150 ms; ≤500 ms |
| Warm capacity; spare capacity | Operator-set fixed slots; one spare per concurrent handoff |
| Directory; logs; inventory path | `relais-bridge.yaml`; stdout/service journal; `deployment-inventory.json` |

Inventory lists account-local resource IDs, endpoints and logs. Default names: `relais-<deployment>-<role>-<instance>`; tags: deployment, role, instance, release. Resolved parameters and private status/log commands let the customer's agent inspect its account. Never include credentials or conversation data.

**NOT-RUN — implementation, safety/timing targets, Pipecat compatibility and bridge capacity/comparisons.**

## Appendix A: Host, task and export details

Socket closure alone is insufficient. Tag all queued bot work, pending candidates and callbacks with bot instance/epoch/generation. Validate those tags again when applying work. During replacement, atomically advance the floor, cancel obsolete tasks and invalidate old checkpoints, interruptions, audio, controls and notices. Exact moves must not revive them.

Serialize replacement with checkpoint capture/write settlement. Resolve uncertain writes before choosing the replacement's durable conversation and sequence baseline. A timeout is not proof of non-commit. Commit the new lineage against the settled tuple before `attached`; a superseded tuple requires reconciliation again. Preserve current media counters. New checkpoint numbering starts at the restored sequence + 1; old-scope acknowledgements cannot advance it.

Register built-in `bridge` through `workerapp`. Amend ADR 0010 A/E's deployment/bridge boundary and Deferred streaming-input/PCM exclusion only for this bridge. Also amend its task-message-only output route, bridge sent-payload retention and playback completion as specified below. No runtime Go plugins or subprocess supervision. Update README's limit when shipping.

Two internal hooks publish bounded owned records outside locks:

- After accepted-event publication, enqueue staged input, Control and gaps. Rollback/rejection publishes nothing. Queue overflow counts/reports gaps; no callback/UDP waits.
- After `ackSnapshot`, enqueue the exact acknowledged bridge pair and captured output receipts, including settled uncertain commits. Never acknowledge a newer revision for an older write.

Network readers cannot mutate application state. Keep at most two ReadOnly handlers per turn: `bridge.state/v1` carries checkpoints/control; `bridge.audio/v1` observes output lifetime. Neither carries packets or bot interruption. The host accepts `output_begin` and reserves a turn marker before packets, without waiting for the lifetime task to start. That task cannot gate first audio or completion. Host-to-bot Control, gaps and playback notices use the post-accept queue. Remaining task chunks retain `TaskRun.After` and the emission floor.

For the bridge only, exclude both non-audio observer tasks from playback's busy-task test. The host turn marker stays busy until `audio_end(turn,last_seq)`, settled outcomes through that sequence and empty turn queues/pacer. Use the existing `PlaybackFinished` hook without another utterance. Public `PlaybackNotice{Turn}` is unchanged. The bridge joins its saved turn outcome to that hook and sends `playback_finished(turn,outcome,gap_count)`: `complete`, `gapped`, `canceled` or `unknown`. Completion proves local send, not caller hearing. Other agents stay unchanged.

A new turn rotates tasks. Cancellation releases active slots; receipts retain normal checkpoint/replay pruning. Start successors on later timer events; never from `Resumed`. Metadata pressure backpressures work; do not raise four active slots.

### Separate host output path

Terms used here:

- **Lineage:** the persisted identity of one authorized bot generation; it survives worker moves.
- **Receipt floor / exceptions:** the settled sequence prefix / sparse outcome ranges above it.
- **Replay horizon:** the cursor boundary after which old input/work cannot return; it is not a guessed timer.
- **Pacer debt:** admitted media still waiting for its send time, including delay retained after late wakes.
- **Scope transfer:** a fenced exact move binds the same lineage/packet identities to the new owner epoch; it never adopts a replaced generation.

The prototype enters `acceptedEvents` on the actor loop. It shares the 100 ms transaction deadline. Expiry after Save begins becomes `ErrSave` and closes the actor, even after a fast Save. The supported path removes this coupling.

**Admission and late packets.** A dedicated bounded ingress loop runs outside the actor. It prepares owned records outside `s.mu`, then commits short updates under that existing session lock. It never queues behind an actor transaction or holds the lock across callbacks, encoding, I/O or waits. Actor commits, snapshots, replacement and the pacer use the same lock. The independent admission deadline includes ingress queue/lock wait. Recheck deadline, scope, floors and reserved capacity at commit. A stale preparation retries only within that deadline. No per-packet `Handle`, Save or store write.

**Proposed late-packet policy:** refuse only the expired packet with `outcome=refused,reason=output_late`. Record its duration and a gap; continue the turn. This preserves the rest of a sentence. No payload is published, no automatic regeneration occurs, and output lateness never becomes `ErrSave` or closes the actor. The host records a terminal refusal before advancing the sequence floor. A gap advances RTP media time only, never the wall-clock send schedule. Do not add its duration to `nextAt`. Any gap interval already covered by late-wake rebase is consumed without advancing the timestamp twice. Record that accounting in the host cursor; no invented audio or extra wait follows starvation. Capacity refusal uses `reason=backpressure`. Lost replies remain uncertain; query before retrying identical bytes.

Packet sequences are session-wide within one lineage, across turns. Epoch is a routing fence, not a sequence reset. Checkpoint sequence numbers remain separate.

| Transition | Generation / lineage | Next output sequence |
| --- | --- | --- |
| Planned move, same bot | Keep both; epoch advances. | Keep the exact cut and bot-issued ceiling; unresolved identities are reconciled, never reused. |
| Crash takeover, same live bot | Keep checkpointed generation/lineage after authenticated runner confirmation; epoch advances. | Above both checkpoint floor and bot's last-issued sequence. Mark the missing tail unknown; absence of a receipt does not authorize replay. |
| Bot restart or replacement | Advance generation above known floors; create a new lineage. Epoch changes only if ownership also changed. | Start at 1; never reuse identities from the old lineage. |

Attach supplies the bot's last-issued output sequence. Settle a fenced checkpoint of any recovered ceiling/unknown tail before enabling resumed provider/tool starts. If identity or the ceiling cannot be reconciled, return typed unavailable or replace the bot under revocation rules; never guess a reset. Runner floors compare epoch first, generation second. An equal pair permits only the same bot/lineage; a higher epoch cannot authorize a different lineage as a reconnect.

Every admission, commit and send checks bot instance/epoch/generation, lineage and turn floors. Process packets in sequence order. Duplicates return retained outcomes; conflicting retained bytes are refused. Below a pruned floor, return settled status and never play again. Replacement invalidates all old-scope queues/callbacks atomically.

Return scoped receipts and absolute packet/byte credits. Python deducts pending/in-flight packets above the advertised settled floor and stops its producer before either pending limit. Slow sockets cannot create another queue. Restore ingress credit only after reserving accepted payloads in the shared mailbox (`MaxMailboxBytes`). `MaxAudioBytes` stays within that mailbox; `MaxAudioDuration` is a separate time limit. Both count all retained payloads, even after send. Receipt exceptions are `(first_seq,last_seq,turn,outcome,reason)` ranges. Merge adjacent identical outcomes within the same turn/lineage immediately, without waiting for storage. Both the range count and encoded bytes fit their cap within `MaxPendingBytes=2,048 B`. Credits stop before metadata/audio/mailbox exhaustion; reserve control/end/refusal space. Pressure never silently discards accepted data.

**Playback and recovery.** The host playback module owns the only live queue, payload map, send cursor and output ledger, guarded by `s.mu`. `V2.Playback` is its snapshot, not another queue. Actor commits reproject that live state; rollback restores application fields but cannot overwrite newer host commits. Stream packets use a private tagged reference: `kind=bridge,stream_id,packet_seq,turn,offset=0,count=1`. The stream ID belongs to the host turn marker, not an actor task. Normal task references keep their existing encoding/validation. A gap is a tagged sequence-range/duration record with no payload; consecutive gaps do not need one record per packet. Bridge references appear in both `V2.Playback.Window` and exact-export queues. The bridge prefix can contain multiple tagged spans within the existing 300 ms/4 KiB limits and actual metadata admission; these are not full-queue limits.

The #252 manifest entry `bridge_output/v1` specifies the tagged task/bridge/gap references, lineage-to-stream map, packet/receipt cursors and exact-export cut, gap-time accounting, cancel/clear floors, turn outcomes and private exact payload/digest section. Refuse incompatible adoption before transfer. Exact `restorePlayback` resolves bridge references against that section, not task results. Validate lineage/scope, turn, sequence, offset, bytes/duration and reconstructed `playbackEqual`; reject missing/conflicting references. Freeze queue, ledger, bridge state and pacer position together after final Save. Preserve the adapter's exact `SaveMoveOnlyLength`. No export callback waits for Python or socket drain. Failed export leaves the source usable while ownership remains; transfer uncertainty follows ADRs 0002/0009. Rolled-back staging publishes nothing.

For bridge sessions only, retire sent payload bytes once the live ledger has their settled receipt and no queued/frozen reference needs them. Store acknowledgement is not required. Keep a SHA-256 digest and identity/outcome for conflict checks; these move-only records share the 1 MiB mailbox, not the audio byte/duration allowance. Bound and compact them; pressure still backpressures, never discards an unresolved receipt. Exact exports carry retained digests as well as unsent bytes. Normal task-audio pruning keeps its checkpoint/horizon rule.

Coalesce compact receipt floors/ranges, gaps and payload presence/length into normal checkpoints. Only the captured store-acknowledged revision gets `output_checkpointed`. Keep digests/outcomes until checkpoint acknowledgement, the replay horizon and removal of live references allow compaction into the settled floor. Retiring bytes locally does not claim durability. Crash takeover restores checkpointed metadata/conversation, not audio bytes. Missing payloads become `OutcomeUnknown`; never refill an old identity with regenerated speech. Uncheckpointed output can be lost or duplicated within disclosed recovery windows. Exact moves carry all accepted queued bytes and receipts. While frozen, the source returns `moving` or no success receipt. Python keeps unresolved bytes within its pending cap. After authenticated scope transfer, query the destination: the frozen cut can prove `unadmitted`, permitting identical-byte resend. Settled receipts suppress replay. A crash checkpoint alone cannot prove that an absent receipt means unadmitted. Uncertain status waits, not blind resend.

The existing pacer still sends the packets. Its late-wake rebase can retain debt; streaming inherits [#265](https://github.com/relaishq/relais/issues/265). Its fix is outside this ADR. Keep 1 MiB shared move/mailbox, 64 MiB worker memory, actual 4 MiB export/resume wire limit, 2,048 B metadata, 6,144 B hot admission and store rate/burst/traffic gates. Account for copies and receipts before admission. No public v2 signature or other agent behavior changes.

**Interruption.** Bot `cancel_before`/`clear_audio` requests use the ingress loop's reserved priority lane, never task emission or packet credit. For bridge sessions only, the host playback module owns canonical `V2.CancelFloor` and the bridge clear-audio floor. Bridge actor effects request changes under the same lock; candidates only project these values. Other v2 agents keep their existing actor-owned floor and commit behavior. Recheck the live floors at task launch/application and before send. Cancellation signals old tasks without waiting for callbacks. Clear removes unsent older-turn packets. Persist floors at the next checkpoint. Rollback cannot lower them; sent audio cannot be recalled. The bridge-only interruption receipt confirms the committed floors. Test its proposed latency while the actor is busy and the packet lane is full.

Bridge queues use an **immutable accepted revision**. Save reports cached bridge bytes and move-only length, without copying/hashing audio. Async writers read owned views. Bridge outbox delivery/removal uses later accepted actor transactions; output uses the separate host revision above. Pending candidates are not the accepted durable bot pair. Unadmitted ingress is not export state and receives no success acknowledgement.

At exact export, stop admissions and drain mutations locally; freeze one owned revision after the final accepted Save. `SaveMoveOnly` serializes exactly that image and declared length; it is repeatable and non-destructive. It never queries Python or drains a socket. `RestoreMoveOnly` validates/copies local bytes before `Resumed`; no remote wait. Failed export leaves the source revision usable. Count retained images, in-flight writer copies and export scratch against session/worker move budgets.

Every queued item has instance/epoch/generation and a local revision. Check again at task application, candidate acceptance, post-accept delivery and export restoration. A replacement transaction invalidates all old-scope queues/tasks atomically. An exact worker move may explicitly remap a frozen revision only for the same authorized bot lineage. It cannot relabel an obsolete generation after bot replacement. Missing unfinished output/effects become `OutcomeUnknown`; the same live bot can keep its generation and lineage. A replaced bot cannot. Never regenerate different audio under an old reference.

Checkpoint capture/write settlement keeps its existing serialization and fences. A confirmed old-generation write may supply recoverable conversation, but cannot authorize old queued effects. Resolve its exact pair before publishing new lineage/baseline; new lineage commit prevents late old CAS writes from replacing it. Unresolved store outcomes postpone attachment. Test simultaneous bot restart, delayed commit and worker takeover.

## Appendix B: Connections and protocol

The bridge and Python packages never phone home. License checks and usage reporting belong only to a separate paid operations layer, outside the call path. Disable model downloads, proxy autodiscovery, tracing/exporters and update checks; supply VAD assets locally. DNS uses configured names (A/AAAA); TLS uses configured SNI/certificates. Reject browser Origins/untrusted redirects. Credentials never enter URLs/logs.

| Connection | Application fields |
| --- | --- |
| Runner ↔ eligible workers, registry WebSocket | TLS identity/Authorization; `hello(protocol,runner_id,bot_instance,agent_name,config_revision,capacity)`; `reserve(session_id,epoch,reservation_id,expires_ms)`; `reserved(reservation_id,accepted)`; `assign(session_id,epoch,bot_instance,generation,lineage_id,attach_token,expires_ms)`; `revoke(session_id,bot_instance,old_epoch,old_generation,new_epoch,new_generation,request_id)`; `revoked(session_id,bot_instance,old_epoch,old_generation,new_epoch,new_generation,request_id)`; `permit_request(session_id,epoch,generation,nonce)`; `permit(session_id,epoch,generation,nonce,remaining_ms)`; `ping/pong(nonce)`. |
| Bot → bootstrap worker, HTTPS lookup | Authentication; request `session_id`; reply `owner,epoch,worker_url` from operator directory. |
| Bot ↔ owner, session WebSocket | Authentication; `attach(protocol,session_id,epoch,bot_instance,generation,attach_token,last_checkpoint_seq,last_output_seq)`; `attached(epoch,generation,lineage_id,resume,next_output_seq,checkpoint_seq,checkpoint_age_ms,snapshot_age_ms,working,history,output_receipts,outcome_unknown,duplicate_windows,limits)`. Socket binds instance/epoch/generation; copy that scope into every internal queued item. |
| Binary | Input `type=1,input_ssrc:u32,input_seq:u64,rtp_ts:u32,duplicate:u8,opus`; output `type=2,turn:u64,seq:u64,opus`. Big-endian integers; remainder is payload. Validate lengths/durations/sequences. |
| Session JSON | `checkpoint(seq,version,input_ssrc,input_seq,working,history)`; `checkpointed(epoch,generation,seq)`; `superseded(seq)`; `deferred(seq,reason,age_ms)`; `refused(seq,reason)`; `audio_end(turn,last_seq)`; `playback_finished(turn,outcome,gap_count)` (bridge-only); `control(id,sequence,name,data)`; `moving(owner,epoch)` advisory; `release(final_checkpoint_seq)`; `ping/pong(nonce)` answered by the bot's pipeline, not by the WebSocket library (library-level pongs do not count as liveness); `input_gap(first_seq,last_seq)`; `outcome_unknown(turn,reason)`. Bytes use base64; duplicate windows use SSRC/first/last. |
| Output JSON, same session WebSocket | Common scope: `session_id,bot_instance,epoch,generation,lineage_id`; credits are per lineage, not per turn. Bot→host: `output_begin(request_id,turn)`; `output_status(seq)`; priority `cancel_before(request_id,turn)` / `clear_audio(request_id,before_turn)`. Host→bot: `output_begun(request_id,stream_id,turn)`; `interrupted(request_id,cancel_floor,clear_floor,revision)`; `output_receipt(seq,turn,revision,outcome,reason)`; `output_status_reply(seq,revision,status,receipt)` with status `unadmitted,settled,unknown`; `output_checkpointed(through_seq,exceptions,revision,checkpoint_seq)`; `output_credit(revision,through_seq,packet_slots,payload_bytes)`. Receipt outcomes: `admitted,refused,canceled,sent,unknown`; refusal reasons include `output_late,backpressure,stale_scope,conflict,closed_turn,invalid_audio`. Resume includes settled/durable/sent floors, exceptions and unknown ranges. Credits are absolute; stale revisions cannot replenish them. |
| Worker → store | Existing lease/clock/fenced component operations: session, owner/epoch, captured bytes/references, quota and acknowledgements. Permit refresh adds no Get/Clock, script fields or replies. New-instance admission is separate. Memory opens no connection; adapter owns wire encoding. |
| App providers | None in bridge packages. Existing STT/LLM/TTS/tools remain app-owned. Each demo adapter documents endpoints, auth/headers, model/settings and audio/text/context/tool fields before release. No provider calls here. |

Runner floors reject lower generations in the same epoch and lower epochs regardless of generation. Revocation retries reuse request ID and exact arguments. Acknowledgement echoes the full transition. Advancing a floor disables old permits, assignments and reconnects before acknowledgement. Bind the accepted pair to bot instance/lineage; equal pairs cannot switch them. Same-bot epoch changes follow the transition table. Keep the highest floor through retries; ordinary bot checkpoints cannot reduce it. A restarted runner requires authoritative reauthorization before work.

For takeover, derive transfer store-time from returned Transfer `ExpiresAt − applied TTL`. Use existing recovery clock observations, or conservatively wait 5 s after local Transfer confirmation. For same-epoch replacement, freeze grants and use the known maximum outstanding deadline, or wait the cap after grant issuance is disabled. Do not use the transfer formula without a transfer. Mixed pools use the maximum known cap; clock/identity uncertainty delays activation.

The owner records a grant-stop boundary before same-epoch replacement. Invalidate in-flight old-generation permit callbacks; retain the maximum possible deadline. If it cannot be established, wait the pool cap from that boundary with grants disabled. New generations cannot borrow an old generation's renewal callback. This is distinct from the transfer expiry proof.

Existing default renewal interval is 1 s; batches of at most 32 sessions and `renewals.Wait()` can delay observations under load. No permit-specific retry/query is added. Expired permits stop new provider/tool starts; already-running output can continue while retained ownership permits media. Mixed/unknown caps cannot shorten a wait.

`limits`: `working_bytes,history_bytes,json_bytes,binary_bytes,input_packets,move_bytes,output_packets,output_payload_bytes,output_admission_ms,mailbox_bytes,audio_bytes,audio_duration_ms,receipt_exceptions,receipt_bytes`; `resume`: fresh/planned_move/takeover/bot_restart. Working allowance subtracts worst encoded bridge metadata. Overflowed pending user text returns `user_text_too_large` before generation. Cap JSON at 32 KiB, binary at 1,200 B, input at 32 packets. Queues, pending slots and frozen images share move-memory caps; reserve control capacity. Reject unknown major/schema versions; negotiate optional fields. Never fall back to Echo.

Keep separate latest-wins working/history slots with coherent complete pairs. Retry token admission without slowing transport snapshots or bypassing forced intent checkpoints. History/summary truncation is explicit and byte-bounded; no automatic summarizer. Keep tool/result pairs intact; refuse an indivisible oversized exchange. Crashes lose uncheckpointed data and buffered audio. Exact moves carry only valid frozen revisions.

Local events `relais.bridge/v1`: `bot_attached,bot_detached,bot_takeover,bot_refused`. Fields: `event_version,event_id,observed_at,session_id,owner_epoch,generation,cause,outcome,duration_ms,checkpoint_age_ms`. Explicit nullable fields, local elapsed durations and bounded cause/outcome enums. No secrets, audio or history. Bounded stdout/file delivery counts drops. Optional advertisements/extensions/exporters time out without blocking media/state. Metrics never label sessions.

Python converts Opus to PCM (raw samples) and back using pinned PyAV; Go stays `CGO_ENABLED=0`. Codec state resets after replacement. Transport/context helper/runner setup retain the VAD/STT/LLM/TTS pipeline. Nonstandard context needs a serializer; video, DataChannel/RTVI and arbitrary tool-state migration remain excluded.

## Appendix C: Measured slices and evidence

1. **Output choice measured:** local factor campaign `run_7b73f0019f727069e905bf8a7401e9bc` and rechecks `run_479173f0703f61332b8e48cb1b75326e` compared sustained 50 packets/s with full checkpoints, jitter, partial batches, CPU/renewal stalls and rapid turns. Batching typically added about 100–110 ms. Free GitHub runner **40116653443** added fast rotation, stress on, five memory and five race/Redis trials per design. Catch-up-20 passed **9/10**; one rapid-phase p95 was **196.8 ms**. That trial had 99 total refusals, including 98 task-admission-capacity refusals: correlation only. Streaming passed **10/10**, zero closures, rotation upper-p95 **29.5–86.4 ms**. Rapid-turn upper-p95 was **103.1–124.7 ms** (memory) and **106.9–114.5 ms** (race/Redis), about 25 ms minimum margin. Logs do not prove that task startup caused this delay; the new path removes that prerequisite. These passes used the spike gates; its refusals would fail the supported-path gate below. This selected streaming, not production acceptance. Local renewal maxima were 1,041/1,157 ms request/success; fleet permit/capacity proof remains NOT-RUN.
2. **Build and measure host output:** implement Appendix A before loopback. Repeat ≥60 s at 50 packets/s with full checkpoints/control/gaps, sustained speech, jitter, partial tails and rapid turns. Add the proposed faster-than-real-time burst. “Ready” is when encoded bytes first exist in Python, before any credit wait or IPC hold; timestamp provider output before applying backpressure. The fixture source uses its own production clock; blocked transport cannot reset ready times or reduce the expected packet count. Track every produced packet ID. Raw p95 ≤150 ms includes credit/IPC/admission waits in each run/phase, with no overlap subtraction. Every unrequested refusal or late packet fails qualification outright; also require zero `output_late` in nonfault phases. Intentional interruption cancellations are separately reconciled to their accepted floor; they cannot hide unexplained loss. All admitted uncanceled packets must drain. Require zero closures, ≤500 ms bridge-queue recovery, and explicit pacer debt. Keep refused duration in recovery accounting, so dropping packets cannot prove recovery.

   Run locally under race/Redis and on a free GitHub runner. Put the existing 150 ms delivery stalls on the Python/IPC side, before host ingress: keep original ready times, then resume the same turn and drain all packets. Host deadlines do not include that upstream hold, but the raw latency gate does; recovery cannot excuse a failing p95. Separately stall host admission beyond its deadline, before the commit lock. Expect packet-only `output_late`, a gapped turn that continues, and no actor closure. This is a negative control: grading must fail it, not count it as a qualifying pass. Park an actor transaction with a controlled budget and prove output admission still proceeds; do not rely on a real 100 ms race-test deadline.

   The 160 B burst must pass the same raw gate. A maximum-size-packet burst is expected to exhaust credits and fail latency qualification; it is an overload control, not a capacity claim. Still require bounded memory, explicit backpressure and no actor closure. Separately force credit exhaustion with reduced test caps or a held pacer; verify bounded producer/memory and include credit-wait time. This overload control must fail latency grading when it exceeds the gate, rather than claim a fast post-credit measurement. Measure queue slope, CPU/Save tails, checkpoint age/bytes and loaded renewal gaps. Verify no per-packet store writes or permit I/O. Test lost receipts, duplicate/conflicting packets, pruning, idle completion, gap outcomes and actual encoded caps. A blocked output admission after a fast Save must not close the actor. Verify unchanged other-agent behavior.

   Pause the store for the proposed 29 s with an already-running output stream. Exercise both fixture-size and negotiated maximum-size packets, enough to exceed the old retained-byte cap. Sent bytes must retire from live receipts, so the stream continues without retention-induced silence, refusals or actor closure; memory/digests stay within caps. No `output_checkpointed` may be fabricated. New provider/tool starts still stop when the 5 s permit expires; already-running output can continue under retained media ownership. This test does not promise new conversation turns through a store outage. On recovery, checkpoint the latest live ledger, without replaying sent audio. A crash during the pause restores only the last durable state and reports the uncertain tail. Test two host stalls before checkpoint acknowledgement: consecutive late packets coalesce into ranges; they cannot consume a slot each. Test gap timestamp advancement without an added schedule delay or a second rebase charge.

   Move mid-sentence with packets in flight and a lost source receipt. Reconcile at the destination, resend only proven-unadmitted identical bytes, and assert zero duplicate packet IDs on wire. Grade caller gaps with the existing pooled planned-move p95 ≤150 ms policy, including baseline margin and clock uncertainty, rather than a maximum for each move. Test failed export/source rollback too. Race a replacement commit against old-generation admission and pacer sends: after that locked commit, no old-generation packet may cross the worker's wire-send boundary. Packets sent before the commit can arrive later. Repeat same-epoch replacement and worker takeover; queued callbacks cannot restore the old floor.
3. **Hear loopback:** Python input/output on one worker, static development token restricted to isolated loopback. Test blocked sockets, local Save/Restore, rollback and export lengths. Verify hooks and unchanged Echo/legacy/other-v2 behavior.
4. **Move authenticated loopback:** two-worker tokens/runner assignment, same-epoch and cross-epoch revocation, stale/duplicate/mismatched acknowledgements, runner partition and attach-time crash. Local provider ledger proves obsolete generations cannot start work; test expiry-bound fallback and delayed renewal/permit replies.
5. **Restore conversation:** both stores, exact acknowledgement, lineage gate and generation checks at application time. Queue old checkpoint/interruption before replacement, move it, and prove it cannot apply. Race restart with takeover and uncertain writes. Exercise history pressure/user-text overflow. Verify idle `playback_finished` with state channel open, partial final audio and no next utterance.
6. **Recover processes:** real bot kill/hang/SIGTERM and fleet redeploy, including a stalled pipeline whose connection still answers WebSocket pings; worker move/drain/crash with unchanged live bot PID. Test frozen export during socket draining, failed export retry, blocked Python and source rollback. Retain consent/decryption/gap gates; new short-test cost stays below 15 s.
7. **Adopt and compare:** pinned Pipecat hands-free demo/five-minute guide with Move/Drain/Kill worker/Kill bot/Redeploy. Verify transport-plus-setup migration and codec/context/config fixtures. Benchmark in decided order; publish hardware, versions, counts, p95, failures, gaps/first audio and context. Measure full-size churn/handoffs, renewal tails and safe capacity. Providers need separate authorization.

Source review at `0898cb6`: `agent_tasks.go:584–592` checks attempt/turn, not bot generation, and spaces emissions. `agent_playback.go:44–49` counts all unfinished turn tasks. `agent_v2.go:435–463` requires exact accepted move length. `worker.go:840–850` waits for renewal batches. Existing checkpoint serialization/settlement lives in `checkpoint_snapshot.go:177–194` and `checkpoint_components.go:34–98`. These support all four findings; none establishes runtime failure frequency.

Preserve ADRs 0002/0004/0008/0013 ownership/recovery limits. ADR 0012's admission reservations stay required. Reference correction only: [ADR 0012](0012-agent-state-store-schema.md) and [session-store guide](../dev/session-store.md) retain stale pending-validation text; #214 records acceptance, with split admission still off by default. Those files are unchanged.

Official sources checked 2026-10-23: [SmallWebRTC](https://docs.pipecat.ai/api-reference/server/services/transport/small-webrtc) describes direct peers; bot-death call loss is an inference to benchmark. Pipecat [frames](https://reference-server.pipecat.ai/en/latest/api/pipecat.frames.frames.html)/[context](https://reference-server.pipecat.ai/en/latest/api/pipecat.processors.aggregators.llm_context.html) support adapter seams. [PyAV](https://pyav.org/docs/stable/overview/installation.html) documents libopus wheels; validate pinned builds. LiveKit documents [~15 s redispatch](https://docs.livekit.io/agents/server/lifecycle/), [10 s shutdown hooks](https://docs.livekit.io/agents/server/job/) and [one-hour Cloud drain](https://docs.livekit.io/deploy/agents/managing-deployments/). Apps can persist earlier. No comparative runtime result is claimed.
