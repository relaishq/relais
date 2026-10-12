# Distributed caller evidence

Use this opt-in mode when the external caller, signaling frontend, control
plane and workers use different clocks. It starts no services and adds no
provider-specific commands. Local and netns runs retain their existing event
matching and output.

## Caller configuration

Add these options to the existing `relais-load -external-caller` invocation:

```sh
-distributed -run-id=trial-001 \
-frontend-control-bound=2ms -frontend-worker-bound=2ms \
-clock-drift-ppm=100 -event-retention=180s
```

The two host bounds are **measured input assumptions**: the absolute difference
between each host's clock and the frontend clock throughout this run. The
worker bound must cover every worker generation used by the run. Zero means
measured agreement, not an omitted check. Both flags are required in distributed
mode. Supply the bounds from independent clock-error measurements. A configured
time source alone does not establish them. The drift rate is the worst relative
clock rate error between caller and any event host, in parts per million.

Retention defaults to 15 seconds outside distributed mode, and 180 seconds
inside it. Distributed mode rejects a shorter value. History still has packet
and content count caps; exhausting a cap advances the retained-history boundary
and makes affected event evidence inconclusive. A retention duration is not a
promise of unlimited memory. Default online video decoding remains unchanged.

The caller samples the authenticated frontend `/clock` before setup and every
10 seconds. It uses the same trusted HTTP client as signaling and status.
`GET /clock-probes` on the caller's loopback operator endpoint exposes the
samples. Each final call report also retains the samples and host bounds.

## Arm, act, then deliver the durable receipt

Before an action, send `POST /arm-event` to each affected caller's loopback
operator endpoint. The body is one identity:

```json
{
  "run_id": "trial-001",
  "operation_id": "kill-07",
  "session_id": "session-id",
  "worker": "17",
  "epoch": 4
}
```

`worker` is the unique registration name of the affected worker generation.
Never reuse it for a replacement. `epoch` is the affected ownership tenure,
not the recovery target's new epoch. Each affected session has its own identity,
with the same operation ID for a shared kill. Wait for all arm acknowledgments
before acting. The arm timestamp is captured locally by the caller.

After the operation, send `POST /record-events` with an array of receipts:

```json
[
  {
    "identity": {
      "run_id": "trial-001",
      "operation_id": "kill-07",
      "session_id": "session-id",
      "worker": "17",
      "epoch": 4
    },
    "kind": "takeover",
    "start": "2026-10-12T18:00:00.000Z",
    "end": "2026-10-12T18:00:00.450Z",
    "result": {
      "id": "session-id",
      "kind": "takeover",
      "from": "17",
      "to": "18",
      "lost": false,
      "error": ""
    },
    "uncertain": false
  }
]
```

For `kind=move`, `start` and `end` are actual control-host event timestamps.
For `kind=takeover`, `start` is the worker-host timestamp of the actual SIGKILL,
and `end` is the control-host takeover completion timestamp. Keep the original
control-plane `MoveResult` in `result`, including its timestamps, numeric phase
measurements, error, `lost` and `loss_cause`. Never use command submission time,
last heartbeat or estimated detection time as the kill origin. A missing actual
origin remains unmeasurable. An uncertain operation may omit timestamps, but
must still have the armed identity and preserve its result/error fields.

The operator is responsible for attaching the identity to the durable server
receipt. This is a test evidence protocol, not a server authentication or
ownership mechanism. Identity attachment must use the selected session, worker
generation and epoch; timestamps cannot identify the action. Failed and
uncertain results use the same feed. The handler attempts every item in a batch
and returns an error for unknown sessions or invalid identities. Identical
pending receipt retries are idempotent; conflicting receipts are rejected.
The arm/receipt inventory is bounded at 4096 identities per caller.

The legacy caller `/kill`, `/move` and `/record-moves` routes are refused in
this mode. Automatic sanitized status polling cannot supply an operation ID or
actual kill timestamp; it does not create duplicate distributed takeover
records. Status still supports caller setup and ordinary local mode.

For load-report joining, populate `EventResult.identities` with the affected
identities. `MatchesEvent` compares all five fields and the scenario. It never
uses timestamp equality for distributed reports. Missing, duplicate and failed
receipts remain inspectable evidence.

## Clock intervals and judgment

A probe retains caller send/receive and frontend receive/send timestamps. For
one server timestamp, its server-minus-caller offset is bounded by
`[server - caller_receive, server - caller_send]`. The two frontend timestamps
tighten this to `[server_send - caller_receive,
server_receive - caller_send]`. The frontend-to-event-host input interval is
added to that interval. The result widens by elapsed time times the drift bound;
the anchor distance also includes uncertainty in the event mapping.

Caller media gaps, content ages and connection observations stay on the caller
clock. Controller phase durations remain on the control clock. Timestamp probe
ordering errors, disjoint intervals beyond the drift allowance and detected
caller wall/monotonic disagreement invalidate clock trust for the run. Failed
periodic probes also invalidate trust. Later samples cannot erase a detected
jump. Saved event summaries are downgraded if later clock evidence becomes
invalid. Interval overflow and missing samples are inconclusive.

Mapped events must fit the caller-local arm/receipt bracket and the retained
observation history. An inconsistent host bound can violate that bracket and
produce inconclusive. **Frontend probes cannot verify the supplied inter-host
bounds.** A false bound that still fits the causal bracket may be undetectable;
independent host clock measurements remain required.

Each `MoveReport` includes its identity and `distributed` evidence. `start` and
`end` in that evidence are intervals. Raw gap bounds use the union and
intersection of every possible event window, not just endpoint samples. A
pass uses the worst possible event gap and a conservative baseline bound. A
failure needs a violation in every possible window. A mixed result is
inconclusive. Content loss uses the same union/intersection rule. Known server
loss and conflicting content identities remain failures even with uncertain
timing; there is no assumed WAN packet-loss allowance.

For takeover, `kill_to_live` bounds the actual kill origin to identified live
content received by the caller. Video requires decoded, non-repeated content;
audio uses identified returned sends. It is not command-submission-to-RTP time.

The final numeric trial policy uses pooled move-gap p95: at most pooled
baseline p95 +50 ms and at most 150 ms. Every pause must remain below 2 seconds.
At least 20 move trials and 10 takeover trials are required. A single move-gap
outlier does not fail pooled p95 unless it changes that percentile. Per-event
`distributed.outcome` is a window diagnostic; the final call's
`distributed.outcome` applies the pooled trial counts/policy. One move and one
kill cannot establish acceptance.

Freshness, lateness, resume and decoded-content verdicts are evaluated at both
ends of the event interval and at every observation or policy boundary where a
classification can change. Strict comparisons include the adjacent nanoseconds.
This detects non-monotonic cases such as a baseline that is valid at both ends
but has too few samples inside the interval. Dense RTP fragments are grouped
only when they occupy the same classification cell; this is not time-grid
sampling. A verdict is trusted only when all evaluations agree. A changing
content label is inconclusive even when both labels have observed content.

`distributed.durations` retains event-to-observation duration intervals for
resume, first decoded content, first new content, first live content, and audio
and video freshness recovery. For a fixed observed instant the bounds are
`[observed - latest_event, observed - earliest_event]`. A recovery interval
entirely within the existing 2-second limit passes; one entirely beyond it
fails. A threshold straddle is inconclusive and includes its duration bounds
and event-interval width in the reason. Resume and first-content rows retain
the existing availability/provenance policy, which has no separate elapsed-time
ceiling; the elapsed intervals are reported without inventing a new threshold.
Caller-local packet gaps, content ages and the selected raw observations remain
unchanged. A zero interval gives the same measurement verdicts as local mode.

A move's start and completion share a control clock, so they move together
within the offset interval. Its measured span widens for relative rate error
and possible variation within the supplied control-host error bound. Takeover
start and completion use their respective worker/control bounds. Possible
first-event baselines and recovery floors remain possible references for later
events; one selected baseline cannot stand in for an uncertain earlier window.
Evaluation is bounded at 4096 classification cells/references per event. An
exhausted budget is explicitly inconclusive, rather than a partial-sampling
pass. Known server loss and independent content corruption still fail.

For a WAN probe, the full offset interval has width RTT minus frontend handler
processing time. RTT/2 is its **half-width**, not its full width. A 20–70 ms RTT
therefore gives approximately 20–70 ms full width (roughly +/-10–35 ms) before
host bounds and drift. A measured absolute frontend-to-control error below
1 ms adds less than 2 ms of full width. At 100 ppm, a probe 10 seconds away adds
about 2 ms of full width. All probes are widened to the event and intersected,
so a low-RTT older sample can beat a high-RTT nearer one; asymmetric one-way
delays are never assumed to be equal. These are estimates, not a measured WAN
qualification result.

These widths leave substantial room below the 2-second recovery/pause limit
when recovery is comfortably inside it. The 150 ms move-gap ceiling and the
baseline +50 ms margin compare raw caller-local gaps: clock width changes which
gaps belong to the event window, without adding tens of milliseconds to a gap.
Steady windows can therefore be conclusive; a window containing an ambiguous
outlier or a recovery near its limit must remain inconclusive.

## Verification

Unit tests cover interval composition/drift/overflow, jump rejection, all
identity fields, failed/uncertain ingestion, retention, pass/fail/inconclusive
windows, pooled p95, zero-interval equivalence, threshold straddles and interior
non-monotonic classifications. The frontend integration test
uses a real in-process relay/control/worker cluster and an external caller over
trusted authenticated HTTPS. A three-second server clock skew is recovered by
`/clock`. It moves the call and simulates worker death, obtains conclusive content
verdicts with nonzero timing uncertainty, preserves actual takeover audio loss
as failure, and judges
an inconsistent host bound inconclusive. This fixture simulates process death;
it does not claim a native OS SIGKILL timestamp or a real distributed run.

```sh
go vet ./...
go test -race -count=1 ./internal/loadgen ./cmd/relais-load ./cmd/relais-signal ./pkg/callharness
make test-harness-short
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run \
  ./internal/loadgen ./cmd/relais-load ./cmd/relais-signal ./pkg/callharness
```
