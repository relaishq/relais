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
Logs survive for diagnosis. The existing `crash-run` modes are unchanged.

## Generator validity

Every child writes periodic stats and one existing caller report per call as
JSON lines to its `.jsonl` file and stdout. The parent aggregates these into
`report.json` and prints a process table. `-output` selects another report path.
Durations in JSON use nanoseconds; timestamps use RFC 3339.

The default limits are documented inputs, stored in each report:

| Measurement | Default saturation limit | Definition |
| --- | --- | --- |
| Send lateness | p99 > 20 ms in any resource interval | Actual successful UDP write time minus that media sample's original pacer deadline, once per RTP packet |
| CPU | > 85% per GOMAXPROCS in any interval | Delta of child `getrusage` user + system CPU, divided by wall time and GOMAXPROCS |

The limits are inclusive: exactly 20 ms or 85% does not trigger saturation.
Override with `-lateness-limit` and `-cpu-limit`; reports record those choices.
Default sampling is one second (`-sample-every`, minimum 100 ms). The final
sample includes the media tail and hangup. CPU includes all child work during
that interval, including online decode coordination and report construction.
Decoder subprocess CPU is excluded from child `RUSAGE_SELF`; its scheduling
impact remains visible in packet-send lateness. This is a conservative pacing
validity check, not a measurement of the topology's total CPU cost.

The pacer deadline advances by the encoded frame interval, independently of
actual send time. Timer ticks dropped by an overloaded scheduler therefore do
not erase missed workload: the remaining packets accumulate schedule debt.
All packets of a fragmented video frame share that frame's scheduled time.
STUN, DTLS and RTCP are excluded. Failed UDP writes are not successful sends;
caller errors still invalidate the measurement. The optional timing callback
runs at the caller socket after each successful write and must be fast and
safe for concurrent audio and video sends.

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
Rows are grouped separately by planned move and takeover. Capacity and soak
runners must require valid generator evidence before making service claims.

## Events

Repeat `-event=after,kind,count,worker` with increasing offsets from media start:

- `10s,move,2,1`: move the first two eligible callers to worker 1. Calls already
  on worker 1 are excluded. Count zero means all eligible calls. Selection is
  deterministic in process/caller order, not a random statistical sample.
- `30s,kill,0,0`: SIGKILL owned media worker 0, affecting **all** of its calls.
  Count must be zero. Each generator records the driver's kill timestamp so
  the harness's automatic takeover yardstick uses the real fault action.

`-workers` defaults to 3. IDs are 0 through workers minus one. A schedule must
leave one worker alive, have at least 3 seconds before its first event, and
more than 7 seconds after its last event. Moves to a worker already killed
are rejected. Events with no affected calls are measurement errors. A move
uses the control plane's operator HTTP API through a small harness hook and
records the existing per-call move report. A kill still relies entirely on
missed heartbeats for recovery. No recovery shortcut is added.

The owned command topology is loopback. `internal/loadgen.ChildOptions`
accepts an external harness topology, including its `CallerSocket` factory,
with signaling separate from media. The socket injection matches #36's
factory on main. Full multi-process namespace placement and network profile
control are future integration work; this command does not claim netns proof.
Fixed service/operator addresses use `clusterprocess.FreeTCP`, so main's #60
allocator will put them outside the OS ephemeral range after integration.
The base tested here still uses its earlier port allocation. Caller media
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
and uploads reports, runner information and all process logs even on failure.
The orchestrator must dispatch the workflow after integration. Hosted Linux
calibration is **NOT-RUN** in this worker checkout.

Local run results are recorded in [load-generator-results.md](load-generator-results.md).
