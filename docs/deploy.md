# Run Relais yourself

[Build from source](../README.md#five-minute-quick-start) today. When archives
are available, [install](install.md) them first. The live topology needs
`relais-relay`, `relais-worker`, `relais-control`, `relais-signal`, and Redis.
`relais-demo`, `relais-load`, and `crash-run` are test tools.

## One machine

The [install guide](install.md) provides `start.sh` and `stop.sh` for this full
loopback topology. The manual recipe below is an alternative; do not run both
at once.

This loopback setup is for a trusted development machine. It is not a public
production deployment. Install Go 1.26+ and `redis-server` on PATH with
`brew install redis` or `sudo apt install redis-server`, then run `make build`.
The commands also use `openssl` and `curl`. Check that the example ports are
free; choose a different set if needed. Never use Redis port **6379**.

| Process | Listener | Purpose |
| --- | --- | --- |
| Redis | TCP 16380 | Dedicated disposable session and frame store |
| Relay | UDP 34780 | Caller media; the one public media port in a deployment |
| Relay | UDP 34781; HTTP 34700 | Private worker leg and control |
| Worker w1 | UDP 34711; HTTP 34701 | Private media and coordination |
| Worker w2 | UDP 34712; HTTP 34702 | Private media and coordination |
| Control | HTTP 34710 | Private calls, placement, moves, heartbeats, status |
| Signal | HTTPS 34743 | Authenticated test signaling |

Run the following blocks in one shell from the repository root. Keep it open
until cleanup. The PIDs belong only to these commands; do not reuse them after
processes have exited. This Redis is disposable and has persistence disabled.

```sh
run_dir="$(mktemp -d)"
umask 077
export RELAIS_REDIS_ADDR=127.0.0.1:16380
export RELAIS_REDIS_PREFIX="relais:local:$(openssl rand -hex 8):"
export RELAIS_SESSIONSTORE_KEY="$(openssl rand -hex 32)"
export RELAIS_SIGNAL_TOKEN="$(openssl rand -hex 32)"
redis-server --bind 127.0.0.1 --port 16380 --save '' --appendonly no \
  --dir "$run_dir" >"$run_dir/redis.log" 2>&1 &
redis_pid=$!
```

Wait for `Ready to accept connections` in that Redis process's log before
continuing. Start relay, then workers, then control. Workers retry heartbeats
while control starts. Wait for each readiness check to succeed; stop and read
its log if any command fails.

```sh
env -u RELAIS_SESSIONSTORE_KEY ./bin/relais-relay \
  -media=127.0.0.1:34780 -leg=127.0.0.1:34781 -http=127.0.0.1:34700 \
  >"$run_dir/relay.log" 2>&1 &
relay_pid=$!
curl --fail --retry 30 --retry-connrefused --retry-delay 1 \
  http://127.0.0.1:34700/status

./bin/relais-worker -name=w1 -media=127.0.0.1:34711 -http=127.0.0.1:34701 \
  -relay-leg=127.0.0.1:34781 -relay-media=127.0.0.1:34780 \
  -control=http://127.0.0.1:34710 -agent=demo >"$run_dir/w1.log" 2>&1 &
w1_pid=$!
./bin/relais-worker -name=w2 -media=127.0.0.1:34712 -http=127.0.0.1:34702 \
  -relay-leg=127.0.0.1:34781 -relay-media=127.0.0.1:34780 \
  -control=http://127.0.0.1:34710 -agent=demo >"$run_dir/w2.log" 2>&1 &
w2_pid=$!
curl --fail --retry 30 --retry-connrefused --retry-delay 1 \
  http://127.0.0.1:34701/status
curl --fail --retry 30 --retry-connrefused --retry-delay 1 \
  http://127.0.0.1:34702/status

./bin/relais-control -http=127.0.0.1:34710 -relay=http://127.0.0.1:34700 \
  -worker=w1=http://127.0.0.1:34701 -worker=w2=http://127.0.0.1:34702 \
  >"$run_dir/control.log" 2>&1 &
control_pid=$!
curl --fail --retry 30 --retry-connrefused --retry-delay 1 \
  http://127.0.0.1:34710/status

./bin/relais-signal -listen=127.0.0.1:34743 \
  -control=http://127.0.0.1:34710 -advertise-ips=127.0.0.1 \
  -cert-out="$run_dir/frontend.pem" >"$run_dir/signal.log" 2>&1 &
signal_pid=$!
```

Each media process writes a JSON readiness line with its PID and addresses.
Wait for signal's `https=` startup line and compare the printed certificate
fingerprint before trusting `frontend.pem`. Then check authenticated status:

```sh
curl --fail --cacert "$run_dir/frontend.pem" \
  -H "Authorization: Bearer $RELAIS_SIGNAL_TOKEN" \
  https://127.0.0.1:34743/status
```

Control should list both workers alive. Use [Connect an app](connect-an-app.md)
for the SDP exchange. Both workers select the same audio agent; use `-agent=echo`
on both for plain audio echo. The default worker and demo-launcher agent is echo.
The shared key must encode 32 bytes as hex or base64. Relay must not receive it.
`-redis` / `RELAIS_REDIS_ADDR` selects the store; `-redis-prefix` /
`RELAIS_REDIS_PREFIX` must agree across relay, workers, and control.
`RELAIS_REDIS_PASSWORD` supplies an optional password. TLS and ACL users are
configured through the environment; see [Managed Redis](#managed-redis).
The legacy Redis Cluster setting is not exposed by these process flags.

Stop signal first to prevent new calls. Stop workers while control is still up,
so SIGTERM can drain them. Then stop control, relay, and this owned Redis:

```sh
kill -TERM "$signal_pid"
wait "$signal_pid" || true
kill -TERM "$w1_pid" "$w2_pid"
wait "$w1_pid" || true
wait "$w2_pid" || true
kill -TERM "$control_pid"
wait "$control_pid" || true
kill -TERM "$relay_pid"
wait "$relay_pid" || true
kill -TERM "$redis_pid"
wait "$redis_pid" || true
unset RELAIS_SESSIONSTORE_KEY RELAIS_SIGNAL_TOKEN
```

With no remaining eligible worker, the final worker closes calls after its
bounded drain attempt. For a single worker replacement, keep the relay and
control running and restart that worker with its original name and addresses.
The rejoin handshake fences old sessions before placement resumes. Adding a new
registration is not a general production autoscaling interface; the dynamic
registration routes below are test-only.

## Managed Redis

`relais-relay`, `relais-worker`, and `relais-control` use the same Redis
connection settings. `relais-signal` reaches Redis through control and has no
Redis client of its own. Keep the session-store encryption key configured as
before: TLS protects transport; it does not replace encrypted snapshots.

| Variable | Meaning |
| --- | --- |
| `RELAIS_REDIS_ADDR` | Explicit Redis `host:port`; `-redis` overrides it. |
| `RELAIS_REDIS_PREFIX` | Shared namespace; `-redis-prefix` overrides it. |
| `RELAIS_REDIS_USERNAME` | Optional Redis ACL username. |
| `RELAIS_REDIS_PASSWORD` | Password for the ACL user, or for the default user when no username is set. |
| `RELAIS_REDIS_TLS` | Set to `1` to enable TLS; unset or `0` disables it. Minimum TLS version: 1.2. |
| `RELAIS_REDIS_TLS_CA_FILE` | Optional PEM CA bundle. Unset uses system roots. |
| `RELAIS_REDIS_TLS_SERVER_NAME` | Optional certificate verification name and SNI override. Defaults to the connection host. |
| `RELAIS_REDIS_TLS_CERT_FILE` | Optional client PEM certificate chain for mutual TLS; requires the key file. |
| `RELAIS_REDIS_TLS_KEY_FILE` | Optional client private key; requires the certificate file. |
| `RELAIS_REDIS_ALLOW_LOCAL_6379` | Set to `1` to permit loopback 6379 in production processes; unset or `0` keeps the refusal. |

Cluster nodes often advertise IPs, so set `RELAIS_REDIS_TLS_SERVER_NAME` or use
certificates with IP SANs.

Production processes accept port 6379 on remote hosts. Literal loopback IPs
(`127.0.0.0/8`, `::1`, including mapped IPv4) and `localhost` refuse 6379 by
default. The opt-in exists to protect a developer's own Redis. Unspecified
addresses such as `0.0.0.0`, `::`, or an empty host are always refused.
Localhost subdomains and ambiguous numeric or hex host spellings also count as
local; `0` is unspecified. Other hostnames count as remote; validation does not
look up DNS. Use a dedicated endpoint and check where a hostname resolves when
you configure it.

The demo, `crash-run`, `relais-load`, local deployment scripts, install tests,
and test helpers always refuse 6379 on every host and ignore the opt-in.
The opt-in does not change those tools.

Example for a TLS managed service with an ACL user (placeholder values only):

```sh
export RELAIS_REDIS_ADDR='cache.example.invalid:6379'
export RELAIS_REDIS_TLS=1
export RELAIS_REDIS_USERNAME='relais-service'
# Supply RELAIS_REDIS_PASSWORD from your deployment's secret store.
# For a private CA, also set RELAIS_REDIS_TLS_CA_FILE=/path/to/ca-bundle.pem.
# For mutual TLS, set both RELAIS_REDIS_TLS_CERT_FILE and RELAIS_REDIS_TLS_KEY_FILE.
# Keep RELAIS_SESSIONSTORE_KEY configured on workers and control as before.
./bin/relais-control # Add the remaining topology flags from your deployment.
```

Give each Redis-using process the appropriate credentials and TLS settings.
Certificate verification is always enabled. TLS settings without
`RELAIS_REDIS_TLS=1`, an unreadable or invalid CA bundle, and a missing or invalid
certificate/key pair cause startup to fail. Credentials are environment-only;
do not put them in command-line arguments or logs.

The Go store constructors also accept `storage.RedisConfig.Username` and
`storage.RedisConfig.TLSConfig`, including Redis Cluster connections. The
process flags do not enable Cluster discovery. Legacy plugin runners also use
the TLS and ACL environment settings when their storage type is Redis; existing
storage configuration still selects their Cluster seed addresses. Their addresses
and passwords remain under `RELAIS_STORAGE_REDIS_URL` and
`RELAIS_STORAGE_REDIS_PASSWORD`.

## Other hosts and security

Use a specific relay bind IP for `-media`, not a wildcard. Workers' `-relay-media`
must advertise the caller-reachable address; it can differ from the bind address
behind NAT. Keep worker media, relay leg, Redis, and all private HTTP listeners
on a trusted network. Expose only relay UDP media and the separately protected
signaling entry point. Signal itself binds private or loopback IPs; public
routing needs NAT or a load balancer. This multi-host sketch is not a deployment
validated by this guide. There is no TURN, TCP, or TLS media fallback.

Private HTTP binds allow loopback by default. For private listeners elsewhere,
set `RELAIS_PRIVATE_NETS` to the permitted private CIDRs on each process. That
setting does not add API authentication or encryption. Local users can also
reach loopback APIs. Redis credentials and snapshot encryption do not protect
plaintext private coordination carrying key material. Use host isolation and
network controls appropriate to that trust boundary. See
[explicit non-guarantees](guarantees.md#explicit-non-guarantees).

Keep `-store-outage-budget` matched on workers and control. Inspect readiness
logs, private `/status`, and private `/metrics` on relay, worker, and control.
Signal provides sanitized status but no metrics or move/drain routes.
The relay is one process by default; read [restart conditions](guarantees.md#relay-restart)
and [same-host standby limits](guarantees.md#same-host-standby-relay) before planning recovery.

## Process flags and private APIs

`make build` also produces the individual binaries for manual process runs.
All three require `-redis` (or `RELAIS_REDIS_ADDR`), with an optional shared
`-redis-prefix` / `RELAIS_REDIS_PREFIX`. Only workers and the control plane need
the same `RELAIS_SESSIONSTORE_KEY` (32 bytes encoded as hex or base64). The relay
uses an ownership-only store with no snapshot key or decryption API; start it
without that environment variable (`env -u RELAIS_SESSIONSTORE_KEY ...`). The
driver strips the key from the relay environment. Run each with `-h` for flags:

- `relais-relay`: `-media` (public UDP), `-leg` (worker UDP), `-http` (private
  control HTTP), `-hold-timeout` (default 3 s).
- `relais-worker`: `-name`, `-media` (private UDP), `-http`, `-relay-leg`,
  `-relay-media`, `-control` (control-plane HTTP URL), `-drain-timeout` (5 s).
  `-agent-drain-task-budget` defaults to 2 s; startup rejects values below 0
  or above 2 s. `-agent-drain-move-reserve` has no default: unset skips v2
  handler quiescence. When set, it must be greater than 0 and less than
  `-drain-timeout`, or startup fails. Use a measured loaded move round plus
  mark time and headroom; see [the measurement procedure](dev/drain-quiescence.md).
- `relais-control`: `-http`, `-relay` (relay control URL), and a repeated
  `-worker name=http://worker-address` for each worker. Start the relay and
  worker HTTP listeners before the control plane. Workers retry heartbeats
  while it starts. Each process prints one JSON readiness line with its PID
  and bound addresses; ephemeral loopback ports are the defaults.

WHIP-style means a single WebRTC-HTTP offer/answer exchange, not full WHIP
compliance. SRTP is encrypted media; SRTCP is its encrypted control traffic.
ICE is WebRTC network-path setup.

The loopback control listener serves WHIP-style `POST /calls`, `DELETE /calls/{id}`,
move/drain endpoints and `GET /status`, plus a private heartbeat endpoint. The
worker API wraps create/end/export/resume/status; the relay API wraps worker
registration/removal and move/forget/hold/release. Private HTTP listeners allow
loopback by default, or explicitly allowed private prefixes through
`RELAIS_PRIVATE_NETS`. These unauthenticated APIs are a security limit:
loopback is not a security boundary on a multi-user host. Any local user able
to connect can invoke mutations and read worker snapshot key material.
Snapshots containing key material travel over this private HTTP leg.

## Worker shutdown

On SIGTERM (or SIGINT), `relais-worker` starts one `-drain-timeout` deadline
(default 5 s) and acknowledges control's private mark-only placement fence.
It then refuses raced local creates with a typed retryable error. Heartbeats do
not clear the mark. When `-agent-drain-move-reserve` is set, it stops new v2 task
launches and waits concurrently for handlers, never buffered playout. The wait
is at most `max(0, min(agent-drain-task-budget, remaining deadline - reserve))`,
computed once after the mark. New task starts stay `never_launched` and move
exactly. Unset reserve skips waiting and preserves urgent v2 export behavior.

The worker then asks control to drain its calls through the existing planned
move path, with the remaining deadline. Unfinished handlers use urgent typed
outcomes; buffered audio moves. HTTP, heartbeats, result consumption, pacing and
checkpoints remain live. The local wait holds no control call lock or drain slot.
A mark failure after at most 100 ms skips the wait and falls through to the
existing drain path, which marks as well.
A second SIGINT or SIGTERM during drain uses the default immediate exit.
It then closes. An unreachable control plane or failed drain is logged; the
worker closes remaining sessions after the bounded attempt.

Separate processes share an encrypted Redis frame cache. A resumed worker can
replay cached video; on a cache miss it asks the caller for a fresh keyframe
with a Picture Loss Indication (PLI). The relay also buffers caller encrypted
media for takeover. Neither cache is a durable no-loss log; see
[frame-cache limits](dev/frame-cache.md) and
[buffered takeover](guarantees.md#crash-takeover-with-the-relay-buffer). Session snapshots have an explicit format version. A mixed-version
rollout must first deploy decoders accepting **both old and new snapshot
versions**, then upgrade writers, and retain old-version decoding until old
snapshots are gone. The current single-version decoder does not provide that
rollout compatibility by itself.

## UDP socket buffers

A UDP socket on the OS default receive buffer drops datagrams in the kernel
when its read loop stalls briefly. Every socket on the media path therefore
requests larger buffers:

| Socket | Receive | Send |
| --- | --- | --- |
| Relay media and worker leg | 16 MiB | 4 MiB |
| Worker media | 8 MiB | 4 MiB |
| Call-harness caller | 4 MiB | 1 MiB |

The kernel caps each request without an error. `relais-relay` and
`relais-worker` log the requested and effective sizes at startup, and print a
warning that names the host limit when it caps a request. On Linux, raise the
limits on the host; a container cannot change them. For example, in
`/etc/sysctl.d/60-relais-udp.conf`:

```conf
net.core.rmem_max = 16777216
net.core.wmem_max = 4194304
```

Apply it with `sudo sysctl --system`. Leave `rmem_default` and `wmem_default`
unchanged; they apply to every socket. Linux 6.18 and later default both limits
to 4 MiB; older kernels default to 212992 bytes. `ss -uamn` and `getsockopt`
show double the granted size; the startup log shows the granted size. Check
for kernel drops with `nstat -az UdpRcvbufErrors`. macOS caps each buffer at
`kern.ipc.maxsockbuf`.

## Contributor test tools

### Real processes: WebRTC you can kill -9

For Linux emulated machines, private API isolation, and running inside an
existing Linux VM or container from a Mac, see
[the namespace topology guide](dev/netns-topology.md). For low-latency private
Linux deployments, follow [the private-route TCP policy](dev/private-route-tuning.md).

`make crash-run` builds and starts `relais-relay`, `relais-control` and two
copies of `relais-worker` as separate OS processes. A real Pion caller sends
Opus audio and VP8 video continuously through the relay. The driver reads
`GET /status`, finds the call's owning worker, sends its PID **SIGKILL**, and
keeps the same call running for 60 seconds after automatic takeover. Recovery
uses missed 100 ms heartbeats (400 ms death threshold), encrypted snapshots and
fenced leases in a shared Redis session store. A fenced lease uses an increasing
ownership version to reject writes from the old owner. It never requests an ICE restart
or a second offer/answer exchange.

Prerequisites: Go 1.26+, `redis-server` and `ffmpeg` on PATH. On macOS, Redis
and ffmpeg can be installed with `brew install redis ffmpeg`. This run is
part of the full CI matrix (nightly, pushes to `main`, and manual runs). It stays
out of PR CI.

```bash
make crash-run                              # ten interleaved trials per mode: twenty total
make crash-run CRASH_FLAGS="-runs=1 -verbose" # one trial per mode: two total
make crash-run CRASH_FLAGS="-runs=1 -after=3s" # developer smoke test only
```

The default interleaves ten trials with Redis cache plus PLI and ten with
cache-off PLI: 20 trials total. PLI requests a fresh video keyframe. Each trial
lasts at least 61.5 seconds, including warmup and the post-takeover observation,
so budget more than 20 minutes plus build and startup time. `-runs=1` still runs
one trial per mode, for two trials total. Each trial requires a media gap below
2 seconds, zero decryption failures after the first resumed packet, zero ICE
restarts/reconnects or renegotiations, resumed audio and video, clean full VP8
decode with ffmpeg, and a connected caller with answered consent checks for
at least 60 seconds after takeover. It exits nonzero on failure. The summary
reports first decoded video **and first decoded live video from SIGKILL**, plus
detection time from SIGKILL. Total decryption failures are shown separately from
those after resume. `-after` below 60 seconds explicitly marks the consent
acceptance check `NOT-RUN`.

The driver creates its own Redis on a free loopback port, with persistence
turned off (`--save '' --appendonly no`) and a working directory under `bin/`.
It creates a fresh shared encryption key and a unique Redis namespace for each
run, and stops every child and its Redis on completion, failure or SIGINT/SIGTERM.
Process logs stay under `bin/crash-run-*/`. To use an existing **dedicated**
instance, set `RELAIS_REDIS_ADDR=127.0.0.1:16379`, or pass `-redis=...`; it will
not stop that instance or flush its data. Development drivers and this target
refuse port **6379** on every host, even with the production local opt-in set.
They have no Redis default address.

### Read the crash-run report

The report includes `run`, `gap_ms`, `decrypt(total/after)`, `reconnects`,
`renegotiations`, `decoded_ms`, `live_ms`, `detection_ms`, and `result`.
For accepted measured values and their exact conditions, use the
[native-process table in the guarantee matrix](guarantees.md#crash-takeover-with-the-relay-buffer).
Do not treat an example report as a performance promise.

External call-harness placement uses literal registration names: the zero value
`CallOptions{}` pins to `"0"`, so that registration must exist. Use
`CallOptions{Worker: -1}` for unpinned control-plane placement. Owned topologies
keep their existing worker-index semantics.

### Distributed test registration

For distributed process tests, the control binary has a separate opt-in
`POST /private/test/register` route. Enable it with a repeatable flag that names
each exact private worker IP and its inclusive HTTP and UDP port ranges:

```text
-test-register-worker=10.0.0.10=9300-9399,41010-41109
-test-register-worker=10.0.0.11=9400-9499,42000-42099
```

The request is JSON, for example. Names must contain printable characters,
use at most 64 UTF-8 bytes, and contain at least one non-space character:

```json
{"name":"fresh-2","url":"http://10.0.0.10:9301","address":"10.0.0.10:41011"}
```

Hosts must be literal RFC 1918 or IPv6 unique-local addresses. DNS names,
loopback, public IPs, CIDRs and duplicate host flags are refused. The URL must
use HTTP with an explicit allowed port and no credentials, path, query or
fragment. The media address must use the same host and an allowed UDP port.
The worker's `/status` must report that exact media address before registration.
Worker requests use no environment proxy and do not follow redirects.
Successful registration returns HTTP 200 with `name` and `address`; invalid
requests return 400, and worker, relay or duplicate-registration failures return
409. The relay accepts the verified media address before the control plane can
select the worker. A failed relay update prevents control-plane registration.
`Plane.Register` still rejects reused names and media addresses. Such a rejection
can leave the verified, in-bounds address in the relay allowlist; forwarding still
requires a confirmed caller flow. The route does not replace or revive an old
registration. Use a fresh name and ports for each replacement process. The
existing relay-sync loop continues to restore registrations after relay restarts.

This route is disabled when no `-test-register-worker` flags are supplied.
It does not enable or broaden the loopback-only `-demo-register` route or change
local defaults. Like the other private control routes, it has **no authentication**
and is **private-network only**. Restrict access to trusted test participants.
For a private control listener, explicitly allow its private prefix through
`RELAIS_PRIVATE_NETS`; the worker allowlist does not grant listener access.

### Private coordination internals

Remote requests have a two-second client bound and never automatically retry
media mutations. Resume uses the caller's coordination context when provided.
A lost transport reply is an uncertain outcome. Takeover retries the same
resume payload and lease; an already running matching tenure acknowledges it
without applying another margin. If that registration dies, drains or is
replaced, a different eligible target gets a new epoch and resumes from the
latest stored counters with a fresh margin. An uncertain export falls back to a fenced
single-call crash takeover from the stored snapshot. Planned rollback after
an uncertain resume uses crash margins. The relay private move API requires
`from`, unless `repair: true` explicitly authorizes repairing every authenticated
route of that session when its prior route is uncertain. A missing or empty
`from` without the flag returns HTTP 400.

A returning worker receives a rejoin token only after recovery of its old
leases settles. It fences and drops its old sessions before echoing that token;
an ordinary heartbeat cannot acknowledge rejoin. Repeating the same token is
safe if its acknowledgement response was lost. Placement and lease listing do
not hold the plane lock across Redis I/O; eligibility and reservations are
rechecked under the lock after listing.

Planned handovers resend the same UDP drain barrier every **75 ms** until its
ACK, without extending the **1 s** barrier deadline. After ACK, an abandoned
hold auto-releases in **3 s**, below the shortest 5 s caller connectivity window.
Healthy local coordination finishes in milliseconds. Slow coordination and
worst-case rollback can exceed the 3 s backstop and report an expired hold. It bounds packet holding, and does
not make a crashed control plane durably resumable.

### Driver cleanup

Each driver's Redis, relay, worker and control child has its own process group.
Cleanup sends group signals only while the child leader remains unreaped;
reaping and group signals share a lock so a reused PID cannot be targeted.
Normal completion, errors, SIGINT and SIGTERM terminate live child groups.
The driver keeps signal handling active during cleanup: the first SIGINT or
SIGTERM cancels the run; a second kills and reaps every owned child group,
including its throwaway Redis, before exiting. This driver policy differs from
the worker’s immediate second-signal exit during drain.

macOS has no `Pdeathsig`: if the driver itself is SIGKILLed, cleanup
cannot run and children (including Redis) survive. Find the driver’s printed
`bin/crash-run-*` directory, take the child PIDs from readiness lines and the
Redis startup log, and verify each with `lsof -p <pid>` (command and `cwd` must
match that run). Terminate only those groups with `kill -TERM -- -<child-pid>`;
use `kill -KILL -- -<child-pid>` only for a group that does not stop. Redis uses
no persistence. Do not target another run or the reserved Redis on port 6379.

