# Capacity samples and cost inputs

Build with `make build`; install Redis and ffmpeg first. No external store
is used. Every step owns disposable Redis on a port other than 6379.

```sh
./bin/relais-load -capacity -workers=3 -duration=30s \
  -capacity-start=2 -capacity-max=16 -callers-per-process=2 \
  -output=bin/capacity.json
```

On a disposable Linux runner, add `sudo -E` and `-topology=netns`. Install
iproute2, iptables and procps first. Each service and the media sockets run in
separate namespaces; signaling and generator operator HTTP stay on the
management host. The driver proves private listener isolation before media.
Only the unshaped **LAN** profile is supported. This is the same namespace
plan and cleanup implementation as [crash-run](netns-topology.md), extended
from two to a configurable worker count. macOS refuses netns.

Audio and the harness VP8 fixture are the default workload; these are not
720p camera or production-traffic measurements. `-video=false` selects audio only.

The first ramp concentrates all calls on one worker, then moves them all to
another worker at six seconds. The second ramp spreads calls over W workers
behind one relay. It moves one caller at six seconds. Steps double up to the
configured cap. Each ramp stops at its first service failure or inconclusive
measurement. The relay ramp reserves one move slot below W times the proven worker load.
Thus worker overload cannot be reported as a measured relay limit.

Reports distinguish **lower bounds**, observed service failures, and unusable
steps. Passing a capped ramp does not find the exact worker or relay limit.
The relay packets/s lower bound sums caller-to-worker and worker-to-caller
forwarded datagrams in a valid relay step. It is observed throughput, not a
NIC packet-rate guarantee. Saturation ends usable capacity evidence; it never
becomes a service-capacity threshold. Nothing relaxes the generator's default
20 ms interval p99, 1% skipped-frame, and 85% CPU/core limits in capacity mode.

Use at most four callers per generator process. The main hosted calibration
on run 38133371237 proved four and first saturated at eight per process with
GOMAXPROCS=1. Capacity defaults to two, to leave more pacing margin on a shared
runner. Reports preserve each process's exact caller count, CPU, lateness,
skips and causes. A single saturated interval makes the whole step
inconclusive, including all service claims for that step. The #41 soak policy
proposal does not change this rule.

## Operating load and acceptance

For W workers, measured worker lower bound C and relay lower bound R:

```text
T = max(0, min((W - 1) * C - 1, R))
initial average calls/worker = T / W
maximum calls/survivor after balancing = ceil(T / (W - 1)) <= C
```

One extra free call slot permits a post-failure planned move without filling
its target beyond C. A two-call minimum is required for the fault sample.
The driver SIGKILLs worker zero at six seconds, then moves one call to the
least-loaded survivor at twelve seconds. A separate run drains worker zero
at six seconds at the same planned load. The real control-plane drain API is
used; its returned per-call move results are recorded by the callers.

Generator validity and the caller yardstick are separate. Capacity requires
all event rows to pass, complete records, zero decryption failures, zero ICE
restarts or renegotiations, and no failed/disconnected call. Audio and video
move p95 gaps are checked separately against each caller's preceding one-second
baseline gap p95 +50 ms, and the 150 ms absolute ceiling. At least ten baseline
gaps are required. Takeover gaps must be under two seconds; content loss,
first-new-content and freshness retain the existing harness verdicts. These
short LAN samples do not establish region/far-caller guarantees, sustained
capacity, or the full 60-second consent observation.

`worker_peak_calls` retains each sampled worker-session peak. A survivor
above C blocks approval even when its caller rows pass. The post-kill move
target is selected from the control-plane ownership counts, rather than
assuming a survivor has room. Drain duration is a conservative wall-time bound
including status selection and recording the API results.

The relay's existing caller buffer is enabled only in capacity mode through
`relais-relay -caller-buffer`. The other commands retain their existing default
recovery path. The bounded buffer defaults and takeover margins are unchanged.

## Measured use and store budget

Service `/metrics` exports process-local CPU seconds, current Go heap and
lifetime peak resident memory, using self `getrusage` and runtime sampling.
Redis supplies its own CPU, allocated bytes and current RSS through INFO.
A zero Redis RSS observation means unavailable; allocated memory remains
visible. No external `ps` permission is needed. The media window excludes
setup and hangup, while lifetime peak RSS includes setup. Current heap and
lifetime peak RSS are distinct; a time average of sampled RSS peaks does not
become current RSS.

For each process, CPU seconds and counter deltas are divided by **total
concurrent calls × its observed seconds**. Memory byte-seconds use the same
denominator, yielding average bytes per concurrent call. Worker values include
idle workers and are normalized to topology call count, not only that worker's
local calls. Do not sum normalized memory across trial types or interpret a
short shared-host run as independent-host sizing. A killed worker retains its
last complete pre-fault observation, up to one sampling interval old; its
missing tail is not extrapolated.

Traffic counters cover:

- Worker UDP payload bytes on its media socket, excluding private relay headers.
- Relay UDP writes toward workers (including relay headers) and callers. These
  are forwarded bytes, not NIC receive totals; rejected datagrams are excluded.
- Private HTTP listener and default private API client TCP reads/writes, including
  protocol and scrape overhead. Explicit custom client transports are untouched.
- Redis TCP reads/writes for session/owner and frame-cache clients, including
  protocol, ciphertext, retries and script loads.
- Redis client command attempts, including each pipeline member and retry,
  labelled `session` or `frame`. Lua's internal commands are not counted as
  client operations. Checkpoint write attempts/successes/failures are also
  retained from the existing metrics. INFO monitoring is excluded from the
  application store command and byte budget.

Budgets apply to observed window averages; per-second store burst limits are
not established by this report.

The initial budget is **256 client commands and 1 MiB of request+reply bytes
per call-second**. This gives a 50-call installation an application budget of
12,800 commands/s and 50 MiB/s; it is an explicit sizing ceiling, not a claim
that any chosen Redis machine carries it. Flags can tighten that ceiling.
Exceeding either ceiling fails the step and prevents a validated operating
load. The recorded Mac sample is below both. Checkpoint cadence remains at
100 ms, with ADR 0002's enforced snapshot-age envelope and margins unchanged.
If a hosted measurement exceeds this budget, event-driven checkpoint work and
crash/harness acceptance are required before approving that operating load.

## Cost

Pass `-prices=prices.json` or the workflow's `prices_json`. Every price must be
explicit, finite and nonnegative; zero must be supplied explicitly. No provider
price is embedded. Example shape (these are **synthetic arithmetic inputs**):

```json
{
  "currency": "TEST",
  "worker": {"hourly": 2},
  "relay": {"hourly": 3},
  "control": {"hourly": 4},
  "store": {"hourly": 5},
  "public_egress_per_gib": 6
}
```

```text
machine cost/call-hour = (W * worker/hour + relay/hour + control/hour + store/hour) / T
transfer cost/call-hour = measured public egress bytes/call-second * 3600 / 2^30 * price/GiB
```

Public egress is the measured forwarded UDP payload plus 28 bytes per packet
for IPv4+UDP. The cost records the measured rate and exact decoded price inputs.
It allocates whole machines at the derived occupancy, including the spare
worker. It does not infer prices from CPU utilization, charge internal legs as
public egress, or assume agent/provider fees, discounts, tax or inter-zone fees.
If planned load fails acceptance, the cost remains a candidate estimate, not
an approved operating cost. Without input prices, cost is **NOT-RUN**.

## Hosted workflow

**Load generator calibration** (`load-calibration.yml`) adds a separate
**Capacity sample** job nightly at 09:30 UTC and on manual capacity dispatch,
with no PR trigger. The existing registered workflow supports branch dispatch
without waiting for a new workflow to be registered on main. It uses emulated LAN, three workers, 30 seconds
per step, a 16-call cap and two callers/process. The job has a 20-minute limit
and its measurement step has a 15-minute limit. Reports, runner information
and all child logs are uploaded even on failure. Cleanup is checked before
upload. Checkout does not retain credentials.

```sh
gh workflow run load-calibration.yml --repo relaishq/relais --ref feat/40-capacity \
  -f mode=capacity -f duration=30s -f capacity_max=16 -f callers_per_process=2
```

The orchestrator must push the branch and dispatch that workflow. This worker
makes no GitHub writes. Hosted numbers must be added separately to
[capacity-results.md](capacity-results.md) before posting on #31.
