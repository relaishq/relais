# Continuous calls with seeded chaos

`relais-load -soak` extends the real-process driver. It uses the existing
harness caller, bounded recording, content/freshness yardstick, service metrics,
owned process groups, disposable non-6379 Redis and optional Linux LAN namespaces.
It changes neither capacity nor calibration acceptance.

```sh
make build
./bin/relais-load -soak -callers=4 -duration=3m -chaos-interval=30s \
  -seed=41 -workers=3 -worker-floor=8 -relay-floor=8 \
  -callers-per-process=2 -output=bin/soak-mac.json
```

Only loopback runs on macOS. On Linux install Redis, ffmpeg, iproute2, iptables
and procps, then use `sudo -E` and `-topology=netns` for the emulated LAN.
The workload is audio plus the harness VP8 fixture, not real cameras.

The full ticket target is at least 50 calls for at least one hour, with an event
every 30 seconds. `full_scale` labels whether those call/duration targets were
met; a passing short run cannot establish the full result. No full run or
GitHub result publication is performed by building the mode.

## Scale and N+1

Calls, duration, chaos interval, seed, worker count, generator processes and
callers per process are parameters. Generator processes default to
`ceil(callers / callers-per-process)`; `-processes` can explicitly override
that distribution within the per-process ceiling. The default is two callers
per process, at most four, with GOMAXPROCS=1 and unchanged generator limits.
Soak requires one-second sampling. Events need enough recovery time, and the
last seven seconds are reserved for recovery rather than another fault.

`-worker-floor` and `-relay-floor` are explicit measured capacity inputs, not
measurements made by soak. Both default to 8 from the latest #40 hosted evidence.
The driver rejects loads above `min((workers-1)*worker-floor - 1, relay-floor)`.
Each move needs a free target slot. All sampled worker session peaks must stay
at or below the supplied worker floor. The eight-call nightly has three
workers: survivors carry about four calls each, below the eight-call floor.
Larger hardware needs capacity evidence for its workload before selecting
floors that authorize a 50-call run. The defaults deliberately reject 50 calls.

## Seed and actions

An isolated Go `math/rand.NewSource(seed)` chooses a permutation of move,
drain and kill. The kinds repeat in that order, so every three events contain
one of each. It also chooses cyclic worker-selection offsets. The complete
plan is printed before media and saved in `schedule`; replaying the same seed,
workers, duration and interval reproduces that plan exactly.

At execution, an offset selects from the sorted occupied pool for drain/kill.
Moves use the least-loaded live destination, with the worker name breaking
ties. This avoids no-op kills and prevents moves over the worker floor.
Resolved workers, affected session IDs, action timestamps and per-call rows
are saved in `run.events`. Resolution also depends on the observed ownership
state: a seed reproduces the planned choices, not network timing or placement.

Kills use SIGKILL. The driver waits for real detection and ownership recovery,
then starts an empty incarnation on the original private endpoints. After a
drain it stops the empty worker and performs the same recycle. The private
`POST /private/workers/{name}/replace` endpoint exposes existing `Plane.Replace`
for the original registered address/interface only. It keeps the dead,
fully-recovered, zero-lease, zero-reservation and race guards. It accepts no
new address or arbitrary URL. This clears the drain flag through replacement,
without changing heartbeat or drain policy. Every new incarnation has its own
log directory and PID. The pool must be fully ready before the next event.

## Verdict and generator exclusions

Soak keeps the #38 limits: interval send-lateness p99 >20 ms, skipped frames
>1%, or CPU per GOMAXPROCS >85% means saturation. Exactly a limit is permitted.
At most 1% of the run's one-second wall-clock intervals may be saturated. A
second counts once if any generator is saturated; every excluded process
interval is named with its start, end and causes. A delayed saturated sample
excludes all wall-clock seconds it overlaps. Hangup-only intervals are outside
the media budget. All raw generator samples are retained in soak reports;
ordinary load/calibration reports keep their existing 256-sample cap.

Each event includes its preceding one-second baseline and the caller's saved
loss-observation, settling and freshness horizons. A conservative seven-second
recovery tail also covers incomplete event evidence. Any overlapping excluded interval makes that event and
its rows inconclusive, with raw trusted/failure counts retained. Such an event
also prevents a conclusive whole-run pass. Over-budget runs are inconclusive.
A separately confirmed service crash still fails, using #40's
`confirmedServiceFailure` rule. Direct caller/worker decryption failures,
control-plane lost calls, excessive worker peaks, memory growth and observed
leaks also remain failures. Other caller continuity, move/takeover gap,
threshold coverage and telemetry checks reuse capacity judgment.

## Flat memory

Services sample current RSS (Linux `/proc/PID/statm`, macOS `proc_pid_rusage`, with cgo enabled) and Go heap
spans in use through private metrics. Callers sample their current RSS and
`runtime.MemStats.HeapInuse`. Redis current RSS is sampled too; it has no Go
heap. Lifetime RSS peaks remain separate capacity telemetry.

Ignore the first `min(duration/10, 5m)`, then fit an ordinary least-squares
line. Fail if projected growth over one hour exceeds
`max(10% of the post-warm-up median, 16 MiB)`. Each metric reports median,
slope, hour projection, threshold and sample count. Every process needs at
least five minutes of post-warm-up observations; shorter windows are
`insufficient-data`. The raw samples remain in
the report. Workers and callers need at least ten minutes of observations;
shorter incarnations are `insufficient-data`, never called flat. Replacements
use separate PID windows and their own warm-up. A short smoke can test the
regression and sampling, but cannot prove flat worker/caller memory.

## Cleanup evidence

After every caller reports hangup, the driver checks control-plane calls and
worker sessions. It keeps the relay running for `FlowTimeout + 5s` (35 seconds
with this topology), then requires zero buffers, holds, routes and pending
routes. The wait and observation time are recorded. It finishes telemetry before shutdown, stops
all services while keeping its Redis alive, and SCANs exactly the run prefix.
It does not delete keys or wait for TTL expiry to manufacture clean evidence.
It then stops Redis and checks owned PIDs and process groups. Loopback TCP/UDP
addresses must bind again. For netns, successful owned namespace deletion
removes its private/media listeners; host caller listener bind checks remain.
Missing observations are inconclusive; observed leftover resources fail.

Redis cleanup accepts bounded retention, and reports every key's class, physical
TTL in milliseconds (`-1` means no key expiry), and applicable limit:

- Per-session lease/state/record/epoch/routes keys need TTL >0 and <=308 s:
  control lease 3 s + store retention 5 min + margin 5 s.
- The global `session_epoch` counter may persist without TTL.
- Worker index metadata has a separate <=605 s bound, from default index grace
  5 min + retention 5 min + margin 5 s. It still needs a positive TTL.
- The relay fencing hash persists to preserve its monotone epoch. Its lease
  lifetime is stored in an `expires` field rather than Redis PTTL. The driver
  checks that field against Redis `TIME` atomically: remaining lifetime must
  be <=600 ms, or the hash must be absent. Clean shutdown releases the holder,
  forwarder and previous-holder identities and sets expiry to zero. The report
  includes physical TTL, logical remaining lifetime and release status.
- Unknown keys, missing expiry evidence, immortal session/index keys and
  overlong lifetimes fail. Missing observations make the result inconclusive.

These limits match the owned topology's defaults. No fencing metadata is
removed or product retention shortened. Old reports without key TTLs or a
post-timeout relay observation cannot prove this contract.

A possible product improvement is for `Plane.End` to call `ForgetSession` so
relay state is freed at once. That change is outside #41.

## CI dispatch

The registered `load-calibration.yml` workflow has a separate `soak` job,
90-minute job budget and 80-minute measurement budget. It does not invoke
`crash-run`. Nightly uses eight calls for ten minutes, 30-second events, seed
41, three workers, two callers per generator, ubuntu-latest, namespace LAN and
measured worker/relay floors of eight. Runner metadata, report and every child
log are uploaded always, including failure. Namespace cleanup is checked.

After the workflow change is published, short dispatch:

```sh
gh workflow run load-calibration.yml --repo relaishq/relais --ref feat/41-soak \
  -f mode=soak -f soak_callers=8 -f soak_duration=10m -f chaos_interval=30s \
  -f seed=41 -f runner_label=ubuntu-latest -f callers_per_process=2 \
  -f soak_workers=3 -f worker_floor=8 -f relay_floor=8
```

Full dispatch, after the project lead chooses the hardware and matching
measured floors (for three workers, worker floor >=26 and relay floor >=50):

```sh
gh workflow run load-calibration.yml --repo relaishq/relais --ref feat/41-soak \
  -f mode=soak -f soak_callers=50 -f soak_duration=1h -f chaos_interval=30s \
  -f seed=41 -f runner_label="$SOAK_RUNNER" -f callers_per_process=2 \
  -f generator_processes=25 -f soak_workers=3 \
  -f worker_floor="$MEASURED_WORKER_FLOOR" -f relay_floor="$MEASURED_RELAY_FLOOR"
```
