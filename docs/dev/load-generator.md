# Multi-process harness load generator

Install `redis-server` and `ffmpeg` (video sampling), build with `make build`, then run the same harness callers against real relay,
control-plane and media-worker processes:

```sh
./bin/relais-load -callers=10 -processes=2 -duration=60s \
  -event=10s,move,2,1 -event=30s,kill,0,0
```

`-callers` is the total number of concurrent calls. `-processes` distributes
those calls evenly; 11 callers across 3 processes means 4, 4 and 3 callers.
Each generator defaults to `GOMAXPROCS=1`, independently of the driver and
server processes. `-gomaxprocs` changes that budget. Audio plus the harness VP8
fixture is the default; `-video=false` selects audio only. This is the harness
measurement source, not a real-camera workload or a production codec mix.

All callers connect before the common media start time. Each uses the normal
harness `Dial`, `SendMedia`, `Hangup`, and per-event content yardstick. Recording
keeps 15 seconds of history and samples video decode online every 30 frames.
There is no separate media generator or replacement yardstick.

The command owns every process through `internal/clusterprocess`. It starts
throwaway Redis on a free loopback port with persistence disabled and a data
directory under `bin/load-run-*`. It refuses port 6379. It does not use the
Redis endpoint inherited from the environment. First interrupt cancels the
run and closes children; a second interrupt force-kills owned process groups.
Normal exit, errors and each calibration step also stop and reap children.
A caller waits at most three minutes for `/start`, including a future start
time. It polls its original parent PID every 250 ms and cancels its work on
reparenting. Caller cleanup then closes its connections and operator endpoint.
Logs survive for diagnosis. The existing `crash-run` modes are unchanged.

## External caller test interface

`-external-caller` is a supported test interface for an already running
system. It starts callers and a loopback operator endpoint only. It owns no
relay, control, worker or Redis processes. Build just the caller with
`go build -o bin/relais-load ./cmd/relais-load`; audio-only runs need no Redis
or ffmpeg installation on the caller host.

```sh
./bin/relais-load -external-caller \
  -signaling=https://192.0.2.10/calls -relay=192.0.2.10:41000 \
  -signaling-cert=server.pem -bearer-token-file=caller-token.txt \
  -caller-bind=192.0.2.20:0 -callers=3 -gomaxprocs=1 \
  -duration=60s -sample-every=1s -video=false -output=caller.jsonl
```

The example addresses must be replaced with the real server address and a
local interface address on the caller host. The signaling frontend must
serve the WHIP-style `POST /calls`, `DELETE /calls/{id}` and `GET /status`
with the existing control-plane response formats. Signaling and relay media
addresses are independent; HTTPS success does not prove UDP reachability.

| Flag | External caller meaning |
| --- | --- |
| `-signaling` | HTTP(S) URL ending in `/calls`, without URL credentials, query or fragment |
| `-relay` | Literal relay IPv4 media address with a nonzero UDP port |
| `-signaling-cert` | One PEM server certificate obtained through a trusted channel; optional when system trust is sufficient |
| `-bearer-token-file` | File containing one bearer token; trailing whitespace is removed; requires HTTPS |
| `-caller-bind` | Literal local IPv4 address and UDP port; `:0` gives each caller its own allocated port |
| `-caller-namespace` | Optional existing Linux network namespace; requires `-caller-bind` |
| `-callers`, `-gomaxprocs` | Concurrent callers and this process's Go CPU budget; no child generators are spawned |
| `-duration`, `-sample-every` | Media duration after `/start` and resource sample interval; duration >=1s, samples in [100ms,duration] |
| `-video` | Audio plus sampled VP8 by default; `false` needs no video decoder |
| `-output` | Required JSON-lines output file, overwritten at startup; also written to stdout |
| `-parent-pid` | Optional original parent PID to watch; defaults to the launching process's parent |

Keep the token file private. The token is attached to signaling, status and
hangup requests through the same HTTP client and is never printed in readiness
or caller reports. A pinned PEM replaces system roots and must match the exact
server certificate. Normal certificate validity and IP SAN or hostname checks
still apply. With an IP URL, the certificate must name that IP in its SAN.
Certificate rotation requires a new trusted PEM. No insecure TLS option is
provided. Requests using a pin or token cannot leave the signaling origin,
including redirects and call resource URLs. HTTP without transport flags
remains available for trusted local tests.

With no namespace, `-caller-bind` binds directly on the host. A literal bind
is required; hostnames, wildcard addresses and IPv6 are rejected. Each caller
needs a distinct port, so use port zero for multiple calls. With no bind, the
existing loopback socket and candidate behavior is preserved. Linux namespace
callers still use the existing namespace socket factory. The internal `-child`
mode remains available to the owned process driver.

After all callers connect, stdout emits a JSON readiness line with `type`,
`pid`, `callers` and `http` (a loopback URL). The caller then waits for its
controller to issue `POST <http>/start` with a common start time:

```json
{"at":"2026-01-01T12:00:00Z"}
```

`at` must be an RFC 3339 timestamp. A past time starts media immediately. The
three-minute start deadline includes waiting for a future time. The loopback
operator endpoint also provides `GET /sessions`, `POST /kill` (records a fault
observation), `POST /move` and `POST /record-moves`, using the existing
`internal/loadgen` request types. Move execution needs an operator-capable
signaling endpoint; a frontend that exposes only calls and status will reject
it. The caller never kills external processes. Keep this unauthenticated local
operator endpoint on loopback. It closes when the run ends or is cancelled.

Output contains `stats`, per-call `call` and final `done` JSON lines. Inspect
per-call `error` and reports as well as process stats: process exit alone is
not a media or capacity verdict. This interface supplies caller evidence; it
does not aggregate separate hosts or establish cross-host clock accuracy.
Owned-mode flags such as `-processes`, `-workers`, `-event`, `-capacity`,
`-calibrate` and `-soak` are rejected in external caller mode.

## Generator validity

Every child writes periodic stats and one existing caller report per call as
JSON lines to its `.jsonl` file and stdout. The parent aggregates these into
`report.json` and prints a process table. `-output` selects another report path.
Durations in JSON use nanoseconds; timestamps use RFC 3339.

The default limits are documented inputs, stored in each report:

| Measurement | Default saturation limit | Definition |
| --- | --- | --- |
| Send lateness | p99 > 20 ms in any resource interval | Actual successful UDP write time minus the delivered tick that triggered the frame, once per RTP packet |
| Skipped frames | > 1% in any resource interval | Dropped ticker opportunities / (successful frames + dropped opportunities), summed across audio and video |
| CPU | > 85% per GOMAXPROCS in any interval | Delta of child `getrusage` user + system CPU, divided by wall time and GOMAXPROCS |

The limits are inclusive: exactly 20 ms, 1% skipped frames or 85% CPU does not
trigger saturation. Override with `-lateness-limit`, `-skipped-frame-limit` and
`-cpu-limit`; reports record those choices.
Default sampling is one second (`-sample-every`, minimum 100 ms). The final
sample includes the media tail and hangup. CPU includes all child work during
that interval, including online decode coordination and report construction.
Decoder subprocess CPU is excluded from child `RUSAGE_SELF`; its scheduling
impact remains visible in packet-send lateness. This is a conservative pacing
validity check, not a measurement of the topology's total CPU cost.

The first sample starts immediately. Later samples use the actual timestamp
from the ticker. A short stall can delay a queued tick, but it does not leave
permanent schedule debt in later intervals. Gaps between delivered tick
timestamps count dropped frame opportunities separately. A final accounting
also catches a stall that lasts through the media deadline. Skips are assigned
to the interval where they become observable; one stalled interval still makes
the whole run inconclusive, while later intervals can recover.

All packets of a fragmented video frame share that frame's scheduled time.
The frame count increments once after a successful sample write; fragments do
not inflate the skipped-frame denominator. STUN, DTLS and RTCP are excluded
from packet lateness. Failed UDP writes are not successful sends; caller errors
still invalidate the measurement. Optional callbacks must be fast and safe for
concurrent audio and video sends.

Each caller process has one shared, demand-driven status poller. Callers keep
their existing one-second observation cadence and read the same snapshot.
Periodic HTTP reads use a poller-owned one-second timeout, independent of a
caller's cancellation or deadline. Successful snapshots and poller errors are
cached for one second from completion; caller cancellation errors stay local
to that reader. The harness excludes these periodic observer reads from
planned moves; final reads at `Hangup` do not take that lock, which is safe
because the control plane's `Status` never takes the call coordination lock.
Local in-process harness behavior remains unchanged.

Final collection at `Hangup` bypasses a periodic snapshot and uses a fresh read
with a poller-owned ten-second timeout. Calls finishing within one second of
that final read's completion share its result, so final collection also avoids one scan per
call. The child allows 15 seconds for hangup, covering that read, echo drain
and deletion. A final refresh can follow a periodic read within the same
second; the reported request counts and rates include both.

`status_requests` includes setup and failed requests. Resource intervals report
`interval_status_requests` and `status_requests_per_second`; the process rate
is the weighted average across media and hangup intervals, excluding setup.
Driver event reads appear separately as `driver_status_requests` and their
rate per configured media duration. This avoids one Redis scan per call per
second; each shared status request still reads all live call owners.

Each process reports cumulative p50, p99 and exact maximum lateness, plus its
worst interval p99. Percentiles use bounded 100 microsecond histogram buckets
rounded up; overflow above one second uses the exact maximum. This can be
conservative near a limit. Interval histograms reset; cumulative histograms
retain every successful media packet without retaining packet history.

Memory reports distinguish current Go heap object bytes, Go runtime memory,
and peak resident set size (RSS) from `getrusage`. RSS is a lifetime high-water
mark, including setup, not current RSS. macOS reports RSS in bytes; Linux
reports it in KiB and the sampler converts it to bytes. Parent JSON retains
the last 256 resource samples per process; peak values and saturation causes
cover the whole run. `samples_dropped` records earlier samples removed.
Child JSON lines on disk retain all samples.

`outcome=valid` means generator evidence is usable. It is **not a service
pass**. Any process saturation, missing evidence, caller execution error or
schedule error makes the run `inconclusive`, with the cause. Normal load mode
exits nonzero for an inconclusive run. The existing per-call event verdicts
remain visible, including trusted failures. Aggregated `yardstick` rows keep
those verdicts: any trusted failure fails its metric, otherwise passing needs
at least 80% trusted event coverage. Unknown metrics remain inconclusive.
Rows are separate for each scheduled event (`event_index` is zero-based),
with coverage measured against all `affected` calls. The driver stores the
selected session IDs and action times. Each affected call must supply a record
with the matching kind, worker and action time; a missing record makes the run
inconclusive even when the remaining trusted coverage is above 80%. For kills,
the recorded action must match within one microsecond. This catches records
lost from the control plane's bounded takeover ring. `recorded` reports the
number of affected calls with a matching record. Capacity and soak runners must require valid generator evidence before making service claims.

## Events

Repeat `-event=after,kind,count,worker` with increasing offsets from media start:

- `10s,move,2,1`: move the first two eligible callers to worker 1. Calls already
  on worker 1 or still owned by a killed worker are excluded. Count zero means
  all eligible calls. Selection is deterministic in process/caller order, not a random statistical sample.
- `10s,drain,0,0`: drain all calls from worker 0 through the real operator API.
  Count must be zero. Per-call API results use the same caller move yardstick.
- `30s,kill,0,0`: SIGKILL owned media worker 0, affecting **all** of its calls.
  Count must be zero. Each generator records the driver's kill timestamp so
  the harness's automatic takeover yardstick uses the real fault action.

`-workers` defaults to 3. IDs are 0 through workers minus one. A schedule must
leave one worker alive, have at least 3 seconds before its first event, and
more than 7 seconds after its last event. Moves to a worker already killed
are rejected. Events with no affected calls are measurement errors. A move
uses the control plane's operator HTTP API through a small harness hook and
records the existing per-call move report. For a move, `affected` is the
selected number of calls whose moves were attempted; an operator error or a
partial group makes the run inconclusive. A kill still relies entirely on
missed heartbeats for recovery. No recovery shortcut is added.

The owned command topology is loopback. `internal/loadgen.ChildOptions`
accepts an external harness topology, including its `CallerSocket` factory,
with signaling separate from media. The socket injection matches #36's
factory on main. Full multi-process namespace placement and network profile
control are future integration work; this command does not claim netns proof.
Fixed service/operator addresses use main's `clusterprocess.FreeTCP`
allocator outside the OS ephemeral range (#60). Caller media
sockets continue to use individual dynamic ports.

## Calibration

```sh
./bin/relais-load -calibrate -processes=1 -gomaxprocs=1 \
  -duration=30s -calibration-start=1 -calibration-max=128 \
  -output=bin/calibration.json
```

Calibration doubles callers per process, recreating the topology for each
step, until the first saturated step or the caller cap. The final step reaches
the exact cap even if it is not a power of two. It requires one generator
process and no fault schedule. Each step retains its ordinary report and logs.

`largest_valid_callers_per_process` and `first_saturated_callers_per_process`
form an **observed bracket**, not an exact capacity threshold. Saturation is
an expected calibration endpoint and exits successfully. Missing evidence or
an invalid step without saturation exits nonzero. If no saturation is observed,
`limit_found=false`; the largest tested count is only a lower bound. If the
first step saturates, no positive caller count was proven on that machine.
A short run cannot establish long-run capacity. Shared host CPU and scheduler
noise can lower the result even when the child's own CPU remains low.

The separate **Load generator calibration** workflow runs nightly at 09:30 UTC
and on `workflow_dispatch`, never on pull requests. It uses hosted Ubuntu,
one generator with GOMAXPROCS 1, a 30-second ramp per step capped at 128 callers,
prints the command's report summaries even on failure, and uploads reports,
runner information and all process logs. Checkout does not persist credentials.
The orchestrator must dispatch the workflow after integration. Hosted Linux
calibration is **NOT-RUN** in this worker checkout.

Local run results are recorded in [load-generator-results.md](load-generator-results.md).

For bounded worker/relay ramps, resource and store traffic, N+1 kill/drain
acceptance and input-priced cost, see [capacity.md](capacity.md).

## Soak

`-soak` runs continuous calls with a seeded move/drain/SIGKILL schedule,
worker recycling, a 1% run saturation budget, per-event exclusions, memory
regression by incarnation and shutdown leak inventories. See
[soak.md](soak.md) for the exact gates, capacity inputs and CI dispatches.
Ordinary load and capacity retain their strict any-saturated-interval rule.
