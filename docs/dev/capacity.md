# Capacity samples and cost inputs

Build with `make build`; install Redis and ffmpeg first. No external store
is used. Every step owns disposable Redis on a port other than 6379.

```sh
./bin/relais-load -capacity -workers=3 -duration=30s \
  -capacity-start=2 -capacity-max=16 -callers-per-process=2 \
  -output=bin/capacity.json
```

On a disposable Linux runner, add `sudo -E` and `-topology=netns`. Install
iproute2, iptables and procps first. Services run in separate namespaces.
The generator process and its signaling/operator HTTP stay on the management
host. Only its media socket enters the caller namespace, where it binds the
caller's specific public-side IPv4 plan address, as in `crash-run`.
The driver proves private listener isolation before media.

Only the unshaped **LAN** profile is supported. This is the same namespace
plan and cleanup implementation as [crash-run](netns-topology.md), extended
from two to a configurable worker count. The hosted capacity job explicitly
uses `-topology=netns`. macOS refuses netns; local Mac samples use
`-topology=loopback`.

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
skips and causes. The cause label `interval send-lateness p99 peak` denotes
the largest one-second-interval p99, not the whole-step p99. A single saturated
interval makes capacity evidence inconclusive. An independently confirmed
service crash still fails, with the generator cause retained. The #41 soak
policy proposal does not change the generator limits.

## Demonstrated load floor and acceptance

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

A non-retired service exit fails the step, including the child process error,
even when the generator is invalid.
Control-plane takeover counter delta must equal the number of calls affected
by scheduled kills: zero in every other step. A whole-call or retained arrival
gap above 150 ms fails outside the affected call's matched scheduled event.
If that call's generator process has maximum send lateness at least as large
as the gap's excess above 150 ms, the step is inconclusive: a caller stall
can explain the excess even when interval p99 passes. A confirmed service
crash remains a failure. The allowed gap window brackets the event's API/resume
interval. Takeovers allow later tracks to recover within the row's two-second
bound, rather than ending at the earliest track's resume. Both allow up to
150 ms for bounding packets. Another call's event cannot excuse the gap.

`worker_peak_calls` retains each sampled worker-session peak. A survivor
above C blocks approval even when its caller rows pass. The post-kill move
target is selected from the control-plane ownership counts, rather than
assuming a survivor has room. Both kill/move and drain samples reject a worker
peak above C. Drain duration brackets only the control-plane drain request:
`At` follows the initial status read, and `FinishedAt` precedes worker-empty
verification and caller result fan-out. The drained worker's `/status` must
confirm zero sessions. HTTP 207 is a known partial-drain failure, even if it
also leaves event rows incomplete; generator saturation remains inconclusive.

Capacity and calibration pass **no buffer flag** to the relay. Both measure
the standalone product default. The report records `relay_buffer_enabled`
from `relais_relay_buffer_enabled`; missing setup evidence remains null. This
is the running configuration, not an inference from whether a replay happened.
The standalone product default is buffer-on following #35.
The old capacity-only override is removed. Buffer limits and recovery margins
are set by the product, not by this load driver.

## Measured use and store budget

Service `/metrics` exports process-local CPU seconds, current Go heap and
lifetime peak resident memory, using self `getrusage` and runtime sampling.
Redis supplies its own CPU, allocated bytes and current RSS through INFO.
A zero Redis RSS observation means unavailable; allocated memory remains
visible. No external `ps` permission is needed. The media window excludes
setup and hangup, while lifetime peak RSS includes setup. Current heap and
lifetime peak RSS are distinct; a time average of sampled RSS peaks does not
become current RSS.

Every process's CPU seconds and counter deltas are divided by **total
concurrent calls × the shared media-window seconds**. Summing these rates
weights a worker that ran for five of thirty seconds by its actual contribution.
`window_seconds` records that common denominator. Per-process observed windows
are retained only for average memory: memory byte-seconds divided by calls ×
that process's observed seconds. Worker values include
idle workers and are normalized to topology call count, not only that worker's
local calls. Do not sum normalized memory across trial types or interpret a
short shared-host run as independent-host sizing. A killed worker retains its
last complete pre-fault observation, up to one sampling interval old; its
missing tail is not extrapolated. Total RSS/call includes the fixed overhead
of the relay, control, Redis and idle workers. It is not marginal memory per
additional call; compare total RSS across valid steps with the same layout.

Traffic counters cover:

- Worker UDP payload bytes on its media socket, excluding private relay headers.
- Relay UDP writes toward workers (including relay headers) and callers. These
  are forwarded bytes, not NIC receive totals; rejected datagrams are excluded.
- Private HTTP listener and default private API client TCP reads/writes, including
  protocol and scrape overhead. Explicit custom client transports are untouched.
- Redis TCP reads/writes, labelled by `client="session"`, `client="owners"`
  (owners and routes), or `client="frame"`, plus `direction="in"|"out"`.
  These include protocol, ciphertext, retries and script loads. Per-client
  totals are retained in `store_bytes_by_client_per_call_second` for diagnosis.
  For TLS Redis connections the wrapper measures Redis stream bytes, excluding
  TLS record overhead. The hosted topology currently uses plain TCP.
- Redis client command attempts, including each pipeline member and retry,
  labelled `session`, `owners` (lease owners and persisted routes), or `frame`. Lua's internal commands are not counted as
  client operations. Checkpoint write attempts/successes/failures are also
  retained from the existing metrics. INFO monitoring is excluded from the
  application store command and byte budget.

Budgets apply to observed window averages; per-second store burst limits are
not established by this report.

The objective is **500 concurrent calls on one Redis primary using no more
than 50% of its comfortable throughput**. Planning assumptions are
100,000 simple client commands/s and roughly 125 MB/s (1 Gbit/s). The chosen
allowances are **100 commands and 128 KiB of request + reply per call-second**:

```text
commands: 100,000 × 50% / 500 = 100 commands/call-second
bytes:    125 MiB/s × 50% / 500 = 128 KiB/call-second
```

The byte arithmetic uses a rounded binary-unit approximation of the 1 Gbit/s
assumption. At exactly 125 decimal MB/s, a strict 50% allowance is 125,000 B per
call-second; the selected 131,072 B is about 52.4%. Real Redis sizing must
revisit this approximation, command mix, CPU and network limits. Simple-command
throughput is an assumption, not a measured Lua/checkpoint workload rating.

The hosted sample's roughly 67 commands and 85 KB per call-second leave about
33% command-budget margin and 35% byte-budget margin. These observed window
averages fit the assumed budget; they do not validate Redis hardware for 500
calls. Flags can tighten the ceiling but cannot relax it or use nonfinite
values. Exceeding either ceiling fails the step and blocks planned-load
approval. The documented action is event-driven checkpoints followed by
crash/harness acceptance before approval. Checkpoint cadence remains at 100 ms,
with ADR 0002's enforced snapshot-age envelope and margins unchanged.

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
The cost has a `validated` flag. It is calculated only when the planned sample
load is validated, prices are supplied and a relay step passed. Otherwise the
report and console explicitly say `cost=NOT-RUN(<reason>)`, including missing
prices, an unvalidated plan or no passing relay step. At a demonstrated load
floor T, allocated cost/call-hour is an **upper-bound estimate**, assuming the
same workload and prices. It is not an operating recommendation.

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
gh workflow run load-calibration.yml --repo relaishq/relais --ref main \
  -f mode=capacity -f duration=30s -f capacity_max=16 -f callers_per_process=2
```

See [capacity-results.md](capacity-results.md) for the hosted observations and
their limits. Re-run after a change to the product default or acceptance gates.
