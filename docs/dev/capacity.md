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
becomes a service-capacity threshold. Capacity mode cannot change the
generator's default 20 ms interval p99, 1% skipped-frame and 85% CPU/core
limits, or its 1 s sample interval.

Use at most four callers per generator process. The main hosted calibration
on run 38133371237 proved four and first saturated at eight per process with
GOMAXPROCS=1. Capacity defaults to two, to leave more pacing margin on a shared
runner. Reports preserve each process's exact caller count, CPU, lateness,
skips and causes. The cause label `interval send-lateness p99 peak` denotes
the largest one-second-interval p99, not the whole-step p99.

Capacity judges generator validity with the soak's rule (#128, #133). One
isolated sample whose interval send-lateness p99 is above 20 ms but at most
40 ms, or whose skipped frames are above 1% but at most 2%, does not make a
step inconclusive. On the #85 VMs these slips occurred while each 2-caller
generator's whole-step p99 stayed at or below 4.6 ms. Slips of 23-33 ms often
came with two skipped frames of about 160 (1.25%). The step stays
inconclusive for any of these:

- two consecutive samples from one generator above any limit;
- lateness p99 above 40 ms or skipped frames above 2% in any sample;
- CPU above 85% per GOMAXPROCS in any sample;
- a sample above a limit whose interval maximum lateness exceeds 40 ms, so
  its stall could hide a gap the recorder does not retain;
- a process whose earlier samples were dropped, so isolation cannot be shown;
- any cause added after the generator judge, such as an execution error.

The raw `run.outcome` and `run.processes[].saturated` keep the strict
single-sample gate as evidence. Each step records its own `generator_outcome`
and lists the slips it tolerated in `tolerated_lateness_slips`. The console
prints them as `tolerated:` lines. An independently confirmed service crash
still fails, with the generator cause retained.

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
as the gap's excess above 150 ms, that gap is inconclusive: a caller stall
can explain the excess even when interval p99 passes. With a valid generator,
other failures still fail the step; gap ambiguity cannot hide decryption,
takeover-count, yardstick, move-gap or store-budget failures. A confirmed service
crash remains a failure. The allowed gap window brackets the event's API/resume
interval. Takeovers allow later tracks to recover within the row's two-second
bound, rather than ending at the earliest track's resume. Both allow up to
150 ms for bounding packets. Another call's event cannot excuse the gap.

A tolerated slip can still shorten a measured move gap. A stall that covers
the last packet before an outage stamps that packet late, as in the soak
(#129). Each move therefore has a stall bound: the largest interval maximum
send lateness above 20 ms, plus one 20 ms audio tick. Only samples from the
moved call's own generator that overlap [move start - 1 s, media resumption]
count. Samples whose maximum stays within 20 ms add nothing; they are the
existing measurement envelope. The move p95 gates are judged again with each
track's bound added. A measured pass that fails there is inconclusive, never a
pass. A takeover gap below 2 s by no more than its bound is also inconclusive.
A measured failure still fails, and no track leaves a percentile.

The same stall can shorten an unscheduled whole-call interruption. The
recorder therefore retains every gap above 90 ms: 150 ms minus the largest
tolerated stall (`callharness.MaxToleratedStall`, two 20 ms lateness limits
plus one 20 ms audio tick). Capacity judges every retained gap, its longest
gap and its rolling arrival history. An unscheduled gap above 150 ms keeps the
rule above. An unscheduled gap at or below 150 ms is inconclusive when the gap
plus its stall bound exceeds 150 ms. That bound uses this caller's generator
samples that overlap [gap start - 1 s, gap end]. A full gap inventory may have
dropped a gap, so it is also inconclusive. A capacity step of at most three
minutes cannot fill the 4096-gap inventory.

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

## Matched split-checkpoint measurement

Manual `mode=split-capacity` runs eight samples in one `ubuntu-latest` job.
It builds once, then runs history before/after and echo before/after back to
back at the same fixed call count, bitrate, relay buffers and duration.
The history baseline uses the documented Demo A legacy operator overrides
(24,576 B snapshot, 270,336 B/s, 98,304 B burst) and an 8 KiB app declaration.
The split arm uses the product defaults and `-agent-split-checkpoints`.
This measures a fixed load on one runner. It does not establish a capacity
limit, independent-host topology, managed-store durability or NIC/log traffic.
The selected topology is separate processes over loopback on both platforms.

`split_callers` selects 1..16 calls (default 2). `duration` selects 20..60 seconds
(default 30) per sample; rolling rotation has at least 65 seconds. One generator
per call keeps the hosted caller load fixed. The job has a 40-minute limit and
a 30-minute measurement-step limit. Manual dispatch selects the ref:

```sh
gh workflow run load-calibration.yml --repo relaishq/relais --ref BRANCH \
  -f mode=split-capacity -f split_callers=2 -f duration=30s
```

The keyless `delayed-loopback history` fixture changes 2,871 B of working JSON
on each audio input (nominally 50/s). Its four exchanges contain 512 B per side:
4,205 B history, updated once per 250 inputs (nominally five seconds). Legacy
Save holds 7,087 B inline. Split Save holds 2,871 B in Bytes and 4,205 B in
History. The fixture retains the original delayed audio loopback behavior.
A keyless no-op SideEffecting task once per turn exercises the forced checkpoint
barrier; it performs no external effect. The churn variant attempts history
changes on each input after input 400; a Capacity callback acknowledges refusal
without retrying the rejected history or ending the call. Actual accepted
component write rates, refusal counts and sampled input progress are recorded.

Every sample owns a fresh Redis on a free non-6379 port. An opt-in plaintext
RESP proxy counts exact client request/reply frames, attempts, failures, script
loads, all declared commit key arguments and per-component ciphertext lengths.
A separate Redis MONITOR stream counts actual HSET field names and value lengths,
including Lua's setup, checkpoint and lease metadata. It retains lengths only.
Both arms pay the same observation overhead; MONITOR traffic and direct driver
INFO, inventory and cleanup reads are excluded from application byte budgets.
The setup (connection plus pre-window warmup), steady, planned move, rotation,
hangup and cleanup phases are separate. Each matched traffic sample includes
one identical planned move probe; its Redis request/reply traffic has its own
phase and stays in the complete media-window gate. One-second bins
show rotation bursts; they do not establish a per-second burst allowance.
History churn has its own observer phase from media StartedAt + 9 s, after the
input-400 ramp and the planned move. Both the load verdict and report apply the
100-command/128-KiB gates to that phase independently of the complete media
window. A missing or truncated churn phase blocks evidence; averaging the ramp
with churn cannot turn a sustained excess into a pass.
Per-client session/frame/owners rates still come from the application wire
counters. Successful component sizes exclude failed writes, while protocol
traffic includes every observed failed attempt and script body. Warm EVAL body submissions are counted separately
from actual SCRIPT LOAD commands and NOSCRIPT replies. Internal Redis script
compilation/cache-hit counts are not inferred from a warm EVAL submission. A second traffic gate
amortizes **all** observed lifecycle requests/replies (including setup, cold
loads, hangup and the standard cleanup wait) over the sample's media call-seconds.
This keeps out-of-window script traffic in the gate without hiding the separate
setup and steady measurements.

The split-only samples add history-churn refusal, rolling master-key rotation
through planned drain/replacement, N+1 SIGKILL recovery followed by a planned move on the survivors, and a separate drain.
All workers and control initially read both keys; replacement workers make key 2
active and retain key 1 for decryption. The authenticated live-tuple inventory
must count exactly five new-key references per call and zero old-key references.
The harness records every drain, takeover, replacement rejoin and final live
owner count. Cleanup checks session/component absence after the standard relay wait, bounded retained metadata,
owned process exit and released listeners before the owned Redis stops.

`scripts/ci/custom-capacity-report.py` also accepts the resulting split report.
It retains an over-budget legacy baseline as an observation and blocks every
other service failure. Split measurements retain the 100 commands, 128 KiB
request+reply/call-second, 550 ms p99 checkpoint **and** snapshot age, generator,
loss, decryption and cleanup gates. Max ages are reported separately.
Application Save and host state admission have separate wall/thread CPU counters.
Timing adds overhead to both arms. Mac thread CPU is unavailable and explicitly
blocks hosted approval. The workflow uploads raw JSON, every child log, exact
binary hashes, runner details and the gate report even on failure. Hosted
results and ADR projection comparisons are recorded in `capacity-results.md`
from the uploaded artifacts.

## Lean preview: external callers and two deployed workers

`relais-load -lean-capacity=manifest.json -output=lean-report.json` judges
artifacts from an **already deployed** fixed pool. It starts no services,
connects to no store/provider, and does not control a remote machine. Existing
`-capacity` and `-soak` policies, their three-worker requirements and their
refusal of distributed point estimates remain unchanged. Other mode/config
flags are rejected by the lean entry. `-prices=ledger.json` can supply the
ledger instead of the manifest's `prices` object.

The approved workload is **T=1 survivor-safe call**, with two ready workers
and observed `WorkerCallCap=2`. The cap is an admission limit, not a measured
worker capacity. A separate fault-free boundary phase fills both workers to
two calls each, then attempts a fifth. It must return `503`, JSON code
`at_capacity`, and `Retry-After: 1`. Authoritative before/after ownership and
reservations must be identical, the refusal counter must increase once, and
the four existing calls must retain valid media. This phase demonstrates a
bounded two-call/worker floor; it does not authorize four survivor-safe calls.

Preview limits remain: one worker crash leaves no destination for another
move. Restore/rejoin the second worker before each subsequent trial. A drain
needs independent zero local sessions, ownership, reservations and transfers.
The template's fixed worker names (`w1`, `w2`) remain unchanged across
replacement. The lean manifest pairs each name with an explicit process
generation and requires a fresh session ID for every phase. A kill must
produce a different restored generation, and its successor must match the
next phase's starting generation. Keep each incarnation's counters separate;
local PIDs are not unique across caller runners. Receipts preserve actual
server worker names and ownership epochs. This bounded fresh-session policy
does not relax the general distributed protocol's guidance for a long-lived
call across replacements; reusing a call between lean phases is refused.

### Schedule and private-runner contract

Print a machine-readable preflight budget without starting anything:

```sh
./bin/relais-load -lean-plan -output=lean-plan.json
```

The minimum serial schedule budgets **40 minutes (2,400 seconds)** against
the private stage's **120-minute** gate window:

| Work | Budget |
| --- | ---: |
| 20 planned moves | 20 × 30 s = 10 min |
| 10 kills/takeovers and 2 drains, with callers live through restore | 12 × 120 s = 24 min |
| Four-call admission boundary and fifth-call refusal | 2 min |
| Caller setup and final cleanup observation | 4 min |

Each recovery trial runs one call, with at least 3 s warmup and more than 7 s
recovery tail. A move uses 30 s. A kill/drain uses 120 s, including the
90 s restore allowance; the caller must remain live until restore is proven.
Restore must complete within the explicit 90 s budget. This is
an unmeasured planning timeout, not a cloud boot guarantee. Expiry is
inconclusive; do not reduce the 20 move/10 takeover counts. The report scales
`expected_wall_seconds` to submitted phase durations/counts, retaining at
least these per-phase budgets, and warns when that schedule exceeds an hour.
It never grants full-soak acceptance: the existing 50-concurrent-call,
one-hour and memory qualification requirements still apply to that claim.

The private acceptance runner owns deployment/fault/teardown operations. It
launches the existing external caller on each selected free hosted runner:

```sh
./bin/relais-load -external-caller -distributed -run-id=RUN_ID \
  -signaling=https://SIGNAL_HOST/calls -relay=RELAY_IPV4:PORT \
  -bearer-token-file=TOKEN_FILE -caller-bind=CALLER_IPV4:0 \
  -frontend-control-bound=MEASURED_BOUND \
  -frontend-worker-bound=MEASURED_BOUND -clock-drift-ppm=MEASURED_PPM \
  -duration=30s -sample-every=1s -event-retention=180s \
  -callers=1 -output=caller.jsonl
```

Use `-duration=120s` for kill/drain phases and the boundary. A producer may
submit shorter completed fault trials only when restore was already proven
within the caller lifetime; the preflight budget still reserves 120 s each.
Use the existing pinned-certificate option when required. Never put the token
in the manifest or command arguments. Runner-local operator endpoints remain
loopback; drive `/start`, `/arm-event` and `/record-events` through the
runner's own trusted execution channel. Never expose private service APIs or
caller operator endpoints publicly. Use the identity, arming margins, actual
worker-host SIGKILL origin and durable control receipt contract in
[distributed evidence](distributed-evidence.md). Missing or uncertain
operations stay in the artifact. After every trial, close its callers before
creating the next trial's callers. Observe final session/relay/store cleanup;
leave deployed services running for their owner to tear down.

### Manifest v1

The input is one strict JSON object, at most 64 MiB. Duplicate keys at any
depth (including escaped spellings), unknown fields and trailing objects are
errors. Paths/credentials for the private runner are not inputs.
Durations use integer nanoseconds; timestamps use RFC3339. The Go definitions
in `cmd/relais-load/lean.go`, `internal/loadgen/report.go`, and the existing
caller/telemetry report types are the wire schema. `TestLeanEntryIsSeparate`
round-trips a complete synthetic manifest through the actual CLI entry.

| Field | Required evidence |
| --- | --- |
| `version`, `run_id`, `region`, `build`, `agent`, `payload` | Version 1 and exact workload/build identity |
| `signaling`, `relay` | Credential-free HTTPS URL and specific IPv4 UDP endpoint |
| `store_durability`, `store_tls`, `relay_buffer_enabled` | `sync`, true, and observed buffer setting |
| `clock` | `frontend_control_bound_ns`, `frontend_worker_bound_ns`, `drift_ppm`, and an `independent_measurements` reference; zero is an explicit measured assumption |
| `callers` | 1–4 unique `id` values, each with external `runner`, `region`, and specific IPv4 `bind` ending in `:0` |
| `phases` | Ordered unique IDs, kind `move`, `kill`, `drain`, or `boundary`; at least 20/10/2/1 respectively; exactly one boundary |
| `cleanup` | Existing `loadgen.LeakReport`: independent checked inventories, relay inactivity wait, retention/scan positive controls; no leaked callers, sessions, keys or owned listeners |
| `topology` | `cloud` by default, including when omitted; only explicit `local` waives required cloud ENA evidence |
| `ena_required` | Legacy opt-in: true also requires ENA for local topology; false never waives cloud evidence |
| `prices` | Optional complete dated ledger described below |

Each phase uses exactly one external generator: one call for recovery, four
calls for the boundary, within the existing calibrated four-call/process cap.
Different phases may use different caller runners/regions. This keeps raw
generator times on one caller clock per phase; the policy refuses a joined
multi-runner phase rather than treating different clocks as equal.

Each phase contains `caller_ids` aligned with its `run.processes`, fresh
`before`, `after`, and `restored` pool observations, and the unmodified
`loadgen.RunReport` in `run`. A pool has `at`, `age_seconds`, `stale`,
`worker_call_cap`, and exactly two `workers`. Each worker has its registration
`name`, unique process incarnation `generation`, `ready`, accepted `sessions`, and
incoming `reservations`. Pool `at` timestamps use the control host clock.
`completed_at` is a control-clock observation after the phase callers close.
The judge maps before/completion times onto the caller clock with its probes
and requires the entire media lifetime inside the conservative bracket. The
next phase cannot start before the previous completion. Pools must be fresh
within one second. Registration
and generation continuity must hold between restored and next-before pools.
Recovery phases have one matched event and one timing bracket. `timings`
contains its complete operation `identity`, `caller_arm_acknowledged_at` and
`caller_receipt_sent_at`, captured on that caller runner's clock. Use the arm
acknowledgment completion and receipt-request send times as conservative
brackets; mapped events must fit entirely inside them. Drain phases also
supply `drain_empty` naming the drained `worker` and its `generation`, with
`receipt.End <= drain_empty.at <= restored.at` on the control clock. The
generation must match both the source and restored pool. The boundary supplies
`admission` with HTTP result, pool snapshots and refusal-counter observations.
Its `before.at <= after.at`, and both observations must fall wholly inside
every accepted caller's connected media window after clock mapping. Earlier
or later snapshots cannot establish that the refusal left four live calls
undisturbed.

Service telemetry needs an independent bound on alignment to the phase's media window. Supply
`telemetry_window_error_ns` and the independent
`telemetry_window_evidence` reference. This is a producer requirement;
frontend clock probes alone do not establish service-window alignment. The error bound must be less than half the phase duration. Store
budgets use the conservative call-second denominator `calls × (duration -
2 × alignment_error)`; the corresponding lower rate uses `duration +
2 × alignment_error`. A threshold straddle is inconclusive; only a breach
across the whole interval fails. Both bounds are reported. Missing/reset
counters block approval. Supply
raw first/last counters, resources and 100 ms per-session age samples.
Service process names are `relay`, `control`, `store`, and
`worker-NAME@GENERATION`; replacements have separate process observations.
Normalize against total concurrent calls × the shared media-window duration;
the judge recomputes rates from raw counter deltas, retaining measured egress
separately from the conservative store-budget denominator.

The judge pools identity-matched distributed trials across **serial** phases
only for trial counts, interval verdicts and move-gap p95. It never treats the
number of serial report records as concurrent occupancy. It reconstructs clock
maps from probes, checks receipt/identity/bracket agreement, and retains all
original distributed fields. Scalar event start/end, per-event track gaps and
resume point estimates cannot establish acceptance. Clock straddles, missing
receipts, incomplete coverage and unknown telemetry remain inconclusive.
Known call loss, corruption or independently confirmed service failure still
fails when timing or generators are uncertain. Caller-local checks run before
clock validation: a gap over two seconds or content loss outside the matched
caller-local arm/receipt bracket plus the 500 ms delivery settle window fails
even with invalid clock probes. The inclusive bracket runs from arm acknowledgment
through receipt send plus `soakDeliverySettle`. Loss inside that bracket still
needs valid interval evidence; the bracket alone
cannot approve it. Missing per-track whole-call gap inventory is inconclusive;
inventory overflow fails, including when the recording flag is absent.

The existing store ceilings (100 commands and 128 KiB/call-second), 550 ms
checkpoint **and** snapshot p99 with 100 ms coverage, generator isolation/stall
rules, whole-call delivery, event loss/freshness/decoded-content rows,
per-track interval move p95, 150 ms unscheduled gaps, <2 s takeover and actual
kill-to-live, survivor occupancy, takeover counters and cleanup all apply.
Every required lean phase must be conclusive. Memory observations retain the
existing trend diagnostic, including `insufficient-data` for short trials.
The report explicitly sets `memory_gate.outcome=not-applicable`: phases are
capped at three minutes, below the trend judge's five post-warm-up minutes
and ten-minute worker-incarnation minimum. This gate is excluded from the
lean verdict; it is not a memory pass or soak memory qualification. Independently
observed memory growth failures still fail. `TestLeanShortPhaseMemoryExcluded`
checks this distinction. No phase average can hide another phase's failure.

Each `allowances` row has `host`, `generation`, `first`, `last`, and
`conntrack_remaining`. Include all four ENA counters:
`pps_allowance_exceeded`, `conntrack_allowance_exceeded`,
`bw_in_allowance_exceeded`, `bw_out_allowance_exceeded`.
Any positive delta fails; missing/reset supplied counters are inconclusive.
Cloud topology requires relay, shared control, and every worker generation;
missing rows make the phase and `cloud_qualification` inconclusive. Only an
explicit local topology may omit these rows; its report carries
`cloud_qualification.outcome=NOT-RUN` with the local-topology reason, even when
the service verdict passes. `TestLeanCloudAllowanceDefault` and
`TestLeanLocalCloudQualification` cover both cases.

### Dated full-stack ledger

Lean prices use a separate schema from the legacy local capacity price file.
Provide `currency`, matching `region`, `date` (`YYYY-MM-DD`), `source`, and
`lines`. Every line has `category`, `resource`, `meter`, `quantity`, `rate`,
and `steady_quantity_per_hour`. Quantities cover the actual allocation window;
rates and units come from the dated tariff. A resource charged under multiple
categories or a duplicate resource/meter is rejected so shared hosts and
transfer are not counted twice. Use distinct charge-component resource IDs
for separate billed items such as nodes and their sync premium. Any explicit zero
also needs `zero_reason`. Missing quantities, rates, categories or dates are
listed in `cost.unknown`; none silently become free.

Required categories are `workers`, `relay`, `control_signaling`, `store_nodes`,
`store_sync`, `public_egress`, `inter_az`, `ingress`, `nat_endpoints`, `disk`,
`ipv4`, `observability_api`, `secrets`, and `retained_surge`. Count both workers,
the relay host including its same-host standby once, shared control/signaling
once, both store nodes, and the applicable synchronous durability premium.
Account for retained/surge resources, ALB/WAF, NAT/endpoints and other billed
items through the corresponding categories. Explicit inapplicable categories
have zero quantity/rate and a reason. No AWS rates are embedded or fetched.

`public_egress.quantity` is GiB from measured forwarded UDP payload plus 28
IPv4/UDP bytes per public packet. The judge cross-checks it and the steady
GiB/hour quantity against measured bytes divided by telemetry call-seconds,
scaled to T=1 and one hour. A steady zero requires zero measured egress;
`zero_reason` cannot override positive observations. The report includes
`measured_steady_T1_public_gib_per_hour`. Do not infer
managed-store billable writes from Redis request/reply traffic. The ledger
keeps other transfer categories separate to avoid duplicate charges.

```text
call_hours = sum(caller HungUpAt - caller ConnectedAt) / 3600
allocated cost/call-hour = sum(quantity × rate) / call_hours
steady T=1 cost/call-hour = sum(steady_quantity_per_hour × rate)
```

Call durations stay on each caller's clock and include connected setup waits
before a common media start. The report's `call_hours_window` explicitly names
ConnectedAt through HungUpAt, including time outside telemetry;
`egress_window` names each phase's service `WindowSeconds`. Egress remains
measured only over those telemetry windows, so these are distinct allocation
and transfer observation windows. Required worker/relay/shared-control/store
quantities must cover observed node-hours, and steady quantities must include
the full two-worker, one-relay, one-control, two-store-node floor.

The report separates actual test allocation (including the four-call boundary)
from the steady survivor-safe T=1 estimate and lists every unknown input.
A service pass can coexist with `cost.status=NOT-RUN`; the private paid stage
must require validated costs separately. No total is emitted for an incomplete
ledger or unvalidated service run. AI fees, taxes and discounts are explicitly
excluded. Idle allocation belongs in supplied resource quantities; this test
is not a measured monthly idle bill or a cheaper-than-hosted comparison.

Synthetic tests prove the policy and artifact protocol only. Real AWS
provisioning/IAM, managed-store durability/failover, ENA behavior, WAN timing,
consent survival, sustained capacity, bills and teardown remain **NOT-RUN**
until the separate approved private cloud stage supplies actual evidence.
