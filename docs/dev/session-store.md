# Redis session store

For the ownership contract and implementation details, see
[`Store`](../../pkg/sessionstore/owners.go) and
[`Redis`, `RedisOptions`, and `TransientError`](../../pkg/sessionstore/redis.go).
The store owns its client; close it after the workers, control plane and relay.

## Configuration

```go
store, err := sessionstore.NewRedis(ctx,
    storage.RedisConfig{Addr: "127.0.0.1:PORT", Prefix: "deployment:"},
    key, sessionstore.RedisOptions{Retention: 5 * time.Minute, KeyID: 2,
        OldKeys: map[byte][]byte{1: previousKey}, IndexGrace: 5 * time.Minute})
```

Supply a 32-byte master key, or pass nil to read `RELAIS_SESSIONSTORE_KEY`
(standard base64). Generate a key with `openssl rand -base64 32` and keep it
outside source control. Empty addresses are rejected. Every process sharing a
prefix needs the same key ring. For rotation, distribute both keys to every
worker and control process first, then select the new KeyID for encryption.
Retire an old decrypt key only after a complete, current inventory across all
primaries reports zero published references to that ID **and** no old-key
writer or uncertain/draining command can still commit. Lease expiry alone is
not sufficient. Failed or stale inventory is not evidence of zero references. Never reuse a key ID
while ciphertext under its former key remains. Per-session subkeys reduce
fleet-wide nonce usage; operators still need to rotate keys before any single
session approaches the AES-GCM random-nonce usage limit.

Retention defaults to five minutes. Set it longer than the maximum lifetime
of an in-flight command, including stalled connections and failover queues.
Metadata and worker indexes expire, while the single global epoch counter is
retained. Do not reset that counter or reuse a cleared namespace with old
workers or commands still alive. Persistence, eviction and failover durability
remain deployment concerns outside this prototype.

IndexGrace also defaults to five minutes and must exceed the two-second
transition command limit. Index entries use Redis-clock publication scores;
listing keeps young uncommitted candidates and settles older ones before
pruning. Epoch rejection is a definite non-commit and permits at most three
attempts, each publishing a fresh epoch. Set grace above any deployment-side
queue or delayed-command lifetime too. Post-commit index repairs run in at
most 32 background jobs, each bounded to two seconds; Close drains them. Workers also extend index
expiry once per renewal tick through a non-droppable operation, alongside the
lease renewals, so maintenance saturation cannot hide live calls.

PutState intentionally uses two scripts. The first reserves an ordered sequence
under the lease fence. The client then encrypts with that sequence in GCM's
associated data; the second script atomically checks the fence, rejects older
sequences, and stores the ciphertext. Redis cannot perform the client-side
AES-GCM encryption, and one round trip cannot return the sequence before the
ciphertext is sealed. Combining them would weaken cross-client ordering or
sequence authentication. A failed reservation simply leaves a harmless gap.

Minimum server version: **Redis 5**, whose effects replication permits writes
following `TIME` inside Lua. Integration tests use **Redis 7** for `PEXPIRETIME`
assertions. Session scripts share a hash tag; worker listing uses a pipeline
that Redis Cluster routes by node. Cluster MOVED/ASK/TRYAGAIN/CLUSTERDOWN
responses are classified as definite-not-run and retried at most three times;
they may still surface as transient if routing remains unavailable. SDK network
retries stay disabled. Local Redis 7 Cluster tests cover NOSCRIPT reload,
legacy slot migration during a component write (liveness only), and process
SIGKILL takeover. These are not managed-store durability or latency evidence.

Drive `controlplane.Run` with the application lifetime context. It retains and
retries uncertain planned exports as well as crash takeovers; an exact candidate
lease can be adopted, and only settlement proves a transition did not commit.
Redis unavailability can delay a call until recovery or lease expiry.

`GET /status` never waits for a key-inventory scan. It reports
`not_yet_scanned` until a sample is available. Scans run only while
`controlplane.Run` is active and are canceled when that lifetime ends.

## Component checkpoints (v10)

`relais-worker -agent-split-checkpoints` opts new calls into v10; it is off by
default. Every worker and control process must support v10 before enabling it.
An adopted v10 call keeps that format with the flag off. Older binaries cannot
restore v10; end all v10 calls before rolling back. v8/v9 calls retain their
legacy state key and encoding. This is the ADR 0012 **bridge release**: it can
read, write, renew, transfer, and release live v10 calls with new-call split
admission off. First roll **all** state-reading/mutating participants (workers,
control, cleanup and listing paths) to the bridge, with common keys and compatible
registrations. Verify that no old participants remain. Then enable admission per
pool. For rollback, turn admission off first and retain a full v10 writer and
lifecycle implementation until every v10 call ends or drains. Only then return to
v0.2. A reader-only build is never a safe rollback target. There is no
history truncation, downconversion, or legacy shadow write.

New v10 custom sessions use 3,072 raw working bytes and 8,192 raw SDK History bytes
when limits are zero. Save/Restore cover both fields as one accepted pair. Legacy
v8/v9 zero limits keep the 1,024-byte working-only profile; v1 keeps its 8,192-byte
working ceiling. Explicit settings keep their meaning. The working ceiling is
configurable up to 64 KiB; larger components require actual encoded-size and rate
admission. Split captures retain the 7,168-byte hot cap, a separate 2,048-byte
host allowance and separate move memory. No state is truncated.

Split rate/burst defaults are 80,000 B/s and 40,960 B, independently of legacy
81,920 B/s and 28,672 B. The hot reservation is `10*7,168+7,168 = 78,848 B/s`,
not the whole transaction times cadence. Cold history and key rotation share
spare tokens, leaving one hot checkpoint. History admission checks the candidate
transaction plus that hot reserve before acceptance, with a maximum transaction
also reserved for an unresolved writer. An admitted pair holds a byte reservation
until a matching acknowledgement; older captures and rotation cannot spend it.
Refusals restore the pair and report typed
Capacity without ending an ordinary call. Successful debits include changed
ciphertexts and published metadata/field bytes. Exact moves preserve quota and
reserve one maximum in-flight transaction. Transport stays on its 100 ms clock
and side effects retain forced checkpoint barriers. Store failure is not app
Capacity.

The additive `sessionstore.ComponentStore` API saves a complete capture with
`CommitComponents` and restores it with one primary `ReadComponents` script.
The returned tuple includes the lease, authenticated components, publication
metadata and store-clock sample. Missing, corrupt or mismatched components
fail closed through recovery loss; history is never replaced with an empty
value, another bank or legacy state. The private v10 restore/export envelope
contains component bytes, never Redis pointers.

All session keys use the existing escaped `{sess:ID}` hash tag. V10 has eleven
keys: the five ownership metadata keys, `setup:T`, `checkpoint:T`,
`work:T:0/1` and `history:T:0/1`. The legacy `state:T` is outside this set;
there is no dual-write. `setup` is a hash with two precreated banks. Both app
banks are also precreated. After the existing fenced sequence reservation,
one commit script checks ownership, epoch, expiry, ordering, expected checkpoint
and app root, setup identity, key types and bounds before any component write.
It overwrites inactive banks, installs expiry, then publishes `b`, `m` and `t`
in one final checkpoint `HSET`. Progress commits retain unchanged setup,
working and history ciphertext. Setup changes are refused. Renew and Transfer
extend every existing key to the same absolute lease expiry; Release, pruning
and fresh Claim remove preparations as well as published values. Lease length
remains 34 seconds and metadata retention defaults to five minutes.

Every changing capture passes encoded-size admission; a refusal is typed and
never truncates bytes. Hot captures use the existing cadence. Unknown commit
replies are settled against the exact published tuple before another capture
can be acknowledged, so a lost reply does not double-debit the write quota.
The durable quota includes the selected commit's actual component write charge.

Components use versioned AES-GCM/HKDF domains and keyed digests. On the first
commit with a new active master key, setup and every referenced old-key part
are re-encrypted into inactive banks before publication. If only rotation's
extra bytes exceed the session write budget, the ordinary commit continues
using old-key references and rotation is retried later. Unreferenced old banks
do not need their keys to restore a published tuple.

Worker and control binaries configure their key rings **only through the
environment**, never key flags or command-line values:

- `RELAIS_SESSIONSTORE_KEY`: active 32-byte key, hex or standard base64.
- `RELAIS_SESSIONSTORE_KEY_ID`: active decimal ID, 0–255; default 0.
- `RELAIS_SESSIONSTORE_OLD_KEYS`: JSON object mapping decimal IDs to hex or
  base64 32-byte decrypt keys, for example `{"1":"<previous-key>"}`. It must
  exclude the active ID. Keep this value in the same protected secret source.

Control `GET /status` exposes `session_store_keys` with `state`, `counts` by
key ID, `unverifiable` record count and `observed_at`. An authenticated partial
scan has state `unverifiable`; its counts remain usable but cannot establish
that a key is safe to retire. One corrupt record or retired decrypt key does
not suppress the other counts. Checkpoint AEAD authenticates the reference IDs
without opening setup/work/history payloads. Inventory scans all primaries and counts only
published references (five for a v10 tuple, one for legacy state). A current
empty count is meaningful only with the retirement conditions above. Stale or
failed scans are labeled and retain their last counts/time; counts are null
before the first successful scan. The cache refresh interval is five seconds,
with each scan bounded to one second. This is a sampled retirement aid, not
a cross-primary transaction. See [ADR 0012 section 1](../adr/0012-agent-state-store-schema.md#1-schema-and-commit-protocol).

## Harness and tests

`callharness.Options.SessionStore` chooses the store; nil selects Memory. The
caller owns its lifetime. `RELAIS_TEST_REDIS_ADDR` enables Redis tests under a
fresh random prefix, removed by SCAN/DEL at cleanup. An absent or unavailable
explicit endpoint skips local Redis tests. Set `RELAIS_TEST_REDIS_REQUIRE=1` to
fail when the endpoint is missing or unreachable. PR race unit tests require
Redis. Tests that start their own standalone Redis require
`RELAIS_TEST_REDIS_REQUIRE=1` or `RELAIS_TEST_REDIS_CLUSTER=1`; an endpoint alone
is not permission to launch a local server. With either switch they strictly
require `redis-server` and a generated `en_US.UTF-8` locale. Without either
switch they skip before checking tools or locales. The PR call-harness job
supplies `redis:7` and checks service health. Its short step clears the Redis
address and disables required mode; a separate race step runs `TestRelayUncertainTransferSurvives`
with Redis required. Full-matrix race and timing steps require Redis for all
Redis harness scenarios.

Use a dedicated throwaway loopback Redis with persistence disabled. Never use
another Redis instance or flush shared data. Storage stream/group tests and
storage benchmarks skip before constructing a
client when `RELAIS_TEST_REDIS_ADDR` is unset, unless required mode is enabled.
Required mode fails on a missing or unreachable endpoint. Benchmarks use random
prefixes and delete only their own keys during cleanup. For an optional skip
check, unset the test address and requirement; there is no test default to 6379.

```sh
RELAIS_TEST_REDIS_ADDR=127.0.0.1:PORT go test -race -count=1 ./pkg/... ./internal/...
RELAIS_TEST_REDIS_ADDR=127.0.0.1:PORT go test -race -count=30 ./pkg/sessionstore
RELAIS_TEST_REDIS_ADDR=127.0.0.1:PORT RELAIS_HARNESS_REQUIRE_FFMPEG=1 make test-harness
RELAIS_TEST_REDIS_ADDR=127.0.0.1:PORT RELAIS_HARNESS_REQUIRE_FFMPEG=1 make test-harness-timing
```

The shared suite covers both stores; media scenarios cover planned moves,
ten-call drains, twenty hard kills, stale snapshots and interrupted keyframes.
Fault tests cover dropped replies, failed settlement, delayed commands and
snapshot replay. Encryption tests scan all keys for synthetic DTLS/SRTP secret
fixtures in raw and base64 forms. `TestLeaseOperationLatency` logs local p50/p99
API latency from 100 samples; these are not production network measurements.
