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
worker first, then select the new KeyID for encryption. Retain old decryption
keys until their snapshots have expired or been replaced. Never reuse a key ID
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
retries stay disabled. A live Cluster deployment has not been exercised here.

Drive `controlplane.Run` with the application lifetime context. It retains and
retries uncertain planned exports as well as crash takeovers; an exact candidate
lease can be adopted, and only settlement proves a transition did not commit.
Redis unavailability can delay a call until recovery or lease expiry.

## Harness and tests

`callharness.Options.SessionStore` chooses the store; nil selects Memory. The
caller owns its lifetime. `RELAIS_TEST_REDIS_ADDR` enables Redis tests under a
fresh random prefix, removed by SCAN/DEL at cleanup. An absent or unavailable
explicit endpoint skips local Redis tests. Set `RELAIS_TEST_REDIS_REQUIRE=1` to
fail when the endpoint is missing or unreachable. PR race unit tests require
Redis. The PR call-harness job supplies `redis:7`
and checks service health. Its short step clears the Redis address and disables
required mode; a separate race step runs `TestRelayUncertainTransferSurvives`
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
