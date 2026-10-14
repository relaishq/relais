# Continuous calls with seeded chaos

`relais-load -soak` extends the real-process driver. It uses the existing
harness caller, bounded recording, content/freshness yardstick, service metrics,
owned process groups, disposable non-6379 Redis and optional Linux LAN namespaces.
It changes neither capacity nor calibration acceptance. Capacity shares only
the generator lateness rule below (#133).

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
at or below the supplied worker floor. The six-call nightly has three
workers: survivors carry about three calls each, below the eight-call floor.
Larger hardware needs capacity evidence for its workload before selecting
floors that authorize a 50-call run. The defaults deliberately reject 50 calls.

## Seed and actions

An isolated Go `math/rand.NewSource(seed)` chooses a permutation of move,
drain and kill. The kinds repeat in that order, so every three events contain
one of each. It also chooses cyclic worker-selection offsets. The complete
plan is printed before media and saved in `schedule`; replaying the same seed,
workers, duration and interval reproduces that plan exactly.

The same seed also sets each caller's media start offset in [0, 1 s) and the
phases of its one-second periodic work, so callers are not synchronised
([load generator](load-generator.md#callers-do-not-start-together), #85).
Every call is sending one second after the start, well before the first event.

Worker 0 is the recorded memory sentinel. Chaos never drains or kills it.
It remains eligible for moves and takeover adoptions for the whole run.
At execution, an offset selects from the sorted occupied non-sentinel pool
for drain/kill.
Moves use the least-loaded live destination, with the worker name breaking
ties. This avoids no-op kills and prevents moves over the worker floor.
Resolved workers, affected session IDs, action timestamps and per-call rows
are saved in `run.events`. Resolution also depends on the observed ownership
state: a seed reproduces the planned choices, not network timing or placement.

Kills use SIGKILL. The driver waits for real detection and ownership recovery,
then starts an empty incarnation on the original private endpoints. After a
drain it stops the empty worker and performs the same recycle. The private
`POST /private/workers/{name}/replace` endpoint only clears `draining` on the
existing dead, fully recovered registration. It checks zero leases, zero
reservations and registration races. It leaves the registration dead and
preserves the rejoin token fence. A resumed old process cannot return with a
bare heartbeat. The driver starts the new process, then waits for a fresh
heartbeat and live membership through the real rejoin handshake. It verifies
every recycle, including the final one, and records old/new PIDs, launch time,
heartbeat time and completion time in `run.worker_rejoins`. Every incarnation
has its own log directory and PID. The pool must be ready before the next event.

## Verdict and generator exclusions

Soak retains the #38 raw limits: interval send-lateness p99 >20 ms, skipped
frames >1%, and CPU per GOMAXPROCS >85%. A modest lateness or skipped-frame
excess (at most 2x its limit) must persist to exclude intervals: the next or
previous sample of the same generator must also exceed a limit. Both samples
are excluded, including the first second of overload. An isolated one-tick
slip remains visible in the raw samples but does not establish overload
(#128, #133). These exclude immediately:

- lateness p99 above 2x `-lateness-limit` (40 ms by default);
- skipped frames above 2x `-skipped-frame-limit` (2% by default);
- CPU per GOMAXPROCS above 85%;
- a sample above a limit whose interval maximum lateness exceeds 40 ms. A
  longer stall could shorten a gap below the recorder's 90 ms retention
  threshold (`callharness.MaxToleratedStall`, 60 ms with one audio tick).

Exactly a limit is permitted. An excluded interval names lateness or skipped
frames only when that excess alone excludes it. Missing intervals do not establish
persistence. Capacity uses the same rule for its steps (see
[capacity.md](capacity.md)). Calibration retains the strict single-sample gate,
and so does the raw `run.processes[].saturated` field. The soak
`saturation` object records its persistence, immediate-lateness,
immediate-skipped-frame and maximum-stall limits.
At most 1% of the run's one-second wall-clock intervals may be saturated. A
second counts once if any generator is saturated; every excluded process
interval is named with its start, end and causes. A delayed saturated sample
excludes all wall-clock seconds it overlaps. Hangup-only intervals are outside
the media budget. So is up to the last second of media from calls with late
start offsets, which end up to one second after `start + duration`. All raw
generator samples are retained in soak reports; ordinary load/calibration
reports keep their existing 256-sample cap.

Each event includes its preceding one-second baseline and the caller's saved
loss-observation, settling and freshness horizons. A conservative seven-second
recovery tail also covers incomplete event evidence. Any overlapping excluded interval makes that event and
its rows inconclusive, with raw trusted/failure counts retained. An unexcluded
caller stall can still shorten a measured gap. Its bound is the largest interval
maximum lateness above the limit, plus one 20 ms audio tick, among the affected
call's generator samples overlapping [move start − 1 s, media resumption]. A
takeover track that passes 2 s by less than its bound is untrusted. Move tracks
are all kept; the p95 gates are also judged with each bound added, and a
measured pass that fails there is inconclusive. A measured failure still fails.
Stalls whose sends stay within the limit are the existing measurement envelope
and are judged as before. A confirmed
event failure still fails even when its window is saturated. Such an event
is listed with its cause in the JSON and log. Inconclusive events do not each
invalidate the entire run. The run needs at least 95% conclusive events, using
an exact fraction, plus at least one conclusive pass of each kind: move, drain
and kill. Exactly 95% is permitted. For example, 19/20 qualifies; 18/19 is
94.737% and does not. Runs above the saturation budget are inconclusive.
Missing caller, cleanup or independent resource evidence remains inconclusive.
A separately confirmed service crash still fails, using #40's
`confirmedServiceFailure` rule. Direct caller/worker decryption failures,
control-plane lost calls, excessive worker peaks, memory growth and observed
leaks also remain failures. Other caller continuity, move/takeover gap,
threshold coverage and telemetry checks reuse capacity judgment. The recorder
retains up to 4096 timestamped gaps over 90 ms per track for the whole call,
separate from its 15 s rolling arrival history. An unscheduled gap above
150 ms fails unless it overlaps a saturated interval of that caller's
generator; that gap is then inconclusive. Saturation in another generator
cannot excuse it. A tolerated stall can stamp the packet before an outage late
and shorten a real interruption below 150 ms (#133). An unscheduled gap at or
below 150 ms is therefore inconclusive when the gap plus its stall bound
exceeds 150 ms. The bound is the largest interval maximum lateness above 20 ms,
plus one 20 ms audio tick, among this caller's generator samples that overlap
[gap start - 1 s, gap end]. Gap inventory overflow counts dropped gaps above
150 ms and fails safe. A full inventory may also have dropped shorter gaps, so
it is inconclusive. Old reports without the inventory are inconclusive.

## Whole-call delivery

Event rows judge content only inside each affected call's event windows. A call
that never moves can still lose echoed content, for example when the kernel
drops datagrams on a full UDP receive buffer (#127). Whole-call delivery covers
the rest of every call.

The caller counts each successfully written audio unit and video frame once,
by content identity, not by RTP sequence number. A unit settles when it leaves
the rolling history, or at hangup when it was sent at least 500 ms (the
yardstick's settle window) before hangup. Later units are `unsettled` and are
not judged. Lost uses the event yardstick's rule: no valid complete return by
settlement. A late return is not lost. The counts survive history expiry, and
each kind keeps the send time of up to 4096 lost units (`Recording.Delivery` in
each call report).

Soak media paths are unshaped, so the steady state has the same expectation as
the event windows: zero lost units. The rule is:

- A lost unit sent inside `[WindowStart, WindowEnd)` of one of its call's
  moves that matches an event affecting that call is judged only by that
  event's `audio_loss` or `video_loss` row. It is never counted twice.
- Any other lost unit fails the soak.
- A lost unit whose send time plus 500 ms overlaps a saturated interval of its
  own generator is inconclusive instead, as for unscheduled gaps. Saturation
  in another generator cannot excuse it.
- More than 4096 lost units of one kind in one call cannot be placed, and fail.
  A missing `Delivery` (old reports), or counts that do not cover every written
  unit, are inconclusive.

The `delivery` object holds per-call and run-total `sent`, `returned`, `lost`,
`unsettled`, `lost_in_event_windows`, `lost_outside_event_windows`,
`lost_outside_saturated`, `lost_inventory_overflow` and
`sequence_discontinuities`, and the first outside loss time per call. RTP
sequence discontinuities are diagnostic only: takeover replays and planned
resumes also break the echoed sequence.

On Linux, the driver reads `/proc/net/snmp` `Udp:` `RcvbufErrors` and, when
present, `/proc/net/snmp6` `Udp6RcvbufErrors` before and after the run.
`kernel_udp` and the log report the change. These counters belong to the
driver's network namespace: they cover every role in a loopback run, but not
the roles' own namespaces in a netns run. Other systems report "not
available". The counters are diagnostic and never change the outcome.

## Flat memory

Linux services sample current RSS from `/proc/PID/statm`. macOS uses
`proc_pid_rusage` with cgo: `ri_phys_footprint` is the memory trend metric;
`ri_resident_size` remains informational. Both fields are retained separately
([Apple structure](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/resource.h)).
Services also sample Go heap spans in use through private metrics. Callers
sample the same process metrics and `runtime.MemStats.HeapInuse`, plus
`/gc/heap/live:bytes` from Go 1.26's `runtime/metrics`: the live objects marked
by the previous GC. Sampling reads the latest natural GC result; it does not
force collection. Redis has no Go heap. Lifetime RSS peaks remain separate
capacity telemetry. Soak fails
before starting the topology when current process sampling is unavailable;
a Darwin build without cgo can build, and sampling tests skip with a reason.

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

Caller processes are test tooling. Their leak signal is retained live memory,
so only their `heap-live` trend gates the run and acceptance. Caller `rss`,
`physical-footprint` and `heap-in-use` trends retain their slopes, projections,
thresholds and outcomes with `diagnostic=true`; even a diagnostic `fail` does
not fail the run or prevent acceptance. A metric unavailable on the host stays
explicitly `insufficient-data`. Caller live heap uses the same warm-up,
five-minute post-warm-up minimum, ten-minute per-PID minimum and hourly slope
threshold above. Missing live-heap samples cannot prove caller memory.

Product processes retain both process-memory and heap-in-use gates: relay,
control plane and every sufficiently long-lived worker, including the
sentinel. Redis retains its process-memory gate. Process memory means RSS on
Linux and physical footprint on macOS, as above. This rule is unchanged:
product RSS limits capacity. The caller decision follows the observed harness
allocation churn and page retention; see [caller memory findings](caller-memory.md).

`outcome` judges execution and event coverage. Insufficient memory does not
prevent a short nightly from passing; observed growth in a gating memory metric
still fails.
`acceptance` is a separate boolean. It requires full scale (at least 50 calls
for at least one hour), `outcome=pass`, and conclusive memory for every
long-lived process. Relay, control, Redis and caller processes always require
memory proof for acceptance. Every caller PID needs a conclusive passing
`heap-live` verdict; a different caller's pass or old RSS-only samples cannot
supply that proof. A worker incarnation observed for at least ten
minutes also requires it; shorter incarnations remain explicitly unjudged.
The uninterrupted sentinel requires conclusive process-memory and heap
verdicts from one PID. Otherwise acceptance includes "worker memory unproven".
Recycled workers report informational RSS/footprint at matched age 30 s after
their first sample, plus the RSS change between available incarnations. Missing
matched-age observations are labeled unavailable. They do not gate acceptance
or substitute for the sentinel.
Missing role verdicts prevent acceptance. The report marks each metric's
`long_lived` status and lists `acceptance_reasons`; the log prints the boolean
and every reason. A short run always has `acceptance=false`.

Soak callers also report heap objects, unused spans, free and released pages,
Go stacks, reported OS stacks, other runtime memory, cumulative allocations
and goroutine count. These fixed-size metrics are sampled once per second only
in soak mode. The existing loopback operator endpoint offers `/debug/memory`,
`/debug/pprof/heap` and `/debug/pprof/allocs`. Profiles are generated only on
request; `heap?gc=1` explicitly forces GC and can affect the experiment. No
global HTTP profiling handlers or additional listeners are installed.

## Cleanup evidence

After every caller reports hangup, the driver checks control-plane calls and
worker sessions. It keeps the relay running for `FlowTimeout + 5s` (35 seconds
with this topology), then requires zero buffers, holds, routes and pending
routes. The wait and observation time are recorded. It finishes telemetry before shutdown, stops
all services while keeping its Redis alive, and SCANs exactly the run prefix.
The scan must find both positive controls: `session_epoch` and the default
relay fencing hash. A missing control makes cleanup inconclusive. It does not
delete keys or wait for TTL expiry to manufacture clean evidence.
It then stops Redis and checks owned PIDs and process groups. Loopback TCP/UDP
addresses must bind again. For netns, successful owned namespace deletion
removes its private/media listeners; host caller listener bind checks remain.
Missing observations are inconclusive; observed leftover resources fail.

Redis cleanup accepts bounded retention, and reports every key's class, physical
TTL in milliseconds (`-1` means no key expiry), and applicable limit:

- Per-session lease/state/record/epoch/routes keys need TTL >0 and <=308 s:
  control lease 3 s + store retention 5 min + margin 5 s.
- `ended:{sess:…}` tombstones need TTL >0 and <=300 s. Redis release sets
  their expiry to server time plus `sessionstore.EndedRetention` (5 min),
  independently of the lease and metadata retention.
- `frames:{sess:…}` frame-cache keys need TTL >0 and <=30 s, matching the
  frame cache's default idle TTL.
- The global `session_epoch` counter may persist without TTL.
- Worker index metadata has a separate <=605 s bound, from default index grace
  5 min + retention 5 min + margin 5 s. It still needs a positive TTL.
- The relay fencing hash persists to preserve its monotone epoch. Its lease
  lifetime is stored in an `expires` field rather than Redis PTTL. The driver
  checks that field against Redis `TIME` atomically: remaining lifetime must
  be <=600 ms. A missing default hash makes cleanup inconclusive. Clean
  shutdown releases the holder,
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
`crash-run`. Nightly uses six calls for 10m10s, giving 20 events. The exact
95% rule tolerates one inconclusive event. It uses 30-second events, seed
41, three workers, two callers per generator, ubuntu-latest, namespace LAN and
measured worker/relay floors of eight. Runner metadata, report and every child
log are uploaded always, including failure. Namespace cleanup is checked.

After the workflow change is published, short dispatch:

```sh
gh workflow run load-calibration.yml --repo relaishq/relais \
  -f mode=soak -f soak_callers=6 -f soak_duration=10m10s -f chaos_interval=30s \
  -f seed=41 -f runner_label=ubuntu-latest -f callers_per_process=2 \
  -f soak_workers=3 -f worker_floor=8 -f relay_floor=8
```

Full dispatch, after the project lead chooses the hardware and matching
measured floors (for three workers, worker floor >=26 and relay floor >=50):

```sh
gh workflow run load-calibration.yml --repo relaishq/relais \
  -f mode=soak -f soak_callers=50 -f soak_duration=1h -f chaos_interval=30s \
  -f seed=41 -f runner_label="$SOAK_RUNNER" -f callers_per_process=2 \
  -f generator_processes=25 -f soak_workers=3 \
  -f worker_floor="$MEASURED_WORKER_FLOOR" -f relay_floor="$MEASURED_RELAY_FLOOR"
```

Stable log verdict lines start with `soak outcome=`, `soak cause:`,
`soak acceptance=`, or `soak acceptance reason:`. Run totals for whole-call
delivery start with `soak delivery outcome=`; each non-passing call adds a
`soak delivery pid=` line. The kernel diagnostic starts with
`soak kernel-udp`. For example:

```sh
grep -E '^soak (outcome|cause|acceptance|delivery|kernel-udp)' bin/soak-linux.log
```
