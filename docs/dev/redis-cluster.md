# Local Redis Cluster tests

Install Go, `redis-server`, `redis-cli`, and `ffmpeg` on PATH. The fixture is
validated locally with Redis 7.0.11. On Ubuntu the `redis-server`, `redis-tools`,
and `ffmpeg` apt packages supply these tools. No Docker or virtual machine is used.

```sh
# Build the real process binaries; race instrumentation covers children too.
go build -race -o bin/ ./cmd/relais-relay ./cmd/relais-worker ./cmd/relais-control ./cmd/relais-signal
RELAIS_TEST_REDIS_CLUSTER=1 make test-cluster
```

Each integration test starts six owned `redis-server` processes in a temporary
directory with `cluster-enabled yes`, three primaries and three replicas. It
reserves free loopback client and cluster-bus ports outside the OS ephemeral
range, using the process driver's port allocator (fallback 20000–29999).
It never uses 6379, 7000–7005, 9101 or 9201. It joins the nodes with
`redis-cli --cluster create ... --cluster-replicas 1 --cluster-yes`, waits for
all nodes and replica links, and stops only those children at cleanup. Failed
tests include their node logs. Local startup is normally about 2–3 seconds.
`make test-cluster` runs packages serially (`-p 1`) so server startup and TLS
fixtures do not overlap the process media timing gates. Each trial has a fresh
store prefix, including relay tenure and route evidence.

Without `RELAIS_TEST_REDIS_CLUSTER=1` or `RELAIS_TEST_REDIS_CLUSTER_ADDRS`,
integration tests skip. They also skip under `-short`. A configured but broken
cluster fails tests. Set `RELAIS_TEST_REDIS_TLS_REQUIRE=1` to fail instead of
skip when the local server lacks TLS; CI requires it. The TLS/ACL fixture
always owns a separate cluster, generates its own certificates and credentials,
requires client certificates, and disables the default ACL user.

For an existing **dedicated disposable** plaintext cluster, use
`RELAIS_TEST_REDIS_CLUSTER_ADDRS=host:port,host:port`. The tests write data,
flush script caches and promote replicas. Do not point this at a shared store.
The TLS test still requires `RELAIS_TEST_REDIS_CLUSTER=1` and owns its fixture.

`make test-cluster` covers storage streams, slot arithmetic, fenced session
state and routes across all primaries, worker indexes, cold-node script reload,
TLS/ACL clients, and the production process call/move/SIGKILL/relay-standby
trials. The live-call promotion occurs before the usual move or worker-kill
event, with media running throughout. It flushes the promoted node's scripts
and checks that ownership epochs, worker indexes and routes survive.

```sh
# Repeat the Cluster and slot tests under the race detector.
RELAIS_TEST_REDIS_CLUSTER=1 go test -race -p 1 -v -count=10 -timeout 25m \
  -run '^Test(Cluster|RedisCluster|Redis.*Slot|SlotCRC16)' \
  ./pkg/storage ./pkg/sessionstore ./pkg/framecache \
  ./internal/processrun ./internal/redistestcluster ./cmd/crash-run
```

Promotion uses coordinated `CLUSTER FAILOVER`, after replica catch-up. This
proves protocol reconnection and fencing, not durability of asynchronous Redis
replication. The process probe uses its existing five-second warmup and
observation gates; it does not claim the full 60-second consent proof. Managed
store durability and provider failover remain separate deployment evidence.

The `cluster-tests` Redis CI job runs on `ubuntu-latest`, installs local Redis and
ffmpeg, builds race-instrumented processes, and runs `make test-cluster` within
a 15-minute job limit. It is separate from the PR `build` aggregate while hosted
reliability is established. No credentials or self-hosted runners are required.
A second `valkey-cluster-tests` job runs the same suite on Valkey **9.1.2**
(released 2026-09-01), outside the `build` aggregate. It builds TLS support from
versioned source after SHA-256 verification. The source checksum is
`19c23908e7d57e8d91ef85b41f5646307582f10f4f0fb999bbf89ed24ec9c983`.
It exposes job-local `redis-server` and `redis-cli` aliases for the existing
fixture; it installs nothing outside the runner job.

The clients use go-redis v9.23.0 native discovery, RESP2 and disabled identity
reporting. Random source order replaces last-good-first. Command/decode errors
continue to another source, with configured dial and I/O timeouts instead of
the former 250 ms cap. Every effective dial address passes the seed policy;
remote loopback announcements use the source host, and TLS-only zero ports use
the source port. Refused nodes fail closed for their slots; the entire map is
not rejected. Dev tools still refuse port 6379 on every host. For N slow
sources, dial-only blackholes can cost **5N seconds** with default dial timeout.
Fresh connections also have handshake and SLOTS I/O budgets: the conservative
DB-0/no-client-name socket bound is **23N seconds**, excluding pool waits and
scheduling. Foreground context deadlines can end discovery earlier; background
reloads can spend the full sweep. See [deployment](../deploy.md) for the formula.

Failure evidence is kept under `RELAIS_TEST_ARTIFACT_DIR`, or under
`relais-cluster-artifacts` in the OS temporary directory when unset. Successful
fixtures remove their own directories. Failure, panic and timeout directories
survive for inspection. CI sets this to `bin/cluster-test-artifacts` and uploads
it on failure. For detached runs whose temporary root is deleted by a runner,
set the variable to a stable, private directory outside that temporary root.

Each Redis node writes `redis.pid`. A private server registry records the exact
child identity and its parent identity. Before starting another cluster, the
harness stops recorded servers only after verifying the parent has exited and
the server's kernel identity still matches. Live parents and reused PIDs are
left alone; unregistered servers are never scanned or signalled. Linux nodes
also receive SIGKILL when the creating harness thread dies. The harness keeps
that thread alive for the child's lifetime. Redis instances predating this
registry cannot be adopted safely and are not automatically stopped.
