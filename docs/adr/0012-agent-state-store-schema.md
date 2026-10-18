# ADR 0012: Split session checkpoints and bounded conversation history

Status: **Proposed** 2026-10-17 — awaiting owner approval. Part of #188.
Baseline: `1afa7077a5a28220e9b5662d2f7b974434e89a36` (accepted ADR 0011; runtime baseline v0.2.0).

## Owner summary

- Today the call record is rewritten in full ten times a second. This design splits it into parts.
- Save connection keys when the call starts. Save connection counters and agent progress ten times a second.
- Save the agent's working notes and history only when they change. Refresh their encryption when the encryption key changes.
- Save all parts in one database step that only the worker owning the call can perform.
- Give new agents **3 KiB for notes and 8 KiB for history**. These are proposed sizes, not measured limits.
- Demo A keeps its last four exchanges. This is a short memory, not a full-call transcript.
- Saved text survives a crash. Live audio and unfinished AI requests can be lost or repeated.
- First upgrade every server so it can read and write the new format. Then turn it on for new calls.
- An older worker cannot take over a new-format call. Keep a release that can read and write it until those calls end before rolling back further.
- Do not plan for more calls yet. Prove the default on free runners, then on the chosen database.

## Context and evidence

[ADR 0010, C and Deferred](0010-pluggable-agents.md) requires this split before a default-budget conversation-survival
claim. Current custom agents use envelope v8, or v9 after Control;
`pkg/mediaworker/{state,handover,agent_v2,snapshot,agent_limits}.go` defines the formats and admission.
`internal/agenthost/registration.go` resolves the current 1 KiB app default. Legacy agents retain an 8 KiB default.
Neither the legacy default nor ordinary Save is a durable acknowledgement.

`pkg/sessionstore/redis.go` reserves a fenced sequence, seals the bytes client-side, then commits through Lua.
Its same-slot keys, monotonic epochs, unknown-outcome handling and checkpoint HMAC are safety contracts. Memory
must implement the same observable contract. Accepted [ADR 0011, *Deploy and
autoscale*](0011-deploy-and-autoscale.md) requires Cluster/TLS and a durable managed store. Its client slice
#190 and restart-reconciliation slice #193 share boundaries below.

### Local size inspection, 2026-10-17

This command only decodes tracked fixtures; it opens no sockets and changes no code:

```sh
python3 - <<'PY'
import json, pathlib
size = lambda x: len(json.dumps(x, separators=(',', ':')).encode())
for name in ('v7-787127e.json', 'v9-control.json'):
    d = json.loads((pathlib.Path('pkg/mediaworker/testdata')/name).read_bytes()); before = size(d)
    del d['DTLSConnection']; [d['State']['DTLS'].pop(k) for k in ('Certificate', 'PrivateKey')]
    print(name, before, before-size(d))
h = size([{'user':'u'*512,'assistant':'a'*512}]*4); print('history', h, 'working remainder', 7087-len(',"history":')-h)
PY
```

Output: v7 **3,474 B**, v9 **3,708 B**; deleting certificate/key entries and the whole opaque DTLS connection removes
**2,121 B** in either fixture. Four maximum-size history exchanges occupy **4,205 B**; subtracting that field from Demo
A's documented **7,087 B** worst case leaves **2,871 B**. The latter is a derivation from [the agent
guide](../build-an-agent.md#run-demo-a), not a new live-agent measurement. Source:
`examples/voice-agent/voice/agent.go`, `State` and `boundedText`.

These fixture sizes do **not** prove that the whole DTLS connection is constant. The pinned `github.com/pion/dtls/v3
v3.1.8` (`go.mod`) serializes epochs and `SequenceNumber` alongside secrets and certificates (`state.go`,
`serializedState`, `serialize`; `conn.go`, record writes). Local module-source inspection confirmed this. The
implementation must preserve changing fields. ADR 0010's **2,072 B/checkpoint** saving remains an optimistic planning
input, not a verified split.

## 1. Schema and commit protocol

**Recommendation:** use a bounded current checkpoint, constant setup content, and separate working/history components.
No append-only transcript, result inbox, recording store or unlimited revision chain. Use an additive typed sessionstore
checkpoint API; keep legacy PutState/GetState for old formats. Never silently implement a split checkpoint through
separate uncoordinated PutState calls.

### Keys and contents

Let `P` be the deployment prefix and `T` the existing escaped session tag `{sess:ID}`. Reuse `Redis.keys` escaping,
including its special-ID encoding; reject braces in `P` as today. The v10 lifecycle key set has **11 keys**: five
existing metadata keys, setup, checkpoint and four app banks. The legacy state key is excluded from v10 scripts; old
sessions retain their old path. Declare every accessed key explicitly. A bank is a fixed storage position; prepare the
inactive bank before publishing it. Initialize every app bank key and both setup-bank fields at session start, with
bounded, unreferenced placeholders where needed. Commit never deletes then recreates a key.

| Key, excluding `P` | Contents and write rule |
| --- | --- |
| Existing `lease:T`, `record:T`, `epoch:T`, `routes:T`, `ended:T` | Ownership, ordering and lifecycle metadata; preserve existing contracts. |
| Existing `state:T` | Legacy ciphertext for old sessions only; outside the v10 key set. No shadow dual-write. |
| `setup:T` | Hash with two precreated ciphertext banks: setup generation, certificate/private key, peer identity and invariant DTLS fields. Content is constant; rotation prepares fresh encryption in the inactive bank. |
| `checkpoint:T` | Hash with encrypted v10 transport/progress payload, writer owner/epoch, put sequence, component references, keyed checkpoint digest and store-clock commit time. Publish together in one final HSET. |
| `work:T:0`, `work:T:1` | Encrypted app State.Version and raw working Bytes; replace inactive bank on content/version change or key rotation. |
| `history:T:0`, `history:T:1` | Encrypted raw History bytes; replace inactive bank on history/version change or key rotation. Empty history is an explicit value, not an absent-key fallback. |

The changing encrypted payload contains ICE nomination, mutable DTLS epochs/sequence state, all SRTP/ SRTCP counters and
replay windows, tracks, checkpoint observations, consumed-input progress, host intents/cursors/timers/Control receipts,
move-only presence/length, quota, and registration identity. **Host progress and quota do not increment app content
revisions.** The app pair includes working and history references plus State.Version. The checkpoint hash carries the
exact root revision/digest; its client-known fields are authenticated as AEAD associated data, not repeated in the
ciphertext. Both components are opaque bytes to the host; the app's State.Version covers their joint schema. The
optional history area is for infrequent durable app data, not a host-defined conversation format.

Implement a versioned DTLS adapter for the pinned serialization: separate invariant fields from
LocalEpoch/RemoteEpoch/SequenceNumber, and reconstruct a complete Pion state on restore. Prove round trips and
changed-counter preservation before enabling it. Do not treat a saved opaque MarshalBinary result as constant. Keep
mutable ICE/track fields in the changing payload. Unexpected setup changes require explicit refusal, not silently
reusing old secrets; re-handshake/rekey support is outside this ADR. Encryption-key rotation changes ciphertext, never
the connection secrets.

### Fencing, revisions and encryption

- Preserve the existing sequence-reservation script and its owner/epoch/expiry fence. A reservation
  is not a checkpoint. Sequence gaps are harmless. Encrypt outside media/actor locks.
- Compare exact accepted component bytes and version, not the enclosing host revision or ciphertext.
  Changed/re-encrypted components receive the reserved put sequence as their storage revision; app
  State.Version changes only with its schema. Otherwise reuse committed bank/revision/digest. A changed
  reference pair gets that sequence as root revision; host-only writes retain the root. Compare
  decimal reservation strings; compare fixed-width big-endian revision bytes lexically, never as Lua numbers.
- Use AES-256-GCM per component with fresh random nonces on encryption, a version/key-ID header and
  HKDF domain separation for setup, work, history and checkpoint. Associated data binds session ID,
  setup generation, component kind, revision and header. Bind checkpoint owner/epoch/sequence too.
  Component encryption must survive Transfer: do not require its creation epoch to equal the new lease.
- Use domain-separated HMAC-SHA256 digests for component contents and a canonical length-prefixed
  root tuple `(State.Version, work revision/digest, history revision/digest)`. Do not expose unkeyed
  text fingerprints. Store digests as **32-byte binary** values with key IDs, not hex. Authenticate
  the hash references, setup digest and checkpoint digest as AEAD associated data; do not duplicate
  them in plaintext JSON. Lua compares opaque tuples; clients authenticate and recompute them.

The **commit EVAL** checks the live owner/epoch, expiry, newer put sequence, expected current checkpoint/root, setup
identity, key types, argument bounds and component-reference preconditions before any mutation. It overwrites precreated
inactive component banks and sets expiry. Setup is written at initialization or re-encrypted in its inactive field on
rotation. Prepare the checkpoint hash expiry before publication. Publish ciphertext, packed references/sequence/digests
and `TIME` timestamp as fields `b`, `m`, `t` in **one final HSET to checkpoint:T**. That is the commit point.
Client-known `m` is AEAD associated data; store-generated commit time `t` remains authoritative store metadata, bound to
that publication, rather than a value the client must predict to encrypt. No later fallible metadata update is required
to recognize the checkpoint. Renew/Transfer preserve and extend this record; sequence reservation remains in the lease
hash.

Do not assume script errors roll back prior commands. Inactive banks protect the last published checkpoint if
preparation fails. Preflight types/arguments and put expiry before publication; noeviction and bounded memory are
deployment requirements. Orphan preparations occupy only fixed banks and expire with the lease. Reference and digest
fields include their key IDs so retained components remain verifiable across master-key rotation. A scripted read
finishes before bank reuse. Never overwrite a referenced app/setup bank during preparation. A lost commit reply is
unknown: re-read the primary's tuple, then retry against that tuple with a fresh reservation if necessary. Do not debit
twice or launch a side effect merely because preparation succeeded. Preserve existing bounded retry, ErrTransient,
ErrLeaseLost and ErrStateSuperseded distinctions.

**Master-key rotation:** unlike today's full-blob rewrite on every checkpoint (`redis.go`, PutState), unchanged
setup/history could otherwise retain an old key for an unlimited call. On the first commit under a new active key,
re-encrypt every referenced old-key component, including setup and history, in inactive banks; publish the whole new
tuple last. Rare-part caps add `4,096+8,448 = 12,544 B` (about 12.5 KB) once per session, plus the ordinary hot write.
Fresh nonces/keyed digests are required. A failure preserves the old published tuple; an unknown reply requires a tuple
read before retry. Update the session-store guide: distribute both keys first, switch all writers, then retire the old
key only after every live published tuple references the new key (or its lease has expired) and no old-key command can
still commit. Verify key IDs, including uncertain/draining calls; elapsed lease time alone is insufficient. Unreferenced
old banks need no decrypt key and are never restore fallback.

Local primitive probe, **2026-10-17**, on an owned throwaway non-default-port Redis: `redis-cli -p "$PORT" EVAL
"redis.call('SET',KEYS[1],'new'); error('injected-before-publish')" 1 'work:{probe}:1'` retained its inactive write
after the error. GET/HGET verified the old published bank; final HSET selected the new bank and preserved its
preinstalled TTL (PTTL check). The process was reaped. This was a Redis primitive test, not
schema/recovery/managed-durability validation.

### Restore, expiry and cleanup

Read the lease, published hash, setup and the referenced app/setup banks in **one same-slot EVAL** on the primary.
Authenticate each piece, recompute component/root/whole-checkpoint digests and check revisions, bank identifiers, setup
generation, session ID, lengths and app/host versions. Return one owned complete v10 snapshot to recovery. Preserve the
captured store-clock sample and bind Checkpoint metadata to this exact assembled tuple; a later superseding write causes
a reread, never a mixture. Transfer may legitimately leave a checkpoint written by the preceding epoch.

Missing components, wrong revisions, authentication failures and conflicting generations are unsafe checkpoints. Fail
closed through existing recovery-loss cleanup. Never substitute empty history, an older bank or legacy state. Preserve
ADR 0002's snapshot-age test and margins. This scheme detects a torn combination; the inactive-bank protocol also keeps
an uncommitted preparation from destroying the previous committed combination. No claim covers operator corruption or a
store that loses writes.

Renew and Transfer extend **all** checkpoint/setup/component expiries to the lease's same absolute store-clock expiry,
including unchanged values. Do not refresh a missing required value into existence. Release, expired-lease pruning,
indexed cleanup and fresh Claim delete all component banks and setup, including uncommitted preparations. Stale Release
cannot delete a successor. New claims create a new setup generation. Preserve ADR 0008's **34 s** default lease lifetime
for its **30 s** outage budget; preserve metadata/ended retention and the default **five-minute** retention setting
(`redis.go`, [session-store guide](../dev/session-store.md)). History has no post-call retention or independent lease.

**Options/tradeoff:** a single blob repeats history on progress changes; an unfenced history store can restore
mismatched state. Append logs/unlimited revisions need compaction and retention policy. Fixed banks cost bounded extra
memory and script complexity. **Change this if:** measured long-transcript needs or an upstream versioned DTLS export
justify a separately proven design.

## 2. Defaults, SDK shape and traffic

**Recommendation:** add `State.History []byte`, `Limits.MaxHistoryBytes` and `FactoryArgs.MaxHistoryBytes`; existing
Save/Restore transaction boundaries cover both byte fields. For newly enabled v10 custom sessions resolve zero limits to
**MaxStateBytes = 3,072 B** working state and **MaxHistoryBytes = 8,192 B** history, **11 KiB total**. Retain ADR 0010's
**2 KiB encoded host allowance**, separate move memory and **64 KiB configurable working-state ceiling**. Larger
configured components require actual encoded-size/rate admission; never silently clamp or truncate. An app without
history uses empty History and the working allowance; larger working blobs remain opt-in. These numeric limits are
proposed choices, informed by the size inspection and arithmetic here.

Demo A should move History out of its working JSON, bump its app schema, and use zero/default limits in a new compatible
pool. Its current four exchanges use **4,205 B** of the history allocation and leave derived **2,871 B** working state
under 3,072 B. History alone could hold **seven** exchanges of that exact shape: exact JSON is `7 * 1,051 + 1 = 7,358
B`, while eight use **8,409 B**. Keep Demo A at four; these are byte examples, not a promise about tokens, language,
minutes or an unbounded call. Recheck its actual worst case through Save after the app schema change, including escaping
and non-ASCII text.

Proposed operator allocations for split mode (legacy mode keeps existing flag meanings):

| Allocation | Proposed default | Purpose |
| --- | ---: | --- |
| Hot bytes/checkpoint | 7,168 B | Combined ciphertext/framing for transport/host plus changed working component |
| Setup bytes | 4,096 B | Initial encrypted setup, separately admitted |
| History bytes/write | 8,448 B | Raw history plus a proposed 256 B framing reserve |
| Total checkpoint transaction | 20,480 B | Bounds initialization/forced writes containing all components |
| Successful payload rate | **80,000 B/s** | Gate-derived below; `10*7,168+7,168 = 78,848 B/s` reserves periodic plus one hot forced write/s |
| Payload burst | **40,960 B** | Two maximum transactions: initialization/rotation plus one required write; same two-write minimum as ADR 0010 |

Keep the **100 ms** checkpoint cadence, forced intent barriers and actual byte accounting (`agent_limits.go`, ADRs
0002/0010). Rework validation to reserve the hot checkpoint independently of cold components, rather than `wholeMax *
cadence`. Cold writes share spare refill and burst; the maximum hot/forced pattern leaves only `80,000-78,848 = 1,152
B/s` for them, so a full history cannot change every five seconds under that maximum pattern. Admit against actual
sizes/tokens; the traffic projection below is less than the caps. Debit changed ciphertexts **and published
metadata/field bytes**, carry quota across moves, and reserve a maximum in-flight transaction on exact export. Count
every attempted protocol byte, retries and script loads in traffic gates. History churn does not earn a slower transport
clock: backpressure/refuse the app transaction before acceptance when it cannot fit. Store failure is not app Capacity.
No new delayed-write timer changes crash semantics.

### Arithmetic projections, not measured store traffic

Use decimal B/s. Inputs: ADR 0010's 3,474 B v7 baseline, 2,072 B optimistic setup saving, 2,048 B host allowance and 10
checkpoints/s; current `redis.go` encryption adds **30 B** per ciphertext (2 header + 12 nonce + 16 tag). ADR 0010
conservatively reserved **42 B** for its admission formula; that reserve is not the current measured cryptographic
overhead. JSON base64 of 8,192 B is 10,924 B. The new components carry raw bytes, avoiding that expansion inside store
values. Proposed extra reference reserve is **384 B**: five binary digests `5*32`, five key IDs, five 8-byte revisions,
8-byte owner epoch, 4-byte State.Version, three bank selectors, one format byte, 2-byte owner length, 64-byte owner
allowance and 8-byte commit time total **303 B** (including a separate 8-byte setup generation); three HSET field-name
bytes plus **78 B** allocated framing bring it to 384 B. Pack `m` once, authenticate it as associated data, and count
`b/m/t` field bytes in the hot cap. These are proposed encoding bounds; measure actual names, addresses and encoding,
including any overflow. No second copy of the five digests is budgeted.

| Layout / workload assumption | Projected checkpoint payload and payload B/s |
| --- | --- |
| Old default 1 KiB app, whole blob at 10/s | `(3,474 + 2,048 + 1,368 + 30) * 10 = 69,200 B/s` |
| Old 8 KiB app, whole blob at 10/s | `(3,474 + 2,048 + 10,924 + 30) * 10 = 164,760 B/s`; needs explicit larger settings |
| New changing transport/host, excluding working bytes | `(3,474 - 2,072 + 2,048 + 384 + 30) * 10 = 38,640 B/s` |
| New full 3 KiB working changes at 10/s; 8 KiB history changes once/5 s | `38,640 + (3,072+30)*10 + (8,192+30)/5 = 71,304.4 B/s` |
| New both app components change once/5 s | `38,640 + (3,072+30)/5 + (8,192+30)/5 = 40,904.8 B/s` |

Initialization adds setup once; TTL refreshes, reserve/commit commands and replies still cost bytes. The hot worst-case
estimate `3,474-2,072+2,048+384+3,072+60 = 6,966 B` fits 7,168 B only as a projection. Actual v10 encoding/counter
growth is a release gate, not permission to exceed a cap. Both app fields changing every checkpoint would instead cost
**151,880 B/s payload**; the default rate cannot sustain that. This is why history must be separate and low-churn. Demo
A updates Seen on audio input (`voice/agent.go`); merely splitting its existing monolithic Bytes would still rewrite
history during recording. Its working bytes may change at each checkpoint; the high-churn row models that.

ADR 0010's history comparison rechecks as `10,924/5 = 2,184.8 B/s` versus `109,240 B/s` at 10/s, before encryption. Its
setup projection is `2,072*10 = 20,720 B/s`, **41.2%** of the tracked **50,277 B/s session traffic**, or **24.4%** of
**84,904 B/s total**, from [capacity-results.md, hosted run 38142256615](../dev/capacity-results.md). These ratios
compare projected removable bytes with an older measured echo workload; they are not measured v10 savings.

Derive old session protocol overhead as `50,277-(3,474+30)*10 = 15,237 B/s` from that tracked run; it is a derivation,
not a v10 measurement. Count all **11 key arguments**: for an illustrative 16-byte prefix and fixture's 16-byte ID, the
11 raw names total **512 B**; RESP bulk framing adds `11*7 = 77 B`, plus **8 B** for numkeys, or **597 B/EVAL**. The old
six-key list plus numkeys is **321 B**. Require Clock and sequence reservation to declare only the lease key (**59 B**),
since those operations do not access the other keys. At 10/s, net key-list change is `10*((597-321)+2*(59-321)) = -2,480
B/s`; protocol estimate becomes **12,757 B/s**. Allocate a further **80 B/checkpoint = 800 B/s** for
expected-root/checkpoint arguments beyond the packed fields. These RESP counts are reproducible with
`sum(len(k)+len(str(len(k)))+5 for k in keys)`; they assume the stated name lengths, warm scripts and no retries.
Measure actual escaped IDs/prefixes/commands.

Thus the provisional ceiling is `131,072-31,371-3,256-12,757-800 = 82,888 B/s`; choose **80,000 B/s**, leaving **2,888
B/s** for remaining session overhead, forced-command attempts and retries. Without operation-specific key lists this
allowance does not hold: block activation pending measurements. The high-churn projection plus those clients/overheads
is **119,488.4 B/s**. Burst is **40,960 B**, not a rate increase: two 20,480 B writes cover initialization/rotation and
forced work. Pace fleet rotation, preserve periodic/forced reservations, and keep window-average traffic/age gates; the
policy has no proven per-second burst capacity (`docs/dev/capacity.md`). No larger burst is justified yet. All these are
mixed-workload projections; TLS/log bytes and cold-start script loads need reports.

**Options/tradeoff:** one 8 KiB Bytes field avoids an SDK addition but fails the churn argument; 64 KiB or an unbounded
history log has no supporting budget evidence. Splitting working/history asks apps to classify their data and limits
working state more than an undivided 8 KiB budget would. **Change this if:** actual v10 encoding fails admission,
representative history churn exceeds the rate, or measured Save/restore costs fail the capacity gate. Stop default
activation and return the measurements for a revised allocation; do not raise policy ceilings to make the test pass.

## 3. Crash contract and developer meaning

**Recommendation:** keep ADR 0010's guarantees/exclusions. On SIGKILL takeover, restore the latest valid committed app
pair with its exact consumed-input and task progress. Text in that pair survives, at least through the last acknowledged
checkpoint unless a later committed app update deliberately replaces it. A committed write with a lost reply may also
survive. A later Save or local callback acceptance is not proof of durability. A Control 200 proves the checkpoint
barrier; 202 does not (`docs/build-an-agent.md`, Control receipts). History is bounded app data, not an append-only
audit.

The current recording, ReadOnly request payloads, generated/buffered speech, local mailbox and in-flight provider
operation still do not survive a crash. Preserve OutcomeUnknown and Resumed notices, retry/abandon rules,
duplicate-input windows, stable task keys and checkpoint-before-side-effect launch. Already committed current-turn text
can survive; it is not proof the caller heard the audio. Provider work can repeat and incur charges. Exact planned moves
still carry bounded move-only bytes; this ADR adds no durable audio/results, exactly-once effects or seamless-playback
claim.

SDK docs must say: “Put small changing state in Bytes. Put bounded conversation text in History. Save both together.
Restore both together. Save is not a durable acknowledgement. A crash can lose the current turn's audio and repeat
work.” Keep existing Control receipts and host barriers. **Tradeoff/change condition:** durable recordings/results
require a separate design with explicit costs, acknowledgement and retention; this split does not provide them.

## 4. Migration and compatibility

**Recommendation:** envelope **v10** for split custom sessions; retain v6/v7 legacy and v8/v9 readers. This is separate
from the encryption header version and app State.Version. Private exports remain self-contained: include setup,
working/history and exact move-only payload, never Redis key pointers. Crash store reads assemble that same v10 restore
object. Keep strict private API compatibility.

The **bridge release is the first release containing the full v10 reader, writer and lifecycle scripts**, including
SDK/admission support from A–D. Its flag disables split admission for **new** calls, not writing existing v10 calls. A
bridge adopting v10 must checkpoint v10 and run its renew, transfer and release paths even with new admission off. B/C
are code slices, never deployable bridge releases. No reader-only build is a valid rollout or rollback target for a live
v10 call.

Old sessions retain their layout and effective bounds/default profile; zero v8/v9 limits must not silently resolve to
new defaults. Explicit 8 KiB apps keep their existing operator settings. Preserve app name/kinds/declared-limit
equality, supported app schemas and old recovery capacity.

| Source / target | Allowed behavior |
| --- | --- |
| Old v0.2 worker -> bridge/new reader | Restore v8/v9 with its old profile and old layout. Continue old writes for this call. |
| Bridge in legacy mode -> old worker | Only the existing v8/v9 app/registration/operator compatibility permits it. No new fields or budgets. |
| Split v10 worker -> old v0.2 worker | Unsupported: old decoder rejects v10. Never feed it a stale legacy shadow checkpoint. |
| Split worker -> full bridge/split worker | Restore and continue v10 writes/lifecycle even with new-call admission off; matching registration, bounds and app schema required. |

Roll **all state-reading/mutating platform participants**, including control and cleanup/listing paths, to the bridge
before activation. Old scripts do not renew/delete the added keys. Keep old workers out of an activated pool. During the
first roll, do not change app identity/defaults or enable v10. During activation, admit new-format calls to a compatible
pool; drain old app pools where declared limits or Demo A's app schema change. Do not promise automatic
cross-registration migration in either direction. Do not convert active legacy sessions in place in the initial
implementation.

Golden tests retain the checked-in v7/v9 fixtures and the existing synthesized v8 coverage. Add a tracked v8 fixture,
self-contained v10 fixtures (empty/full history and Control), component/header fixtures and cross-layout store round
trips. Test source/target directions, zero-limit resolution, explicit old bounds, uncertain writes and private API
rejection. A fixture passing Decode alone is insufficient: run Restore, takeover and exact move with the supported app
schemas.

**Operator action:** deploy the bridge everywhere, verify no old participants remain, then enable split admission in the
selected pool. Defaults change only for split sessions. Keep common keys and compatible registrations. **Rollback:**
turn off new admissions; the target must retain the full v10 writer and lifecycle scripts until all v10 calls end/drain.
Only then roll back further to v0.2. No downconversion or history truncation. **Tradeoff:** staged activation costs a
release step and limits rollback. **Change this if:** measured need justifies explicit per-worker capability placement,
a deferred ADR 0010 feature; never use an old worker as an unadvertised fallback.

## 5. Cluster and durable-store fit

**Recommendation:** retain the existing session hash tag for every component. Same-slot multi-key scripts work in
Cluster [E1–E2]. Global epoch allocation and worker indexes remain separate keyed operations, outside the checkpoint
script; no cross-slot transaction or database-wide atomicity claim. Use primary reads for lease, checkpoint and
reconciliation. Do not enable replica-read balancing.

Keep cached parameterized Lua and EVALSHA. NOSCRIPT requires loading/executing on the slot's current primary;
failover/reconnect can lose script caches [E3]. `redis.go` already has node-specific reload for ListByWorker pipelines;
extend its key set and test it. Preserve capped routing retries and disabled blind network mutation retries.
MOVED/ASK/TRYAGAIN during migration are not proof a previous timed-out write failed; settle uncertain outcomes by
reading the published tuple [E1]. Ensure TLS/ACL support for each script command/key, redirected connections and
scanning/cleanup on all primaries.

During legacy resharding, missing declared keys or split locations can cause ASK/TRYAGAIN: a liveness stall, not
permission to weaken fencing [E1]. Precreate every bank key at session start; commits overwrite existing keys/fields and
never DEL-then-create. Initialization or genuinely missing metadata can still stall; retry within existing budgets. Test
**Valkey 9 atomic slot migration** with `CLUSTER MIGRATESLOTS`, including write pause/redirect, failover and cold script
caches [E9].

AWS documents **100 MiBps writes/primary** for durable ElastiCache, with Valkey **9.0+**, Cluster mode, Multi-AZ and a
replica required [E4]. MemoryDB documents up to **100 MB/s/primary**, a different unit [E6], and always-enabled Cluster
mode [E7]. Synchronous ElastiCache and MemoryDB acknowledge after Multi-AZ log persistence and preserve primary
consistency through failover; replicas lag [E5, E8]. These are provider capabilities, not Relais throughput/latency
measurements. Keep ADR 0011's durable store direction; local Redis cannot validate managed-store acknowledgement
durability.

Write latency affects sequence reservation and commit, not just bytes. Measure the whole path: store-clock sample ->
transport copy -> seal/reserve/commit -> reply, plus tuple-read/transfer/restore. The checkpoint's commit age cannot
hide an old counter copy. Keep ADR 0002's **550 ms** snapshot-age envelope and existing rate/margin guards; ADR 0008's
**30 s** outage budget does not make old counters safe for takeover. Durable writes may exhaust the envelope or delay
forced side-effect launches even when media stays live. Measure p50/p95/p99 and fault tails before approving the
deployment.

Planned move/drain must include final changed history, adoption persistence and survivor contention. Re-measure
task-wait/move reserves against actual store tails; no default drain reserve appears here (ADRs 0009/0010). Do not widen
age, lease or shutdown budgets simply to mask managed-store latency.

[guarantees.md, Capacity and soak](../guarantees.md#capacity-and-soak) plans **about 200 calls per Redis primary** with
headroom from a CPU-bound echo sample. Keep that limit; do not scale it by byte savings. Bandwidth quotients omit
durable-log/TTL writes, script CPU, hot-slot skew, protocol, other clients, TLS and replication. Measured limits can be
lower. **Tradeoff/change condition:** durable Cluster adds latency/failure cases; if it fails acceptance, return
evidence to the owner and ADR 0011 without weakening ownership safety.

## 6. Measurement and acceptance plan

Prefer free GitHub-hosted Linux runners and `.github/workflows/load-calibration.yml`'s manual `custom-capacity` path,
`relais-load -capacity -worker-bin=... -agent=...`, and `scripts/ci/custom-capacity-report.py`. No paid/cloud run is
authorized by this ADR.

1. **Contract failures first:** Memory and disposable Redis/Cluster tests for stale owner/epoch,
   reordered sequences, bank reuse, missing/corrupt components, wrong digest/version, expiry/renewal,
   release/reclaim, key rotation, script-cache loss and uncertain replies. Inject failures before
   publication and after commit; prove the former restores the previous pair and the latter the new
   pair. After an unknown-outcome history commit, send a host-only write with the stale expected
   root; assert rejection before any mutation and no history reversion. Check precreated keys during
   legacy resharding and Valkey 9 atomic migration [E9]. Rotation must rewrap unchanged setup/history
   before old-key removal, including long-lived calls and a failure before publication. Otherwise
   assert unchanged-history ciphertext is not rewritten during progress-only checkpoints.
2. **Default crash proof:** run a real worker subprocess with Demo A and a deterministic keyless
   provider fixture, or delayed-loopback extended with bounded conversation history. Standalone
   local Redis suffices for this crash proof. Complete the
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
   attempted/failed writes, all 11 key arguments, HSET metadata/fields, rotation bursts, script loads,
   Save CPU/time and successful writes. Existing TLS counters
   exclude TLS record overhead (`docs/dev/capacity.md`); report NIC/log bytes separately when available.
4. **Recovery and capacity gate:** report checkpoint-age AND snapshot-age p99, maxima and takeover
   outcomes, forced-write latency, move/drain time and N+1 survivors. Keep **100 commands and 128 KiB
   request+reply per call-second**, **550 ms** default p99 age gates, generator validity and loss/
   decryption/cleanup gates (`docs/dev/capacity.md`, `docs/build-an-agent.md`). Include frequent working
   changes and a history-churn refusal case; an idle fixture alone cannot validate the default.
5. **Managed-store gate with ADR 0011:** separately approved/capped run on the selected durable Cluster;
   repeat acknowledged-write failover, slot redirects, TLS, age/latency and loaded move/drain tests.
   Free local Cluster proves protocol behavior, not managed log durability or per-primary capacity.

Publish commit/build, flags, fixture, runner, topology, call count, change rate and raw reports with each result. No
runs of the new schema exist yet: **NOT-RUN — crash survival, traffic savings, checkpoint ages, default admission and
managed capacity.** If the byte adapter/allocation fails, stop activation; cheapest next data is the local
codec/worst-case fixture test, then the free custom capacity job. Update guarantees and agent guide only with scoped
observed results.

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
- **E9** [Valkey 9 atomic slot migration, pauses and redirects](https://valkey.io/topics/atomic-slot-migration/). Read 2026-10-17.

## Decisions for the owner

1. **Store notes and history separately.** Give new agents 3 KiB for notes and 8 KiB for history. Demo A keeps its last four exchanges.
2. **Save all parts in one step.** Only the worker owning the call can save them, so a crash restores one matching set.
3. **Keep the current crash limits.** Saved text survives. Live audio and unfinished AI requests can be lost or repeated.
4. **Upgrade every server before turning the new format on.** Ship a release that reads and writes it. Keep that release until all new-format calls end before rolling back further. An older worker cannot take over those calls.
5. **Do not plan for more calls yet.** Prove the default on free runners first, then on the chosen database.

## Implementation plan

Owner approval precedes public-contract changes. File issues from the accepted plan. This task changes only this ADR.
The **bridge release ships after A, the combined B/C store slice and D are complete**; new-call split admission stays
off until proof/activation. No intermediate reader-only release.

| Order / slice | Demo and blockers | Overlap / ownership |
| --- | --- | --- |
| 1a. ADR 0011 slice 1, **#190: Cluster/TLS client** | Accepted deploy direction; free client/redirect/TLS gates | Owns client/store changes until merged; runs in parallel with A |
| 1b. **A: Codec, DTLS adapter and size proof** | ADR approval; mediaworker-only codec/goldens, counters, binary metadata and actual size report; revise allocations if they fail | Parallel with #190; no edits to `pkg/sessionstore/redis.go` |
| 2. **B/C: One component-store slice, one owner** | Built on #190 and A; all keys/precreation, full reader/writer/lifecycle, tuple read, rotation, failure/migration tests; new admission off | One owner of `redis.go`, scripts and APIs; includes every recovery call site listed below |
| 3. **D: SDK history and split admission** | B/C and A; Save/Restore/moves, gate-derived rate/burst, forced fences, carried quota and legacy profiles | Completes full bridge release; store call sites/metrics coordinate with its owner |
| 4. **E: Default-budget SIGKILL proof** | D; Demo A conversion, compatible new pool and default flags; standalone local Redis plus a real worker subprocess | No managed-store or paid-run blocker |
| 5. **F: Matched traffic and capacity** | E; free before/after reports, churn/rotation, N+1, drain and age gates | Uses #190 client/Cluster fixtures; validates all key/field bytes and proposed caps |
| 6. **G: Paid managed-store/activation acceptance** | F; joint ADR 0011 paid preview acceptance, separate approval/cost cap; full-writer rollout and rollback drill, then scoped guide/guarantee updates | Shared durable-store tests and rotation/key-retirement guide; no cloud spend authorized here |

ADR 0011 slice 4, **#193: control restart reconciliation**, may do control-plane-only work in parallel. Its
session-store changes land **after B/C**, rebuilding recovery state from the final tuple-read API. Never have two
editors of `pkg/sessionstore/redis.go` at once. B/C replaces the split reads in `pkg/controlplane/takeover.go`
(GetState, baseline line 586), `checkpoint.go` (Checkpoint, line 28), `controlplane.go` (GetState, line 789), and
`cmd/crash-run/store_stall.go` (GetState, lines 137/176) with one tuple read, carrying the returned checkpoint metadata
through decisions. Audit every other recovery GetState/Checkpoint caller, including #193; no state-then-metadata pair
remains for v10.
