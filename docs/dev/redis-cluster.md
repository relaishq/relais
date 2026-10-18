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

The `cluster-tests` CI job runs on `ubuntu-latest`, installs local Redis and
ffmpeg, builds race-instrumented processes, and runs `make test-cluster` within
a 15-minute job limit. It is separate from the PR `build` aggregate while hosted
reliability is established. No credentials or self-hosted runners are required.

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
