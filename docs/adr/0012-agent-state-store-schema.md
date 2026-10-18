# ADR 0012: Split session checkpoints and bounded conversation history

Status: **Proposed** 2026-10-17 — awaiting owner approval. Part of #188.
Baseline: `1b952f65f0866e42a79fb8b1fd091328bff05b7a` (v0.2.0).

## Owner summary

- Save transport setup once. Save changing transport and agent progress at the current cadence.
- Save working app bytes and conversation history only when each changes, in the same fenced write.
- Recommend **3 KiB working state plus 8 KiB history** on defaults. These are proposed allocations, not measured capacity.
- Give history a separate SDK field. Otherwise a packet-counter change can rewrite the whole conversation.
- Demo A can keep its existing last four exchanges, each with up to 512 JSON-encoded text bytes per side. This is bounded memory, not a full-call transcript.
- A crash keeps committed text. It still loses local recordings, buffered speech and unfinished provider work.
- Keep every session's keys in one Cluster slot: one database shard can update them together.
- Install compatible readers everywhere before enabling the new format. Older workers cannot take over new-format calls.
- Keep the existing capacity and recovery gates. Smaller writes do not establish more calls per primary.
- Approve the design first. Enable defaults only after a keyless crash test and matched traffic/capacity runs pass.

## Context and evidence

[ADR 0010, C and Deferred](0010-pluggable-agents.md) requires this split before a default-budget
conversation-survival claim. Current custom agents use envelope v8, or v9 after Control;
`pkg/mediaworker/{state,handover,agent_v2,snapshot,agent_limits}.go` defines the formats and admission.
`internal/agenthost/registration.go` resolves the current 1 KiB app default. Legacy agents retain
an 8 KiB default. Neither the legacy default nor ordinary Save is a durable acknowledgement.

`pkg/sessionstore/redis.go` reserves a fenced sequence, seals the bytes client-side, then commits
through Lua. Its same-slot keys, monotonic epochs, unknown-outcome handling and checkpoint HMAC
are safety contracts. Memory must implement the same observable contract. The parallel proposed
ADR 0011, *Deploy and autoscale*, requires Cluster/TLS and a durable managed store; this ADR
coordinates with its store-client slice but does not depend on its remaining deployment automation.

### Local size inspection, 2026-10-17

This command only decodes tracked fixtures; it opens no sockets and changes no code:

```sh
python3 - <<'PY'
import json
from pathlib import Path
size = lambda x: len(json.dumps(x, separators=(',', ':')).encode())
for name in ('v7-787127e.json', 'v9-control.json'):
    d = json.loads((Path('pkg/mediaworker/testdata') / name).read_bytes())
    before = size(d)
    del d['DTLSConnection']
    for k in ('Certificate', 'PrivateKey'):
        del d['State']['DTLS'][k]
    print(name, before, before - size(d))
h = size([{'user': 'u'*512, 'assistant': 'a'*512}]*4)
print('history', h, 'working remainder', 7087 - len(',"history":') - h)
PY
```

Output: v7 **3,474 B**, v9 **3,708 B**; deleting certificate/key entries and the whole opaque DTLS
connection removes **2,121 B** in either fixture. Four maximum-size history exchanges occupy
**4,205 B**; subtracting that field from Demo A's documented **7,087 B** worst case leaves
**2,871 B**. The latter is a derivation from [the agent guide](../build-an-agent.md#run-demo-a),
not a new live-agent measurement. Source: `examples/voice-agent/voice/agent.go`, `State` and `boundedText`.

These fixture sizes do **not** prove that the whole DTLS connection is constant. The pinned
`github.com/pion/dtls/v3 v3.1.8` (`go.mod`) serializes epochs and `SequenceNumber` alongside secrets
and certificates (`state.go`, `serializedState`, `serialize`; `conn.go`, record writes).
Local module-source inspection confirmed this. The implementation must preserve changing fields.
ADR 0010's **2,072 B/checkpoint** saving remains an optimistic planning input, not a verified split.

## 1. Schema and commit protocol

**Recommendation:** use a bounded current checkpoint, immutable setup, and separate working/history
components. No append-only transcript, result inbox, recording store or unlimited revision chain.
Use an additive typed sessionstore checkpoint API; keep legacy PutState/GetState for old formats.
Never silently implement a split checkpoint through separate uncoordinated PutState calls.

### Keys and contents

Let `P` be the deployment prefix and `T` the existing escaped session tag `{sess:ID}`.
Reuse `Redis.keys` escaping, including its special-ID encoding; reject braces in `P` as today.
Every key below, including both component banks, is an explicit `KEYS` argument to the session script.
A bank is one of two fixed storage positions; writes prepare the inactive bank before publishing it.

| Key, excluding `P` | Contents and write rule |
| --- | --- |
| Existing `lease:T`, `record:T`, `epoch:T`, `routes:T`, `ended:T` | Ownership, ordering and lifecycle metadata; preserve existing contracts. |
| Existing `state:T` | Legacy ciphertext during the reader-first migration only. No shadow dual-write after activation. |
| `setup:T` | Encrypted setup generation, certificate/private key, pinned peer identity and invariant DTLS fields. Write once after establishment; retain across ownership transfers. |
| `checkpoint:T` | Hash with encrypted v10 transport/progress payload, writer owner/epoch, put sequence, component references, keyed checkpoint digest and store-clock commit time. Publish together in one final HSET. |
| `work:T:0`, `work:T:1` | Encrypted app State.Version and raw working Bytes; replace inactive bank only when content/version changes. |
| `history:T:0`, `history:T:1` | Encrypted raw History bytes; replace inactive bank only when history/version changes. Empty history is an explicit value, not an absent-key fallback. |

The changing encrypted payload contains ICE nomination, mutable DTLS epochs/sequence state, all SRTP/
SRTCP counters and replay windows, tracks, checkpoint observations, consumed-input progress, host
intents/cursors/timers/Control receipts, move-only presence/length, quota, and registration identity.
**Host progress and quota do not increment app content revisions.** The app pair includes working
and history references plus State.Version; the payload names its exact root revision and keyed digest.
Both components are opaque bytes to the host; the app's State.Version covers their joint schema.
The optional history area is for infrequent durable app data, not a host-defined conversation format.

Implement a versioned DTLS adapter for the pinned serialization: separate invariant fields from
LocalEpoch/RemoteEpoch/SequenceNumber, and reconstruct a complete Pion state on restore. Prove
round trips and changed-counter preservation before enabling it. Do not treat a saved opaque
MarshalBinary result as constant. Keep mutable ICE/track fields in the changing payload. Unexpected
setup changes require explicit refusal, not silently reusing old secrets; re-handshake/rekey support
is outside this ADR. Setup key rotation retains old decrypt keys until those sessions expire.

### Fencing, revisions and encryption

- Preserve the existing sequence-reservation script and its owner/epoch/expiry fence. A reservation
  is not a checkpoint. Sequence gaps are harmless. Encrypt outside media/actor locks.
- Compare exact accepted component bytes and version, not the enclosing host revision or ciphertext.
  Changed components receive the reserved put sequence as their new revision. Unchanged components
  reuse the last committed bank/revision/digest. A changed pair gets that sequence as root revision;
  host-only writes retain the root. Compare decimal sequence strings without Lua numeric rounding.
- Use AES-256-GCM per component with fresh random nonces on encryption, a version/key-ID header and
  HKDF domain separation for setup, work, history and checkpoint. Associated data binds session ID,
  setup generation, component kind, revision and header. Bind checkpoint owner/epoch/sequence too.
  Component encryption must survive Transfer: do not require its creation epoch to equal the new lease.
- Use domain-separated HMAC-SHA256 digests for component contents and a canonical length-prefixed
  root tuple `(State.Version, work revision/digest, history revision/digest)`. Do not expose unkeyed
  text fingerprints. The encrypted payload authenticates those exact references and setup digest.
  Lua compares opaque references/digests; clients authenticate and recompute them after decryption.

The **commit EVAL** checks the live owner/epoch, expiry, newer put sequence, expected current
checkpoint/root, setup identity, key types, argument bounds and component-reference preconditions
before any mutation. It writes changed components into inactive banks, sets their expiry, and writes
setup only on initialization. It prepares the checkpoint hash's lease-relative expiry before
publication (create an unpublished hash shell first on initialization). It then publishes ciphertext,
references, sequence, keyed tuple
digest and `TIME` timestamp in **one HSET to checkpoint:T**. That final command is the commit point.
No later fallible metadata update is required to recognize the checkpoint. Renew/Transfer preserve
and extend this record; sequence reservation remains in the lease hash.

Do not assume script errors roll back prior commands. Inactive banks protect the last published
checkpoint if preparation fails. Preflight types/arguments and put expiry before publication;
noeviction and bounded memory are deployment requirements. Orphan preparations occupy only fixed
banks and expire with the lease. Reference and digest fields include their key IDs so retained
components remain verifiable across master-key rotation. Once published, a scripted read can finish
before any bank is reused.
Never overwrite a referenced bank or mutate setup during preparation. A lost commit reply is unknown:
re-read the primary's tuple, then retry against that tuple with a fresh reservation if necessary.
Do not debit twice or launch a side effect merely because preparation succeeded. Preserve existing
bounded retry, ErrTransient, ErrLeaseLost and ErrStateSuperseded distinctions.

Local Redis primitive probe, **2026-10-17**: on an owned throwaway loopback server at an ephemeral
non-default port, `redis-cli -p "$PORT" EVAL "redis.call('SET',KEYS[1],'new'); error('injected-before-publish')" 1 'work:{probe}:1'`
returned an error but retained the inactive write. GET/HGET checks confirmed the published old bank
remained intact; a final HSET selected the new bank. HSET also preserved a preinstalled hash TTL
(checked with PTTL). The owned process was stopped and reaped. This tests Redis primitives only,
not the proposed schema, crash recovery or managed durability.

### Restore, expiry and cleanup

Read the lease, published hash, setup and the referenced component banks in **one same-slot EVAL**
on the primary. Authenticate each piece, recompute component/root/whole-checkpoint digests and
check revisions, bank identifiers, setup generation, session ID, lengths and app/host versions.
Return one owned complete v10 snapshot to recovery. Preserve the captured store-clock sample and
bind Checkpoint metadata to this exact assembled tuple; a later superseding write causes a reread,
never a mixture. Transfer may legitimately leave a checkpoint written by the preceding epoch.

Missing components, wrong revisions, authentication failures and conflicting generations are unsafe
checkpoints. Fail closed through existing recovery-loss cleanup. Never substitute empty history,
an older bank or legacy state. Preserve ADR 0002's snapshot-age test and margins. This scheme detects
a torn combination; the inactive-bank protocol also keeps an uncommitted preparation from destroying
the previous committed combination. No claim covers operator corruption or a store that loses writes.

Renew and Transfer extend **all** checkpoint/setup/component expiries to the lease's same absolute
store-clock expiry, including unchanged values. Do not refresh a missing required value into existence.
Release, expired-lease pruning, indexed cleanup and fresh Claim delete all component banks and setup,
including uncommitted preparations. Stale Release cannot delete a successor. New claims create a new
setup generation. Preserve ADR 0008's **34 s** default lease lifetime for its **30 s** outage budget;
preserve metadata/ended retention and the default **five-minute** retention setting (`redis.go`,
[session-store guide](../dev/session-store.md)). History has no post-call retention or independent lease.

**Options/tradeoff:** a single larger blob is simpler but repeats history on progress changes. A separate
unfenced history store can restore mismatched state. Unlimited immutable revisions or an append log
need garbage collection, compaction and new retention semantics. Fixed banks cost extra bounded memory
and a larger script/key set. **Change this if:** measured workloads require long transcripts or if a
versioned upstream DTLS export eliminates the adapter; those need their own bounded design/proof.

## 2. Defaults, SDK shape and traffic

**Recommendation:** add `State.History []byte`, `Limits.MaxHistoryBytes` and
`FactoryArgs.MaxHistoryBytes`; existing Save/Restore transaction boundaries cover both byte fields.
For newly enabled v10 custom sessions resolve zero limits to **MaxStateBytes = 3,072 B** working
state and **MaxHistoryBytes = 8,192 B** history, **11 KiB total**. Retain ADR 0010's **2 KiB encoded
host allowance**, separate move memory and **64 KiB configurable working-state ceiling**. Larger
configured components require actual encoded-size/rate admission; never silently clamp or truncate.
An app without history uses empty History and the working allowance; larger working blobs remain opt-in.
These numeric limits are proposed choices, informed by the size inspection and arithmetic here.

Demo A should move History out of its working JSON, bump its app schema, and use zero/default limits
in a new compatible pool. Its current four exchanges use **4,205 B** of the history allocation and
leave derived **2,871 B** working state under 3,072 B. History alone could hold **seven** exchanges
of that exact shape: exact JSON is `7 * 1,051 + 1 = 7,358 B`, while eight use **8,409 B**.
Keep Demo A at four; these are byte examples,
not a promise about tokens, language, minutes or an unbounded call. Recheck its actual worst case
through Save after the app schema change, including escaping and non-ASCII text.

Proposed operator allocations for split mode (legacy mode keeps existing flag meanings):

| Allocation | Proposed default | Purpose |
| --- | ---: | --- |
| Hot bytes/checkpoint | 7,168 B | Combined ciphertext/framing for transport/host plus changed working component |
| Setup bytes | 4,096 B | Initial encrypted setup, separately admitted |
| History bytes/write | 8,448 B | Raw history plus a proposed 256 B framing reserve |
| Total checkpoint transaction | 20,480 B | Bounds initialization/forced writes containing all components |
| Successful payload rate | 98,304 B/s | 96 KiB/s; `10*7,168 + 20,480 = 92,160 B/s`, with 6,144 B/s spare |
| Payload burst | 81,920 B | Four maximum transactions; proposed reserve for initialization and forced work |

Keep the **100 ms** checkpoint cadence, forced intent barriers and actual byte accounting
(`agent_limits.go`, ADRs 0002/0010). Rework validation to reserve the hot checkpoint independently
of cold components, rather than `wholeMax * cadence`. Debit actual successfully committed changed
ciphertexts, carry quota across moves, and reserve a maximum in-flight transaction on exact export.
Count every attempted protocol byte, retries and script loads in traffic gates. History churn does
not earn a slower transport clock: backpressure/refuse the app transaction before acceptance when
it cannot fit. Store failure is not app Capacity. No new delayed-write timer changes crash semantics.

### Arithmetic projections, not measured store traffic

Use decimal B/s. Inputs: ADR 0010's 3,474 B v7 baseline, 2,072 B optimistic setup saving, 2,048 B
host allowance and 10 checkpoints/s; current `redis.go` encryption adds **30 B** per ciphertext
(2 header + 12 nonce + 16 tag). ADR 0010 conservatively reserved **42 B** for its admission formula;
that reserve is not the current measured cryptographic overhead. JSON base64 of 8,192 B is 10,924 B.
The new components carry raw bytes, avoiding that expansion inside store values. Proposed extra
reference/framing reserve **256 B** is an estimate to replace with encoded measurements.

| Layout / workload assumption | Projected checkpoint payload and payload B/s |
| --- | --- |
| Old default 1 KiB app, whole blob at 10/s | `(3,474 + 2,048 + 1,368 + 30) * 10 = 69,200 B/s` |
| Old 8 KiB app, whole blob at 10/s | `(3,474 + 2,048 + 10,924 + 30) * 10 = 164,760 B/s`; needs explicit larger settings |
| New changing transport/host, excluding working bytes | `(3,474 - 2,072 + 2,048 + 256 + 30) * 10 = 37,360 B/s` |
| New full 3 KiB working changes at 10/s; 8 KiB history changes once/5 s | `37,360 + (3,072+30)*10 + (8,192+30)/5 = 70,024.4 B/s` |
| New both app components change once/5 s | `37,360 + (3,072+30)/5 + (8,192+30)/5 = 39,624.8 B/s` |

Initialization adds setup once; TTL refreshes, reserve/commit commands and replies still cost bytes.
The hot worst-case estimate `3,474-2,072+2,048+256+3,072+60 = 6,838 B` fits 7,168 B only as a
projection. Actual v10 encoding/counter growth is a release gate, not permission to exceed a cap.
Both app fields changing every checkpoint would instead cost **150,600 B/s payload**; the default
rate cannot sustain that. This is why history must be separate and low-churn. Demo A updates Seen
on audio input (`voice/agent.go`); merely splitting its existing monolithic Bytes would still rewrite
history during recording. Its working bytes may change at each checkpoint; the high-churn row models that.

ADR 0010's history comparison rechecks as `10,924/5 = 2,184.8 B/s` versus `109,240 B/s` at 10/s,
before encryption. Its setup projection is `2,072*10 = 20,720 B/s`, **41.2%** of the tracked
**50,277 B/s session traffic**, or **24.4%** of **84,904 B/s total**, from
[capacity-results.md, hosted run 38142256615](../dev/capacity-results.md). These ratios compare
projected removable bytes with an older measured echo workload; they are not measured v10 savings.

Adding that run's **31,371 frame + 3,256 owners/routes B/s** to the new high-churn payload gives
**104,651.4 B/s**, leaving **26,420.6 B/s** below the **131,072 B/s** request/reply ceiling for
session protocol, reads and retries. This mixed-workload projection is not a wire-traffic result.
Do not subtract setup savings twice or treat durable-log bytes as application TCP bytes.

**Options/tradeoff:** one 8 KiB Bytes field avoids an SDK addition but fails the churn argument;
64 KiB or an unbounded history log has no supporting budget evidence. Splitting working/history asks
apps to classify their data and limits working state more than an undivided 8 KiB budget would.
**Change this if:** actual v10 encoding fails admission, representative history churn exceeds the
rate, or measured Save/restore costs fail the capacity gate. Stop default activation and return the
measurements for a revised allocation; do not raise policy ceilings to make the test pass.

## 3. Crash contract and developer meaning

**Recommendation:** keep ADR 0010's guarantees/exclusions. On SIGKILL takeover, restore the latest
valid committed app pair with its exact consumed-input and task progress. Text in that pair survives,
at least through the last acknowledged checkpoint unless a later committed app update deliberately
replaces it. A committed write with a lost reply may also survive. A later Save or local callback
acceptance is not proof of durability. A Control 200 proves the checkpoint barrier; 202 does not
(`docs/build-an-agent.md`, Control receipts). History is bounded app data, not an append-only audit.

The current recording, ReadOnly request payloads, generated/buffered speech, local mailbox and
in-flight provider operation still do not survive a crash. Preserve OutcomeUnknown and Resumed
notices, retry/abandon rules, duplicate-input windows, stable task keys and checkpoint-before-side-effect
launch. Already committed current-turn text can survive; it is not proof the caller heard the audio.
Provider work can repeat and incur charges. Exact planned moves still carry bounded move-only bytes;
this ADR adds no durable audio/results, exactly-once effects or seamless-playback claim.

SDK docs must say: “Put small changing state in Bytes. Put bounded conversation text in History.
Save both together. Restore both together. Save is not a durable acknowledgement. A crash can lose
the current turn's audio and repeat work.” Do not introduce an SDK persistence receipt in this slice;
retain current Control receipts and host barriers. **Tradeoff:** explicit bounded recovery is weaker
than durable recording/result storage. **Change this if:** a separately funded durable-inbox/media
feature defines costs, acknowledgement and retention; do not infer it from this split.

## 4. Migration and compatibility

**Recommendation:** envelope **v10** for split custom sessions; retain v6/v7 legacy and v8/v9 readers.
This is separate from the encryption header version and app State.Version. Private exports remain
self-contained: include setup, working/history and exact move-only payload, never Redis key pointers.
Crash store reads assemble that same v10 restore object. Keep strict private API compatibility.

Ship a reader-first bridge release with split writing off. It reads all supported envelopes and both
store layouts, writes the old per-session layout for old sessions, and has explicit operator activation
for new split sessions. Preserve each legacy session's effective bounds/default profile; a zero v8/v9
limit must not silently become the new default. Existing explicit 8 KiB apps need their existing
operator settings. A bridge must retain that recovery capacity, not force old data into 3 KiB.
App name/kinds/declared-limit equality and supported app schemas remain required.

| Source / target | Allowed behavior |
| --- | --- |
| Old v0.2 worker -> bridge/new reader | Restore v8/v9 with its old profile and old layout. Continue old writes for this call. |
| Bridge in legacy mode -> old worker | Only the existing v8/v9 app/registration/operator compatibility permits it. No new fields or budgets. |
| Split v10 worker -> old v0.2 worker | Unsupported: old decoder rejects v10. Never feed it a stale legacy shadow checkpoint. |
| Split worker -> bridge/split reader | Supported after matching registration, bounds and app-schema checks; preserve the split mode. |

Roll **all state-reading/mutating platform participants**, including control and cleanup/listing paths,
to the bridge before activation. Old scripts do not renew/delete the added keys. Keep old workers out
of an activated pool. During the first roll, do not change app identity/defaults or enable v10. During
activation, admit new-format calls to a compatible pool; drain old app pools where declared limits or
Demo A's app schema change. Do not promise automatic cross-registration migration in either direction.
Do not convert active legacy sessions in place in the initial implementation.

Golden tests retain the checked-in v7/v9 fixtures and the existing synthesized v8 coverage. Add a
tracked v8 fixture, self-contained v10 fixtures (empty/full history and Control), component/header
fixtures and cross-layout store round trips. Test source/target directions, zero-limit resolution,
explicit old bounds, uncertain writes and private API rejection. A fixture passing Decode alone is
insufficient: run Restore, takeover and exact move with the supported app schemas.

**Operator action:** deploy the bridge everywhere, verify no old participants remain, then enable
split admission in the selected pool. Defaults change only for split sessions. Keep common keys and
compatible registrations. **Rollback:** turn off new split admissions and retain bridge readers until
all v10 calls end/drain; rollback to v0.2 requires ending those calls first. No downconversion or
history truncation. **Tradeoff:** staged activation costs a release step and limits rollback.
**Change this if:** measured need justifies explicit per-worker capability placement, a deferred ADR
0010 feature; never use an old worker as an unadvertised fallback.

## 5. Cluster and durable-store fit

**Recommendation:** retain the existing session hash tag for every component. Same-slot multi-key
scripts work in Cluster [E1–E2]. Global epoch allocation and worker indexes remain separate keyed
operations, outside the checkpoint script; no cross-slot transaction or database-wide atomicity claim.
Use primary reads for lease, checkpoint and reconciliation. Do not enable replica-read balancing.

Keep cached parameterized Lua and EVALSHA. NOSCRIPT requires loading/executing on the slot's current
primary; failover/reconnect can lose script caches [E3]. `redis.go` already has node-specific reload
for ListByWorker pipelines; extend its key set and test it. Preserve capped routing retries and disabled
blind network mutation retries. MOVED/ASK/TRYAGAIN during migration are not proof a previous timed-out
write failed; settle uncertain outcomes by reading the published tuple [E1]. Ensure TLS/ACL support
for each script command/key, redirected connections and scanning/cleanup on all primaries.

AWS documents **100 MiBps writes/primary** for durable ElastiCache, with Valkey **9.0+**, Cluster mode,
Multi-AZ and a replica required [E4]. MemoryDB documents up to **100 MB/s/primary**, a different unit
[E6], and always-enabled Cluster mode [E7]. Synchronous ElastiCache and MemoryDB acknowledge after
Multi-AZ log persistence and preserve primary consistency through failover; replicas lag [E5, E8].
These are provider capabilities, not Relais throughput/latency measurements. Keep ADR 0011's durable
store direction; local Redis cannot validate managed-store acknowledgement durability.

Write latency affects sequence reservation and commit, not just bytes. Measure the whole path:
store-clock sample -> transport copy -> seal/reserve/commit -> reply, plus tuple-read/transfer/restore.
The checkpoint's commit age cannot hide an old counter copy. Keep ADR 0002's **550 ms** snapshot-age
envelope and existing rate/margin guards; ADR 0008's **30 s** outage budget does not make old counters
safe for takeover. Durable writes may exhaust the envelope or delay forced side-effect launches even
when media stays live. Measure p50/p95/p99 and fault tails before approving the deployment.

Planned move/drain must include final changed history, adoption persistence and survivor contention.
Re-measure task-wait/move reserves against actual store tails; no default drain reserve appears here
(ADRs 0009/0010). Do not widen age, lease or shutdown budgets simply to mask managed-store latency.

[guarantees.md, Capacity and soak](../guarantees.md#capacity-and-soak) plans **about 200 calls per
Redis primary** with headroom because the tracked shared-host echo sample was CPU-bound. Keep that
planning limit; do not scale it by the byte savings. For illustration only, ElastiCache's advertised
write ceiling divided by the projected **70,024.4 B/s payload** is about **1,497 calls** without
headroom, about **749** at 50%; MemoryDB's decimal ceiling gives about **1,428 / 714**. Neither is a
capacity claim: durable-log/TTL writes, script CPU, hot-slot skew, protocol, frame cache, TLS and
replication costs are missing. Measured ceilings can be lower than 200; no increase without a rerun.

**Options/tradeoff:** standalone Redis cannot serve the selected deployment; async failover can
invalidate fencing even with perfect component linkage. Durable Cluster adds latency, log traffic
and failure cases. **Change this if:** managed durability or latency fails the shared acceptance
gates; return the result to the owner and ADR 0011, without weakening ownership safety.

## 6. Measurement and acceptance plan

Prefer free GitHub-hosted Linux runners and `.github/workflows/load-calibration.yml`'s manual
`custom-capacity` path, `relais-load -capacity -worker-bin=... -agent=...`, and
`scripts/ci/custom-capacity-report.py`. No paid/cloud run is authorized by this ADR.

1. **Contract failures first:** Memory and disposable Redis/Cluster tests for stale owner/epoch,
   reordered sequences, bank reuse, missing/corrupt components, wrong digest/version, expiry/renewal,
   release/reclaim, key rotation, script-cache loss and uncertain replies. Inject failures before
   publication and after commit; prove the former restores the previous pair and the latter the new
   pair. Assert unchanged-history ciphertext is not rewritten during progress-only checkpoints.
2. **Default crash proof:** run a real worker subprocess with Demo A and a deterministic keyless
   provider fixture, or delayed-loopback extended with bounded conversation history. Complete the
   four-exchange fixture, wait for a confirmed checkpoint, SIGKILL only that worker, and verify the
   replacement restores exact history bytes and consumes them in the next response. Use no enlarged
   state/store flags. Kill during recording, provider wait and playback; assert preserved prior text,
   OutcomeUnknown for lost move-only data, no fabricated audio and no uncommitted-history claim.
   Test a newer locally accepted but uncommitted history update separately. Mock-provider proof is
   not live paid-provider proof; a live Demo A run remains optional separately authorized evidence.
3. **Matched traffic run:** before/after builds on the same runner/topology, bitrate, buffers and load;
   old build gets the documented Demo A operator overrides, new build uses defaults. Also compare old
   versus new echo defaults separately. Report initial setup and steady state separately, component
   bytes/write, actual change rates, commands/call-second, session/frame/owners request+reply bytes,
   attempted/failed writes, script loads, Save CPU/time and successful writes. Existing TLS counters
   exclude TLS record overhead (`docs/dev/capacity.md`); report NIC/log bytes separately when available.
4. **Recovery and capacity gate:** report checkpoint-age AND snapshot-age p99, maxima and takeover
   outcomes, forced-write latency, move/drain time and N+1 survivors. Keep **100 commands and 128 KiB
   request+reply per call-second**, **550 ms** default p99 age gates, generator validity and loss/
   decryption/cleanup gates (`docs/dev/capacity.md`, `docs/build-an-agent.md`). Include frequent working
   changes and a history-churn refusal case; an idle fixture alone cannot validate the default.
5. **Managed-store gate with ADR 0011:** separately approved/capped run on the selected durable Cluster;
   repeat acknowledged-write failover, slot redirects, TLS, age/latency and loaded move/drain tests.
   Free local Cluster proves protocol behavior, not managed log durability or per-primary capacity.

Publish commit/build, flags, fixture, runner, topology, call count, change rate and raw reports with
each result. No runs of the new schema exist yet: **NOT-RUN — crash survival, traffic savings,
checkpoint ages, default admission and managed capacity.** If the byte adapter/allocation fails,
stop activation; cheapest next data is the local codec/worst-case fixture test, then the free custom
capacity job. Update guarantees and agent guide only with scoped observed results.

## External evidence

Read **2026-10-17**. These sources describe capabilities, not Relais measurements.

- **E1** [Valkey Cluster specification: hash tags, multi-key operations and redirects](https://valkey.io/topics/cluster-spec/).
- **E2** [Valkey EVAL: declare all accessed keys explicitly](https://valkey.io/commands/eval/).
- **E3** [Valkey programmability: atomic execution and volatile scripts](https://valkey.io/topics/programmability/). Cache-loss/pipeline handling is also implemented in the pinned go-redis `script.go` and `pkg/sessionstore/redis.go`.
- **E4** [AWS ElastiCache durability limitations](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Durability.Limitations.html).
- **E5** [AWS ElastiCache synchronous-write consistency](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/Durability.Consistency.html).
- **E6** [AWS MemoryDB replication and per-primary throughput](https://docs.aws.amazon.com/memorydb/latest/devguide/replication.html).
- **E7** [AWS MemoryDB updates: Cluster mode always enabled](https://docs.aws.amazon.com/memorydb/latest/devguide/managing-updates.html).
- **E8** [AWS MemoryDB primary consistency and log persistence](https://docs.aws.amazon.com/memorydb/latest/devguide/consistency.html).

## Decisions for the owner

1. **Use separate working state and history.** Give new agents 3 KiB for changing state and 8 KiB for saved text. Demo A keeps its last four exchanges.
2. **Save the parts together.** A fence means only the current call owner can save them. A crash must restore one matching set.
3. **Keep the current crash limits.** Saved text survives. Current audio and unfinished provider work can be lost or repeated.
4. **Upgrade readers before changing writes.** Keep compatible readers until all new-format calls end. An older worker cannot take over those calls.
5. **Keep the current capacity plan.** Do not promise more calls from smaller writes. Prove the default on free runners first, then test the chosen durable store.

## Implementation plan

Each slice needs owner approval of this ADR before it changes the public contract. File slice issues
from the accepted plan; this design task creates no implementation issues or code changes.

| Slice | Independent demo/measurement | Blockers | Overlap with ADR 0011's Cluster/TLS store-client slice |
| --- | --- | --- | --- |
| A. Component codec and size proof | DTLS changed-counter round trip; v10/component goldens; Demo A split worst-case sizes and hot cap report | ADR approval; stop/revise allocations if actual encoding fails | None beyond agreeing key/format types |
| B. Reader-first store bridge | Legacy fixtures/zero bounds and new self-contained restore decode; split writes disabled; rollback drill | A; shared API/schema agreement | `sessionstore` interfaces, Redis reads, node routing and listing/cleanup; sequence after or integrate with that client change |
| C. Fenced component store | Memory + Redis inactive-bank commit, tuple read, TTL lifecycle, failure injection; unchanged-history byte counter | B; same-slot API | Direct overlap in `redis.go`, scripts, key lists, EVALSHA reload, TTL/cleanup and Cluster tests; one owner integrates these changes |
| D. SDK history and split checkpoint admission | Working/history Save/Restore and exact move; rate/burst refusal, forced intent fence and carried quota | A–C; retain bridge/legacy profile | Store call sites/metrics only; coordinate typed API and retry behavior |
| E. Default conversation SIGKILL proof | Demo A app-schema conversion, keyless provider history fixture and real subprocess crash at each cut; default flags | D; same-registration new pool; app Restore compatibility | Uses the selected client; no independent client rewrite |
| F. Matched traffic and capacity evidence | Before/after custom-capacity reports; churn, N+1, loaded drain and age gates on free runners | E and ADR 0011 client ready for local Cluster variant | Shared Cluster/TLS fixtures and traffic labels; keep baseline/proposed runs comparable |
| G. Managed-store and activation gate | Durable failover/latency evidence, reader-first rollout/rollback, then scoped guide/guarantee updates | F; ADR 0011 store choice and separately approved cloud spend | Joint managed-store acceptance; rollout must include all new lifecycle scripts |
