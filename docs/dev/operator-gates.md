# Operator session core

`relais-gates session -config FILE [-spool DIR] [-timing-evidence FILE]` accepts one init and runs the
public `RunSequence` over JSON Lines on stdin/stdout. Stdout contains protocol
frames only. The private JSON configuration supplies `parameters` (the existing
process endpoints), two release `pins`, clock `bounds`, `architecture`,
caller `endpoints` and `token`, and an absolute `judgeBinary` path.
Optional `callerIdBase` allocates caller identities after setup callers.
The binary is checked against Init's judge SHA-256 before resource requests;
release pins are bound to Init's scenario. No release build or download occurs
inside a gate. `-spool` binds an agreed local blob directory without putting its
path in artifacts. Configuration and the spool are local inputs, not public
reports. `relais-gates version` emits the unchanged protocol-v1 hello.

Numeric timing diagnostics are written as JSON Lines to stderr by default.
`-timing-evidence FILE` requires an absolute path and writes a new local file
with mode 0600; an existing file is never overwritten. Place this file outside
any temporary state directory that the launcher deletes. The operator owns
persistence of this output. Rehearsal uses the same recorder in its diagnostic
log. A write error prints `operator-timing-evidence-incomplete` to stderr after
the session. It does not change the session exit code or final result.

Each `numeric-timing` record is a **cumulative** pool snapshot with a sequence
number. Use the final snapshot (`final: true`) for totals. Phase, call and move
are zero-based indexes in that snapshot. Event is the scheduled event index;
-1 means that no unique join exists. These are never deployment identities. Every event
retains mapped start/end lower/upper/width in nanoseconds since caller start,
available metric duration bounds, and audio/video gap and baseline bounds.
The optional version-1 `timing_evidence` caller section records the original
mapping before planned-move correlation, host offset bounds, drift ppm, probe
residual intervals, four probe timestamps, signed event-age intervals and the
anchor distance and per-side drift allowance used for each mapping. The probes
that set the intersection endpoints are identified by their numeric indexes.
Both binding-probe indexes are -1 when mapping fails.
The planned-move section records raw span, host allowance per side, rate input
and the resulting span bounds, including the existing zero clamp.

Content candidate rows retain each distinct label/trust/failure state for
`first_content` and `first_new_content`. Each state also retains its minimum
and maximum evaluated start and end. These optional ranges are absent in earlier
version-1 records. Representative start/end, packet-resume,
source-send, prior-receipt, keyframe-request and decoded-frame times, plus branch
membership flags and explicit witness-change flags show which boundary changed
when labels disagree. Change flags compare with the first row for that metric.
They are representative cells, not a full replay history. `host-clocks` records retain
error/rate measurements and measurement times for each actual four-host refresh;
raw tracking text is removed. Clock evidence now writes `version: 2` (the
previous unversioned shape is version 1). Version 2 covers recorded diagnostics,
including optional frontend clock falsification records.
Protocol, result, numeric media timing and judge input formats are unchanged.
Only fixed host slots and content labels survive this projection. No deployment
identities or addresses are included.

The clock check takes the frontend host from the inventory component `signal`.
It takes the control host from the inventory component `control`.
A missing or duplicate component means missing independent clock evidence.
Inventory that has not been read also means missing clock evidence.
All four slots (`relay`, `w1`, `w2`, `shared`) must still have a clock row.
The control bound is the sum of the frontend and control host errors.
The worker bound is the frontend error plus the larger worker error.
The drift bound is the frontend rate error plus the largest rate error among
the control host and both workers. If frontend and control share a host,
the check still adds both errors.

Each host sample also retains `trackingLog`: newly read rows, a cursor, the
session's armed time (`runStartUnixMs`) and break records. A session keeps each
host's live cursor and sends it on the next read. The cursor binds device/inode,
byte position, a trailing-content hash, daemon boot/PID/start identity, the
previous row and wall/monotonic read brackets. The first read returns recent
complete rows from at most 64 KiB of log history. A read without a cursor records
an initial `cursor-gap` only with an armed run start (`runStartUnixMs > 0`).
The gap runs from the run start second to the first kept row if that row is
in-run. If it keeps no row and the read occurs after the run start, the gap ends
at the read time. A first kept row before the run creates no initial gap.
Later reads resume at the cursor and read through EOF up to 64 KiB. An additional
encoded-size budget keeps rows and breaks below the host unpack limit. These two
limits create no additional break; the next read resumes at the cursor. A batch exceeding
the existing compressed wire limit drops its rows and records a gap instead of
failing the clock read; the cursor still advances, so those rows are not read
again. The replacement gap preserves the earliest start of all in-run breaks.
Partial final lines wait for the next read. An oversized line
records one loss break and is discarded through its newline or the current EOF;
the cursor remembers whether later bytes still belong to that discarded line.

Continuity uses row timestamp gaps greater than 60 s, inode, anchor and size
checks. Time between host reads alone does not create a break. Rotation, loss,
restart, wall steps, source changes, non-Normal leap status and inconsistent
bounds also produce recorded breaks. Wall steps use the change in realtime
minus monotonic time, allowing both read widths plus 1 us for read resolution.
Public rows omit `reference` entirely; a source change is retained as a break.
Only byte position and a hashed anchor survive from the private cursor.

Chrony's `chrony.conf(5)`, `log tracking` format has 14 whitespace-separated
columns: UTC date, UTC time, reference (server IPv4/IPv6 address or refclock
name), stratum, frequency (ppm), skew (ppm), offset (s), leap letter, combined
source count, offset standard deviation (s), remaining correction (s), root
delay (s), root dispersion (s), and maximum error (s). Repeated banners and
column headers are skipped. There is no residual-frequency column. Column 14
is chrony's error bound since the preceding update, including remaining
correction and frequency uncertainty. Self-consistency records a break when
`abs(offset_k) > maxerror_k`; the reader does not age a previous row or parse
`maxclockerror` from configuration. The rehearsal config enables `log tracking`
in `/var/log/chrony`, sets `maxclockerror 1`, and retains `chronyd -x`.

Rows and breaks have `beforeRun`: true only when the row's entire timestamp
second, or a break's entire interval, precedes the session armed time. A second
that straddles that time counts as in-run. Breaks overlapping the run count as
in-run even if they start earlier. Undated loss records conservatively count
in-run. Historical rows and breaks remain recorded; rehearsal acceptance counts
only breaks with `beforeRun: false` and checks coverage of in-run timestamps.

The fixed pre-fault clock admission stays unchanged. Slice 4b uses retained
timelines to qualify event windows, as described below. Attempted refreshes
with a failing host also retain a
non-zero `At` and available host timelines, including in batch mode. Such
failed refreshes count toward the recorder's 256-refresh cap, but do not enter
the runtime's successful sample cache or make a failed gate succeed.
For rehearsal acceptance, inspect all four slots in `host-clocks` JSONL or
report `clocks`: require version 2, usable per-host cursors, a continuous row
timeline covering the run including its tail, no in-run breaks, and zero
`omitted_refreshes`. An empty batch or a non-zero `At` alone proves no coverage.
Coverage is enforced with the fixed bounds. Bound selection belongs to slice 5.

## Recorded clock selection (slice 5a)

`relais-gates clock-select -evidence admission.json -policy policy.json` writes
a version-1 receipt and its SHA-256 digest to stdout. Refusal exits 1 and writes
a fixed reason to stderr; it emits no receipt. The command performs no host,
caller or judge operations. Selection and lease evidence are recorded only:
existing gate bounds, caller flags, clock admission, coverage qualification and
judge outputs stay unchanged. A receipt is not authorization to use its bounds.

The launcher supplies `clocks.SelectionEvidence`: a run ID, inventory-resolved
`frontendSlot` and `controlSlot`, `callerLeapMode`, admission start/end in Unix
seconds, four `hosts` (`relay`, `shared`, `w1`, `w2`), and frontend
`falsification` evidence. Each host supplies its slot, leap mode, complete
tracking log and lease observation. All five leap modes must agree, either
`smeared` or `unsmeared`; every covering tracking row must be Normal (`N`).
There is no default launcher policy. For example, a rehearsal can use:

```json
{
  "windowSeconds": 120,
  "floorNs": 50000,
  "scaleNumerator": 2,
  "scaleDenominator": 1,
  "allowanceNs": 10000,
  "signalReserveNs": 50000,
  "controlCeilingNs": 250000,
  "workerCeilingNs": 350000,
  "maxDispersionGrowthPpm": "5"
}
```

These are rehearsal targets, not measured deployment guarantees. The declared
dispersion-growth maximum must cover the measured total rate, including clock
frequency uncertainty. It is not chrony's `maxclockerror` setting alone.

The admission window lasts at least 120 seconds. Its end must equal the oldest
latest retained server update minus one second; its start is that end minus
`windowSeconds`. This admits only the trailing common window. Each server
needs at least ten distinct updates within it, predecessor/closing coverage
with one-second timestamp margins, no interval longer than 60 seconds and no
overlapping tracking break (also allowing the break's one-second margin).
Identical repeated rows do not count again; distinct equal-second updates do.
Refuse any break from the start margin through the latest host state, even after
the common window. Leap/reference checks continue through every host's last row.
Expired leases refuse selection. Lease measurements must lie within the inclusive
interval from one second before to 60 seconds after that host's latest retained
row; otherwise selection refuses with `clock-select-stale`.
Warm-up rows remain in evidence outside the window. Startup, a scheduled leap,
loss or a source change cannot be hidden by counting later good rows.

Falsification evidence must be enabled, have no losses or sticky falsification,
and contain checked records for control and both workers throughout admission.
The selector checks the raw four timestamps against the padded tracking
envelopes again. At the one-Hz cadence, each target's endpoints must be within
two seconds of the window endpoints and successive records at most two seconds
apart. Missing, unchecked or contradicted observations refuse selection.

Column 14 is retained as `maxErrorDecimal` by the explicit diagnostic reader.
Chrony prints it with four significant digits (`%.3e`). Add half its last
printed unit before taking each host's maximum: `4.244e-02` represents at most
`0.042445` seconds. Old numeric-only rows cannot supply this receipt evidence.
The selector does not infer missing precision from a binary float.

For each host maximum M, compute `H(M) = ceil_ns(max(f, s*M) + A)` using exact
rational arithmetic, then `B_control = H(M_frontend) + H(M_control)` and
`B_worker = H(M_frontend) + H(max(M_w1, M_w2)) + K`. Each host rounds outward
before the sum; K is added once. Co-located frontend and control still count
twice. Above either ceiling, refuse rather than clip. Caller drift stays
100 ppm. No pass threshold or trial population changes.

The receipt records the formula, run, interval, roles, all five modes, policy,
per-server update counts/padded maxima, declared and measured growth rates,
recorded lease values, selected bounds and evidence digest. Digest the compact
UTF-8 JSON from the Go receipt schema, excluding the enclosing `sha256` member
and output newline. Host arrays are sorted by slot; rows must already be in
timestamp order, preserving equal-second input order. The evidence digest covers normalized falsification records from
`Falsification.MarshalJSON`, not original log or JSON bytes, and excludes opaque
cursors. Input whitespace and JSON member order are accepted; recompute receipt
bytes to compare digests.
The receipt exposes fixed slots and numeric values, not source addresses.
In slice 5b, binding will additionally compare evidence with the session's own
pre-run rows, so recomputation alone is not proof of observation.

`host.ClockWithLease` opts into `read_clock(..., lease=True)`. Existing
`Clock`/`ClockWithProbes` reads remain unchanged, including command count and
output. The diagnostic reader takes two bounded `chronyc -c tracking` reads,
separated by 0.2 seconds, requiring the same reference update and identity.
It reads the selected source's runtime Poll exponent from `chronyc -c sources`;
refclock `dpoll` and the configured maxpoll limit are not that field. It never
parses chrony configuration.

At report time, `E = abs(System time) + RootDelay/2 + RootDispersion`. Add 2 ns
for the printed nine-decimal values before rounding outward. Record
the two dispersion values and the minimum possible elapsed interval (second
request-send minus first completion). The Go recorder adds one nanosecond for
the two printed dispersion rounding errors and calculates an outward bound on
the total dispersion-growth rate. That measured total replaces the summed
residual/skew/maxclockerror slope; residual and skew remain recorded and are
not added again. Selection refuses a rate above the declared maximum.

In 5a, `hostElapsedNs` records only monotonic time inside the host reader.
`RecordLease` uses this host-only age and records an outward bound and remaining
lifetime until the last update plus two runtime polls. Repeated reads do not renew an update. Changed
updates, read failures, malformed optional lease data and wire pressure record
unavailable evidence; they do not alter existing scalar admission. In 5b the
gate must add its monotonic elapsed time from request-send, including transport;
the host-only age in 5a does not prove this full age. The new
lease fields are optional recorded data. No existing fault path checks them.

Slice 5b will bind Init/config/caller/coverage to the receipt, enforce fault-path
leases, preserve restoration, and drain coverage before the final judge.

The opt-in frontend clock prober writes an append-only run-local file, capped
at 4 MiB including one capacity marker. Each complete ASCII line is exactly
84 bytes: `T FFFFFFFFFFFFFFFFFFF HHHHHHHHHHHHHHHHHHH SSSSSSSSSSSSSSSSSSS RRRRRRRRRRRRRRRRRRR C`
followed by LF, with one space between fields. `T` is `0` (control), `1` (w1)
or `2` (w2); the four zero-padded 19-digit decimal fields are frontend send,
host receive, host send and frontend receive wall-clock Unix nanoseconds. `C`
is `0` (valid sample), `1` (request failure) or `2` (invalid sample); unavailable
host timestamps are zero. Frontend send is sampled after connection setup and
is zero when no connection became ready. The capacity marker has `T=3`, `C=3` and four zero
timestamps; it is file-wide, not a pre-run sample. Later lost-attempt counts
are unknown; the marker stops further probing. Restarts append, completing
a partial capacity marker or terminating another partial tail as one malformed
line. Readers wait at an unfinished last line without advancing its cursor;
the Go reference parser is `internal/clockprobe.ParseLine`. The Python reader
uses this same format. Pinned rehearsal services receive no new flags.

Set the private process parameter `clockProbeFile` to collect that run-local
file. An empty value disables probe collection and omits `falsification`.
The existing host inventory identifies the one host with component `signal`.
Only that host reads the file. The same inventory maps `control`, `w1` and
`w2` to clock slots. A missing or duplicate component has no fallback. It
records unavailable evidence. The existing clock admission check is unchanged.

Each existing clock refresh reads complete lines with a separate file cursor.
The reader uses the existing 64 KiB read window. It packs the tracking baseline
first, including its existing wire fallback. It then adds a probe
prefix that fits 23998 bytes of base64(gzip) and 256000 unpacked bytes. Deferred
lines keep their cursor position. An unfinished last line waits for the next
read. File loss, replacement or an invalid anchor records a cursor gap and
resumes at the current file end; it does not replay old attempts. File errors
and invalid optional payloads only add diagnostic counts. They do not change
tracking rows, measured error/rate, read success, admission, causes or verdicts.
Concurrent refreshes do not wait for a probe reader. They keep their normal
tracking reads. A skipped probe collection is not a read failure.
The startup read excludes attempts ending more than one second before arm.

Prefix packing uses at most 13 compression checks per read. A failure diagnostic
can add one check. The baseline and final output each use one more compression.
Packing can defer a fitting prefix instead of finding the largest prefix.
Deferred lines keep their cursor position for the next read. The probe path
must be absolute and must not contain a NUL or newline. The reader opens only a
direct regular file. It rejects symlinks and uses a nonblocking open so a FIFO
cannot delay the clock read.

Version 2 adds optional `falsification`: `frontendSlot`, `records`,
`countsByStatus`, `countsByReason`, `rttHistogram` and `lossCounts`. Each record
has `target`, `fSendUnixNs`, `hRecvUnixNs`, `hSendUnixNs`, `fRecvUnixNs`, `status`
and `reason`. Checked intervals also have `lowerNs`, `upperNs`, `pairErrorNs`
and `rttNs` where available. Absolute nanoseconds are decimal strings. Status
is `falsified`, `not-falsified` or `unchecked`; there is no pass status. Reasons
are `disjoint`, `intersects`, `no-coverage`, `tracking-break`,
`inventory-unavailable`, `request-failed` and `invalid-sample`. Probe cursors,
file paths, addresses and raw errors never enter the public projection.

For each probe, widen frontend `[f_send,f_recv]` and host `[h_recv,h_send]` by
one second on each side. Use each host's own wall times. Both spans need a
preceding tracking row and a closing row. Row k's `maxErrorSeconds` covers
`(t[k-1],t[k]]`. Take its maximum over all intersecting spans, including touching
boundaries and equal-second rows. Missing coverage, gaps over 60 seconds,
overlapping breaks or non-Normal leap status leave the probe unchecked. Break
intervals also receive a one-second margin. Do not age or extrapolate a row.
Round each host maximum up to nanoseconds, then add them to get `B`.
With `L=h_send-f_recv` and `U=h_recv-f_send`, a probe is falsified only when
`L > B` or `U < -B`. Equality intersects. Zero/invalid timestamps, failed
requests and negative RTT stay unchecked. Probes never narrow a clock bound.

Counts and histograms are cumulative snapshots; replace earlier totals instead
of adding them. `records` contains new or revised details. Match a revision by
its target and four timestamps. Later closing rows can resolve an unchecked
record. At most 32768 details, including revisions, are emitted per session;
this same quota bounds retained attempts. Omitted details are counted. No final
drain runs in this slice. A tail with no closing rows stays unchecked and is
counted. Rehearsal probe acceptance waits for release pins with the prober.

`rttHistogram` has `upperBoundsNs` and `countsByTarget` for control, w1 and w2.
Its nine disjoint buckets end at 0.1, 0.25, 0.5, 1, 2, 5, 10 and 50 ms; the
last bucket has no upper limit. It includes valid RTTs even without tracking
coverage. `lossCounts` has `readFailures`, `malformedLines`, `cursorGaps`,
`capacityEvents` and `omittedRecords`. Missing optional metadata from an
attempted probe read counts as a read failure. Missing inventory and skipped
concurrent probe reads do not count as read failures. Keep the last successful
inventory when a refresh fails; the topology is fixed for the session. A capacity
event or cursor gap means the number of missing attempts is unknown. A
falsified record sets the sticky coverage trust latch described below. The pass
rule stays unchanged.

Each event retains at most two gap rows and eight probes per endpoint (the
first six plus the binding probes) and 16 candidate states shared across `first_content` and
`first_new_content`. Each snapshot retains the latest rows in pool traversal
order, up to 128 events and 1 MiB of event JSON payload. Earlier rows are evicted
when either limit is reached. A session writes at most 15 intermediate snapshots
plus the latest final snapshot, and 256 clock refreshes. The final snapshot is
always written on completion, even after the intermediate snapshot cap is
reached; omitted earlier snapshots are counted. Omitted gap rows, probes,
candidate observations, events, snapshots and refreshes have explicit counters. A malformed caller stream has
an `unavailable_streams` count. Older caller evidence can omit the optional
section; mapping/duration fields already present are still projected. No
protocol or result schema changes come from this optional media timing section.
It is not used by classifier agreement, thresholds or pooled policy.
`TimingEvidence.Version` owns its inner schema. Version 1 fields stay fixed;
future additions must use version-aware optionality so older evidence does not
become incomplete.

The optional result `diagnostics` array retains each gate's procedure completion
and fixed reason code, media counters, pooled counters, pending qualification
and caller-stop confirmation. Media and pool diagnostics also retain indexed
undecided metric names and causes, phase inclusion/exclusion causes, safe evidence
field paths, and included/dropped phase, excluded event and saturation counts.
These use the same fixed label allow-lists as rehearsal reports. Each media/pool
projection is capped at 12 KiB to fit the unchanged final frame. Phase causes take
priority; `omittedUndecidedMetrics` and `omittedPhaseDiagnostics` count any rows
that do not fit, while total counters stay unchanged. Each list stops at its
first row that does not fit. It contains no resource output. Inconclusive
diagnostics have a reason even when procedure execution completed. Legacy gate
reasons remain unchanged. These additive
fields keep result schema version 1. Preparation consumes the caller's original
window; procedure and restoration leave one minute each for collection and stop.

The resource-backed runner owns preparation, gate admission, process/release
procedures, caller brokers, the evidence pool and the pinned judge. Host steps
use `host.execute`; all caller and mailbox access uses the same session's
requests, deadlines and latch. Failover's optional store boundary uses
`store.observe` and `store.failover`. Missing required capabilities refuse
preparation before the first resource request with `precondition-failed`;
there is no fabricated operation or unknown effect for that refusal.

Load additionally requires the optional `caller.admission` capability marker.
The free adapter does not offer it or either store capability. This is an
additive protocol-v1 negotiation value, not a new effect request: the existing
caller/mailbox operations carry their already-defined `admission-boundary`
manifest phase. Old peers omit the marker and are refused before effects.
Private configuration must supply `admission.survivorSeconds` and
`admission.boundarySeconds`. The combined media duration plus the existing
seven-minute campaign reserve must fit the 20-minute procedure cap. The separate
caller maximum is 22 minutes; no deadline is renewed. Boundary fit is checked
after the actual survivor stop and idle wait, then again after boundary startup.

`PrepareCallers` proves both setups before survivor begin. The public
`RunAdmission` then collects the survivor, confirms its one reserved stop,
proves control, local workers and store ownership idle, and calls `BeginBoundary`.
The latter requires terminal survivor evidence and the same confirmed stop.
The two-call boundary runs the existing load measurement definition and owns a
separate collection and stop reservation. Its raw JSONL remains in the resource
mailbox and is retained separately in `ResourceResult.BoundaryJSONL`; it never
enters the survivor pool. Known boundary decryption, ICE or loss failures remain
failures even when other evidence is incomplete. Native admission/measurement
verdicts remain conservative and inconclusive; no new passing policy or pinned
judge threshold was introduced.

`TestSessionBinaryOwnsAdmission` exercises both phases through a built command,
missing admission/store capabilities, unknown launch/stop and known boundary
failure against a fake resource server. Its synthetic evidence proves lifecycle
and fencing only, not a paid media pass. `TestResourceStoreFailoverRequests`
pins request-backed observe/failover/observe and unknown fencing.

Effect request operations are unchanged. Caller operations have an
explicit payload contract: Args is `{"manifest": <operatorcaller.Manifest>}`.
The public session allocates that immutable manifest. The resource server
configures its fixed launcher from it. A zero-exit readiness reply carries
`{"ready":true}`; start carries `{"start":"started"}`; stop carries
`{"inactive":true}`. Only a proved `notSubmitted` refusal can establish that
a start was not submitted. Transport `unknown` dominates every payload and
cannot supply operation classifications, including a refusal. Positive product
failures from confirmed transport remain separate from refusal classifications.

Mailbox Args and payloads reuse `operatorcaller.MailboxArgs` and
`MailboxValue`: read requires explicit `found`; create returns `committed`
or `conflict`. Unknown reads never become absence. Writes remain create-only.
Large read values use the existing reply blob envelope and agreed spool.
The bounded decoded value may contain up to `MaxBlobBytes` of JSON, including
base64 data; it is not restricted to a small host observation.

Sequence sessions stream traces as trace frames. Final's `trace` contains only
new terminal entries published after streaming (normally it is empty), avoiding
a duplicate trace that could exceed the unchanged 256 KiB frame limit. Clients
append that terminal tail to the streamed trace.
Request and result traces include allow-listed operation, target and action
labels. Trial polling emits fixed `trial-takeover`, `trial-settled` and
`trial-ready` wait-purpose labels, without retaining status payloads.
The session request bound is 32,768: half-second caller polling can exceed the
old 4,096 bound within a real campaign. Pending timeouts still have deterministic
request order. Concurrent caller collection and host requests serialize through
one exchange owner and share the session's locked fatal state. Admission to that
owner is cancellable: queue time consumes the supplied context and absolute
request budget, and an expired waiter never submits a request.

`testdata/resource-session.json` is a language-neutral negative boundary
fixture. Its inventories reuse frozen observation values with the documented
target-OS translation. `TestSessionBinaryUsesResourceSequence` builds the real
command and checks requests through caller start, unknown/no-retry behavior,
definite refusal and missing-capability refusal against a fake resource server.
It does not claim a completed media campaign.

The engine owns request identities, deadlines and the fatal latch. An adapter
serves requests; it does not choose a gate verdict or repair an unknown effect.
The public operations are host execution, caller readiness/start/stop, mailbox
read/create and optional store observe/failover. Arguments and payloads are
operation-specific JSON. No generic shell-command adapter is provided.

Request IDs and Unix milliseconds are decimal strings. Init supplies the
original armed time and absolute end, at most 105 minutes apart. The engine maps
remaining time to its elapsed clock once. Gate windows are at most 20 minutes;
mutation windows end at minute 103 and final pooling ends by minute 105. Wall
clock changes do not extend these bounds. Each request carries its absolute end
and remaining milliseconds. The adapter must clamp its wait to both. Each active gate fixes its cutoff once;
every request and wait is clamped to that cutoff. Adapter execution must call
`operatortransport.RequestContext` once at admission and retain the returned
context through execution and polling. Expired queued requests receive a proved
`deadline-before-submission` refusal from the pipe adapter boundary.

`host.execute` and optional `host.read-batch` require at least `operator.HostSubmissionMinimum` (30 seconds)
remaining at submission, including read-only host observations. The public
exchange checks this after queueing and deadline clamping, before sending;
adapters must recheck before remote submission because transit consumes time.
Adapters may require more than 30 seconds. The adapter rounds the remaining
budget down to whole seconds after transit; the public floor does not guarantee
admission at the adapter.
The floor does not apply to caller or mailbox requests. A local refusal follows
the same proved `notSubmitted` result path as an adapter refusal. The additive
result `refusals` array retains the request ID, operation, public target/action,
original absolute deadline and remaining milliseconds, and fixed refusal code.
The first 64 refusal rows are retained, and `omittedRefusals` counts later
refusals. Every refusal still reaches terminal request accounting and its result
trace retains the fixed code. Refusals never become unknown effects
or successful steps. These additive diagnostics keep protocol and result schema
version 1; the gate, caller and session maxima and collection/stop reserves are unchanged.

Adapters can advertise optional `host.read-batch` to collect the four
independent inventory reads or the four independent clock reads in one exchange.
Without it, the session keeps the serial `host.execute` path. A batch carries
`HostReadBatchArgs` in `Request.Args`, action `inventory` or `clock`, target
`hosts`, and no outer script. Its four `reads` use the fixed order `relay`,
`w1`, `w2`, `shared`. Each carries the original host script. The adapter validates
all children before submission, keeps the original deadline, rechecks the host
floor for each submission, and collects all results without retry. The local
rehearsal adapter executes children one at a time because its containers share
the service CPUs; simultaneous probes can disturb the pre-fault media baseline.
This still uses one batch exchange for inventory and one for clocks. An adapter
for independent hosts may execute children concurrently.
Whenever any child ran, the adapter must return outer `Transport: Succeeded`
with exit code 0, even if a child failed or timed out. Children carry their own
results in `HostReadBatchOutput.replies`, including host certainty, exit status
and bounded `HostOutput`; child IDs are the corresponding host slots. An outer
`notSubmitted` refusal is valid only when no child was submitted. Outside
adapters with a failed/nonzero outer reply still have their complete child
results classified: definite failures remain failed reads, and unknown or
unreadable child results latch. An outer failure can never authorize a pass.
The batch path is stricter than serial reads for outside adapters: a submitted
`Failed` child without an exit code or a decodable `HostOutput` latches. Serial
pure-read transport failures can remain definite failures without that evidence.
Unknown-child diagnostics retain the fixed host slot and allow-listed reason
alongside the batch uncertainty. Batch unknowns and outer refusals emit the
existing precondition-check diagnostic; an outer-only failure has no proven
child slot. Missing, duplicate, malformed or unknown child results latch the
session. A definite
failure cannot hide another host's unknown result. Batches never include
mutations or control status. Every mutation still waits for fresh four-host
inventory and the existing clock freshness check.

`TestProcedureRoundTripCounts` and `TestMeasurementRoundTripCounts` pin these
host exchange counts in a deterministic immediate-readiness model. They exclude
caller setup, mailbox traffic, evidence collection and variable polling. The
load row is one observation; each additional measurement observation costs
seven exchanges. These counts are transport evidence, not a completed media gate.

| Procedure | Serial adapter | Batch adapter |
| --- | ---: | ---: |
| rollout | 197 | 92 |
| control-restart | 232 | 109 |
| upgrade-rollback | 320 | 149 |
| failover | 63 | 39 |
| relay-deploy | 22 | 10 |
| load observation | 7 | 7 |

Process and release procedures reserve `RestoreReserve` (60 seconds) inside
their original procedure window. Restoration uses that window's full cutoff
and attempts unfreeze before worker recovery waits. Four inventory reads and
up to four clock reads precede unfreeze; a batch adapter uses one exchange per
read set. The reserve and proof checks are unchanged. At 3.05 seconds per read, the reserve
leaves 35.6 seconds for submission, above the 30-second floor plus a one-second
rounding/transit margin. `TestProcedureReservesUnfreezeAdmission` checks this
latency model. Slower reads can still refuse restoration. Failed restoration
sets `restore-failed`, blocks pool qualification and stops successor gates;
the final read-only pool still runs. Known failures remain failures.
`TestFailedRestorationCannotQualifyForPool`,
`TestPoolPromotionRequiresCompletedProcedure` and
`TestRestorationFailureStopsSuccessors` check these verdict and fence rules.
Measure gates use the same `restore-failed` reason and session latch when worker
restoration fails. Known call loss remains a failure. Rehearsal reports retain
`restore-failed` after sanitization. `TestMeasureRestorationFailureStopsSuccessors`
checks the successor fence and final read-only pool. Missing samples alone do not latch.

Each effect is submitted once. Its ID is recorded before writing. A timeout
retains its pending identity. Unknown completion latches before returning to
gate code; late success cannot erase it. Duplicate/mismatched replies are
protocol errors. Fatal-event publication and actual request writes share one
admission lock; a queued fatal event fences submission before the engine reads
it. Duplicate inbound requests are never delivered to the adapter. Already queued
requests must also pass fatal-state admission before delivery. `Peer.Finish`
serializes the final builder and write with fault publication, propagates any
fault into the session latch and rejects an unlatched or passing final artifact. Writer admission rechecks cancellation and deadlines after encoding. No further request can pass the latch, including restore,
caller start/stop and mailbox writes. Resource teardown belongs to its separate
owner. Transport completion and operation evidence remain separate: exit zero
does not prove a valid receipt, and a nonzero mutating exit can still be unknown.
Control status is effectful. The fixed store/worker/relay, inventory and clock
read allowlist is pure. A confirmed pure-read failure can end a gate without
implying mutation. Inventory and clock host replies put `operator.HostOutput`
(`{"stdout":"..."}`) in `Reply.Payload`; certainty and exit status stay on the
reply. Session observation methods validate and decode stdout before accepting
the result. Unknown or malformed replies fence later effects through the same latch.

Control frames are limited to 256 KiB, scripts to 64 KiB, and host output to
23,999 bytes at the host decoder. Larger payloads use a bounded blob reference
with an opaque name, exact size and SHA-256. `operatortransport.ReadBlob` verifies
an agreed local spool root before returning bytes. `Exchange` validates and
resolves every blob before calling an operation classifier. Inline payload and
blob evidence are mutually exclusive. Spool paths and payloads do
not enter public traces. Subprocess adapters use absolute executable paths and
structured argv, with a minimal environment and discarded stderr.

Public categories use an explicit allowlist of complete fixed strings. Resource
names, arbitrary class names and arbitrary category suffixes become
`unclassified`. Initialization requires a lowercase SHA-256 scenario digest;
final artifacts use only that stored identity. This is an intentional privacy
tightening of the original permissive category syntax. Frozen oracle categories
retain their original results. Timeout traces have a deterministic request order.

## Frozen replay coverage

`internal/operatorgates/testdata/transcripts` contains the exact frozen gzip
archive and checksum manifest. Loading verifies compressed, decompressed and
logical member hashes and resolves nested content-addressed blobs. Expected
results are never generated from Go.

`testdata/coverage.json` inventories all 165 scenarios. P1 checks deadline and
sanitized-category projections (430 assertions in 34 scenarios), plus classified
certainty/fencing seams from three scenarios. Object key ordering is canonical;
request/effect order is preserved. Original nested invocation IDs are joined
before allocating the session's own monotonic request IDs. These are projections,
not complete gate, caller, host-script or media replay. P2 adds 179 inventory/clock
function replays and 13 typed observation projections across 20 scenarios. Only
the standalone signal-bracket scenario is fully replayed; other full scenarios
remain pending. Live pipe protocol cases have independent
expectations because the original oracle predates this protocol.

`testdata/mutation-coverage.json` accounts for all 414 original mutation IDs:
160 public mutants are owned and killed, 41 are retired with explicit successor
contracts, and 213 remain assigned to private owners. The 175 additional native
mutants are counted separately. These are accounting categories, not claims that
private code was tested.
The late-start projection retains pending/readiness/outcome order while replacing
the removed multi-launch policy with one pending start. The public mutation edits and additional mutants are in
`scripts/ci/gate-mutation/cases.json`. Four additional resource-session mutants cover retry on unknown, unknown as
failure, fabricated capability and rehearsal bypass. Two further mutants cover
the host submission floor and refusal metadata retention. Two restoration mutants
cover failed restoration qualification and incomplete-procedure pool promotion.
Two additional measure-gate mutants cover the worker restoration latch and known call loss.
Three more mutants cover uncertain transport classification, refusal promotion with an unknown exit status, and queued exchange deadlines. Several old dispatch/mailbox guards converge
on one session guard; each original ledger ID is still applied and killed
separately. `go run ./scripts/ci/gate-mutation` copies source, excluding Git,
secret environment files and build outputs, into a temporary tree and uses an isolated cache with offline, read-only
modules. A kill requires the named baseline test to pass and its mutant to fail
at the named assertion. Build errors, missing tests, panics and timeouts are
invalid. New protocol mutants are counted separately from the 414 original IDs.
Other ledger cases are never included in the killed count.

`make test-operator` runs operator packages, process/caller/evidence composition
tests, both pinned-release comparisons and owned mutants. It is included in
`make check`. Real container rehearsal runs only in its GitHub workflow. See
[operator observations](operator-observations.md) for the typed boundaries and relocation proof.

## Public four-gate rehearsal

The command calls `operatorgates.CheckReadiness()` before release reads or host
effects. Incomplete replay or mutation accounting refuses with the specific
`operator-readiness` reason. This accounting check does not establish rehearsal
or calibration success. `TestRehearseRefusesIncompleteReadiness` pins the refusal.

On a disposable Linux runner with systemd, Docker, ffmpeg, chrony and at least
four allowed logical CPUs, build with `make operator-rehearsal-build`, then run
`make operator-rehearsal OPERATOR_REHEARSAL_RUNNER='sudo -n'`. The command uses
v0.3.0 and v0.3.1 linux_amd64 archives with fixed SHA-256 pins. It reads the
current checkout's template, stages both releases before media, builds both
release-specific judges offline, and uses the unchanged v0.3.0 caller binary
from its verified archive for every pinned gate. Cache the release commits' Go
modules before running.

After the pinned sequence, a diagnostic head-caller phase repeats rollout once
with the `relais-load` that `make operator-rehearsal-build` builds from the
checkout (`bin/relais-load`, beside `relais-gates`). It runs in its own session,
spool and judge copy, saves `head-caller-evidence.json` (with
`headCaller.commit` and `headCaller.callerSha256`) and writes only the
report's `headCaller` section. `Evaluate` never reads that section, so the pinned
verdict is unchanged. The phase is skipped when the pinned session latched. It
shares the deployment and teardown: an unconfirmed head-caller stop still fails
`cleanup`. It adds about 20 minutes. `callerTiming`, at the top level for pinned
phases and in `headCaller`, lists per call the probe count, probe-capacity
invalidation, gaps over 30 s between probe sends (frontend restarts leave about
140 s in rollout and upgrade-rollback), and per move or takeover the nearest
probe, probes within 300 ms and the `timing_evidence` binding probes' age and
drift. These rows are diagnostics, not gate inputs.

Release staging copies each archive to `/opt/relais/relais-stage-<version>.tar.gz`
and passes that exact `file://` path to the template downloader. It verifies
SHA-256 before extraction, then writes the receipt under
`/opt/relais/preview/<version>`. Both upgrade and rollback consume these staged
directories. Container `/tmp` mounts can differ between archive-copy and running
process views. Staging failures name the runner source, copy destination and
downloader source paths.

The workflow separates release reads from execution. Only its release-read step
receives `RELAIS_RELEASE_TOKEN`; it invokes `relais-gates rehearse -prepare-only`.
The execution target uses `OPERATOR_REHEARSAL_FLAGS=-prepared`. The token stays
inside the scoped public release reader and never reaches child processes.

The actual allowed CPU mask determines the layout. Two CPUs belong to the
caller unit, its Go bridge and decoder (`GOMAXPROCS=2`); the remaining CPUs serve
the driver and containers. Before gate faults, a separate 180-second caller
measures send lateness, CPU pressure and decoder counters with the topology
idle. The report keeps the peak p99, maximum lateness and time-weighted flagged
intervals. This check does not replace the pinned judges' event-window guards.
Rejections retain named `causes` and numeric `checks` (rule, observed value and
limit) in the report and `calibration-rejected` log, including partial evidence.
The percentage is computed on every exit from the accepted sample prefix.
Calibration invokes the same hash-verified judge binary as the gates, built
against the pinned release's product types. Its generator entry calls the same
`applySaturation` path: collected sample checks, cadence and cumulative clock
validation, `callerWindow`, `sampleSpan`, interval flags and saturation budget.
There is no separate minimum packet/sample count, media duration or GOMAXPROCS
rule in calibration. The launcher still enforces the declared two-CPU profile.
The first and final intervals may be partial for cadence validation. All
intervals, including the first, are flagged above 1.025 s. A final zero-packet
row is allowed and still participates in timestamp and span validation. Flagged
intervals are clipped to first-video-send through hangup and merged before the
1% budget is computed; time before media and after hangup contributes nothing.
Intervals above 85% CPU per GOMAXPROCS are flagged. The TypeScript runner retains every
stats row; its early-abort window does not replace the final judge's policy.
The A/B pinned-build tests compare calibration with the ordinary generator path
for phased-first/empty-final evidence and rejected CPU/budget controls.

Generator event coverage follows each measurement's baseline, recovery and
freshness horizons, with one sampler period of end padding. It does not require
stats during pre-media registration or an extra period after hangup. Incomplete
measurements still use their conservative observation horizon. The separate
media-span check requires stats through hangup, including the final partial row.
`TestGeneratorCoverageUsesEventHorizon` checks the normal pre-media delay, final
partial interval, uncovered event edges and incomplete-measurement fallback.

The shared session runs rollout, control restart, A→B→A, then relay deploy.
After setup, controls and calibration, the container adapter becomes a resource
server on JSON pipes. It serves the same `ServeSequence` and `ResourceRunner`
used by the session command. The rehearsal no longer supplies its own gate
sequence or direct caller/mailbox procedure ports. This uses in-process pipes
to retain the existing typed report and evidence sinks without adding private
report frames to the protocol. `TestRehearsalUsesSessionResourceRequests`
requires actual resource requests through this entry; bypassing it is a killed
mutant. The unchanged GitHub workflow's prepared execution invokes this path,
including the real container, systemd caller, create-only mailbox, blob spool and
both pinned-release preparation steps. The container run itself remains
workflow-only verification.

Owner decision, 2026-10-28: the free rehearsal proves that the operator procedures
complete and that the media evidence contains no known failure. A shared runner
cannot provide the scheduling isolation needed to settle every media timing
measurement. Media timing is judged strictly only in the paid run. The pinned
judge, its thresholds and the paid-run pass rule remain unchanged.

A free rehearsal passes only when every covered procedure completed, every
caller stopped, calibration passed and cleanup was verified. Any unknown command
result, session latch, precondition failure, driver error or interrupt blocks it.
The judge must report zero failed metrics, lost calls, decryption failures, ICE
restarts, excluded events and dropped or invalid phases. All four phases must be
included, with complete caller and generator evidence. Duplicate packets still
block, and the two existing relay exemptions, `relayLocalTiming` and
`relayConnectionInvariants`, apply exactly as before.

The strict `pooledMedia.status`, qualified move/takeover counts and timing
verdicts remain in the report. They are not rewritten to a media pass. Only
inconclusive `first_content`, `first_new_content` and `distributed_window` metrics
are non-blocking for the free rehearsal. Every row appears in `timingUndecided`
with its gate, zero-based phase and event indexes, metric and sanitized cause
codes. The list must account for every inconclusive metric; a missing row,
unknown metric or missing cause blocks eligibility. `procedureCompleted` records
procedure completion separately from the strict media result. The judge's
`completeMedia`, `lostCalls`, phase diagnostics and `undecidedMetrics` retain the
evidence used by this decision. Timing uncertainty may reduce the strict
qualified counts below 20 moves or 10 takeovers without blocking the free run;
the paid judge still enforces those counts.

`TestRehearsalAllowsUndecidedTiming`, `TestRehearsalTimingDoesNotHideFailures` and
`TestRehearsalReportsEveryUndecidedTiming` pin this distinction. Mutants that
accept failed metrics or dropped phases, or reject allowed timing uncertainty,
must be killed. `TestPaidMediaTimingRemainsStrict` and
`TestPaidSummaryRejectsUndecidedTiming` retain the strict paid rule.

Relay deploy contributes one completed caller and zero events or trial metrics.
Its call must complete with valid caller and generator evidence and no known
media failures. The pool records `relayCalls: 1` separately and applies the
unchanged trial judge only to the other phases. Other gates without trial
collections remain excluded. `TestRelayCompletedCallContributesNoTrials` and
`TestRelayPoolKeepsTrialPopulationUnchanged` pin these populations via `TestPinnedReleaseJudgesMatchFrozenEvidence`, once,
against the byte-identical executable both release commits build.
`TestFrozenRelayProcedureHasNoTrialReceipts` checks the frozen relay procedure
fixtures; their media callbacks are canned summaries, so the raw zero-event
caller case uses synthetic product reports.

The scenario digest is created after binary preparation and before container
boot. Reports without a valid digest are refused; an earlier preparation failure
retains diagnostics without emitting an unidentified report. The two fixed
release pins have identical Go sources, module locks and embedded media; only
release compatibility JSON differs. Their deterministic judge binaries therefore
share a hash. Source commit checks still distinguish the pins.

Negative controls log their input paths, expected refusal, unit state and exit
statuses, without file contents. A `Type=simple` start job can succeed before
credential setup fails; qualification requires `ExecMainStatus=243`, a failed
unit and no process. `TestCredentialControlObservesAsynchronousRefusal` and
`TestCredentialControlRejectsOtherOutcomesAndRestores` pin this distinction.
Every changed file is restored byte-for-byte, affected units must run again,
and the restored clock must pass the same source check. The ACL control restores
exact template rules and repeats its positive authenticated probe even after a
failed negative observation. All hosts are verified again before gates start.

The workflow uploads `runtime/operator-report/report.json`,
`runtime/operator-report/host-diagnostics.log`,
`runtime/operator-report/media-evidence.json` and
`runtime/operator-report/head-caller-evidence.json`. It records the tested commit
(the merge commit for a PR), the separate PR head, template and binary hashes,
release pins, clock samples, CPU masks and phase evidence. Raw mailboxes,
credentials, generated keys and unit environments are not artifacts. Cleanup
runs after partial boot, cancellation and gate failure using deployment-owned
container IDs and caller units. Images remain reusable build cache.
The media artifact retains the judge pool's per-phase caller JSONL, events,
receipts and registration probes, plus the release commit and judge binary hash.
It replaces opaque identities/text with consistent aliases and shifts all
nonzero timestamps by one common offset to a synthetic epoch. Missing, null and
duplicate members and exact known-field number tokens remain intact; unknown
extension values are scrubbed. Replay tests compare every summary field before
and after sanitization for passing, failing and invalid evidence, including
every timing cause family. Recognized product timing reasons retain only fixed,
privacy-safe classifier prefixes. Dynamic text is removed; identities and
unrecognized reasons stay hashed. This preserves `undecidedMetrics.causes`
without changing judge policy or the event coverage window.
To rejudge offline, extract the artifact's `pool` object as the judge input.
Build the pinned judge with `operatorevidence.BuildPinned` from `sourceCommit`
and this checkout's embedded judge sources, verify `judgeSha256`, then invoke
that binary with the input file. Neither caller manifests nor credentials are
needed. Evidence is saved before judgment, including when the judge fails.
Every invalid phase reports a sanitized `issue.path` and fixed `issue.reason`
and prints them in a `media-phase` diagnostic. Each phase qualifies independently;
an invalid sibling keeps the whole pool incomplete but does not suppress other
phases or erase known failures.
Caller failures capture unit state and the last 60 journal lines before cleanup,
with scoped secrets redacted. Unit state includes user, filesystem protection,
working directory, affinity and runtime limit. Loader stderr and file failure
paths remain visible without credential contents.
The fixed caller launcher accepts exactly one mailbox mode. Directory mode
retains its existing scripts and rehearsal arguments. Adapter mode supplies an
absolute executable path and lowercase SHA-256; an optional absolute spool
path supports protocol blobs. The start script copies the adapter into the
identity's fresh work directory, checks the staged hash before execution and
sets mode 0700, as it does for the bridge and loader. A mismatch refuses launch.
It passes only the staged adapter path, optional spool and absolute runtime
deadline. The command refuses adapter mode when the deadline is omitted or
empty. Directory mode refuses an orphan `-spool`, including an explicit empty
value. The adapter receives no extra arguments and keeps the transport's fixed
environment; credentials are not forwarded in arguments or environment.
After manifest validation, the protocol mailbox permanently keeps the earlier
of its runtime cutoff and the manifest deadline. Every request also observes
the active context and the ten-second request bound. Stop and final publication
cannot renew the cutoff when they remove cancellation or use a separate grace
context. Unknown results still latch the mailbox without retry; writes remain
create-only and judge policy is unchanged.

The operator must provide a trusted work root and trusted ancestor directories.
The launcher does not validate their ownership or permissions. Fresh-directory
creation and staged hashes protect the copied bytes; protection from replacement
between verification and execution depends on that trusted ancestry.

`TestDirectoryLaunchScriptsStayUnchanged` compares both directory scripts
byte-for-byte with pre-adapter fixtures. `TestStartScriptStagesMailboxAdapter`
executes staging, quoted flags and hash refusal. Its copy-boundary controls
replace the source during copy, including restoring the original source while
leaving changed staged bytes. Mutants cover source-only and pre-copy hashing.
`TestCallerRequiresAdapterDeadlineBeforeStart` covers omitted and empty deadlines
without starting an adapter. `TestBridgeCapsCleanupToManifest` enters the same
post-validation lifecycle used by `RunBridge`, runs a real child with an invalid
stream, and checks the actual stop, report and finished mailbox requests against
both earlier and later runtime cutoffs. It uses a synthetic host observation;
it does not qualify the Linux host profile. `TestMailboxAdapterSubprocess`
checks real subprocess requests, create conflicts and unknown fencing on all
platforms. `TestCallerBinaryUsesMailboxAdapter` runs the generated script and
built caller with a fake subprocess adapter on Linux through setup and a
confirmed conductor stop, plus an unknown-create control. It proves mailbox
wiring and lifecycle only; it does not run media or systemd.

The launcher waits for executable startup with `Type=exec`; the existing mailbox
handshake still proves caller readiness. Signaling probe failures retain TLS or
HTTP failure codes, and missing completion evidence retains the underlying error.
The preliminary signaling burst remains optional; the required registration
burst still supplies the final caller clock and proximity proof.
Calibration and gate callers share the same start function and P4 launcher.
After calibration stops, fresh inventories and product status must prove zero
control calls and reservations, plus two live, non-draining, locally empty
workers, before the first gate. Precondition diagnostics identify the host,
observation and fixed failure code; reports retain those categories. A caller
proved never submitted needs no stop, while unknown starts still require proof.
Each negative-control restore requires that same health proof before the next
control. The missing-unit restore uses P3's `GatePrecondition` repair to prove
empty, stop, replace and start the worker, clearing control's draining mark.
After a confirmed stop, repair waits for control's settled-death proof even if
its last heartbeat still says alive. Confirmed repair steps are logged before
the final running-process inventory and full health check.
Sanitized control-status lines record the flags and counts before repair, after
every restore and after calibration. The boot-time ACL control's full deployment
check runs once all hosts exist; no store management credential is retained.
Calibration reserves two minutes for preparation: the unchanged loader supplies
one initial clock probe and one every ten seconds, so ten-probe registration
needs at least ninety seconds after loader startup. The media calibration runs
for 180 seconds so a 1.25-second phased first interval stays below the judge's
1% budget, within a six-minute caller runtime limit.
Caller preparation, including the ten-probe registration, precedes each fresh
20-minute gate window. Campaign admission uses one elapsed-clock snapshot so a
1,200-second minimum fits a full window exactly. Before launch, the caller's
separate 22-minute manifest includes startup and must fit media plus 120 seconds; both windows stay
clamped to the original session cutoff, with mutations ending by minute 103.
Procedure/restoration ends at least two minutes before the caller cutoff,
collection ends one minute before it, and stop stays within that cutoff.
The archived 21-minute manifest rejection in `26b5fb0694a332d7.json`, event 7,
retains its original bytes. `TestOracleCallerManifests` explicitly checks that
case under the new 22-minute policy; native manifest tests also reject a window
one millisecond above the new maximum.
Their UDP bind uses the runner's route source for the relay, with port zero;
the loader rejects wildcard bind addresses. The bridge checks the staged loader
hash, executable bit, certificate readability and ffmpeg inside the caller unit.
Unknown outcomes emit structured `command-result-unknown` diagnostics and remain
in the report's `uncertainties` array. Each includes the operation kind, request
ID, logical target or caller ID, action, deadline and remaining milliseconds,
and the fixed certainty rule. Payloads, raw mailbox keys and error strings are
excluded. Diagnostics never clear the fatal latch or authorize a retry. Caller
journals are collected on unknown outcomes even when the unit remains active.

PR checks and one complete execution are not initial profile qualification.
Keep three declared complete executions at the same code/profile/bounds; all
three must meet the strict rule, without discarding failures. This rehearsal
covers a local TLS proxy, local time reference and shared runner clock. It does
not cover managed store failover, load admission, independent host clocks,
arm64 execution or cloud resource lifecycle and billing.

NOT-RUN — The command assembly and report policy have offline tests. Real
Docker execution and free-runner calibration require the workflow artifact.

The shared numeric contract accepts exactly `-1` for
`callharness.MoveReport.From` and `.To`: distributed/external callers cannot map
worker names to local indexes. These are value exceptions in
`EvidenceNumericSentinels`, with release-source locations, rather than optional
fields. Other negative values and out-of-range indexes remain invalid.
`TestDistributedMoveUnknownWorkerIndex` checks both boundaries;
`TestDistributedCallerWorkerSentinelsQualify` checks the complete judge path.

### Enforced event clock coverage (slices 4a and 4b)

Clock evidence stays version 2. The judge now makes uncovered event windows
undecided. It does not change the pass rule or make a passing window fail.
Independent failures remain failures. The fixed bounds stay at 2 ms for control,
3 ms for workers and 100 ppm for caller drift. The pre-fault `clocks.Check`
stays unchanged. Lease aging, `maxclockerror` declaration and receipt binding
belong to slice 5. There is no freeze-gap rule.

A session retains every clock read attempt, including successful slots in a
failed batch. Those slots can already have advanced their tracking cursors.
Dropping their rows would lose acknowledged evidence. A failed read alone does
not create a coverage gap: the next successful cursor/anchor read can recover
all intervening rows. Real log loss and tracking breaks remain recorded. The
successful pre-fault cache is separate and still contains successful checks
only. Policy history keeps its earliest 256 attempts; drain history has its own
64-attempt budget. `omittedAttempts`, `omittedDrainAttempts` and
`omittedBrackets` are separate counters. Overflow preserves the retained prefix.
Per-slot `missingIntervals` bound omitted evidence, so a later drain cannot
bridge omitted rows; dropping an exact replay creates no loss interval.
`report.clocks` remains the successful policy-sample cache, with its original
Control/Worker/Drift values. Attempts and drain samples live only under coverage.

One optional `clockCoverage` object belongs to the run-level media `Pool`, not
individual phases. It contains `version`, inventory-derived `frontendSlot` and
`controlSlot`, fixed `bounds`, cursor-free numeric policy `attempts`, separate
`drainAttempts`, omission counters and per-slot `missingIntervals`,
`killBrackets`, `windows`, diagnostic `results` and `drainComplete`. Each window
uses zero-based `phase` and `event` indexes and preserves its receipt's start,
end, kind and affected worker slot. Brackets retain worker slot and decimal
Before/After/midpoint nanoseconds, without operation or session identities.
Results contain sorted, unique causes. Before transport, the collector evaluates
the retained evidence with the shared fixed bounds. A different configured
control bound, worker bound or drift makes coverage undecided with
`clock-coverage-bound` in both recorded results and qualification. It sends one compact `clockCoverage`
object with `results` and the `falsified` latch. It does not send tracking rows.
The existing total judge input limit still applies. Each pinned release first
runs its own `r.Judge()`. The added qualification then marks matching windows
undecided and removes trust from their metrics. Trusted failures stay trusted.
The same release judge rebuilds the yardstick, distributed result and run
outcome. This applies to every phase and to the final pool. Gap populations and
trial counts stay unchanged.
Rehearsal reports also retain
this projection, including the separate head-caller sequence. Public projection
removes tracking source labels, raw output and private cursor members. Retained
media artifacts keep the existing fields byte-identical with and without
coverage. Coverage uses a whole-second origin to preserve tracking-log seconds;
`mediaTimeOffsetNs` records coverage time minus legacy media time. Subtract that
offset to compare coverage endpoints with caller timestamps. Coverage's own
rows, breaks and endpoints always share one shift.

The standard-library-only `observe` evaluator is deterministic and reads no
clock. It checks control Start–End continuously for planned moves; for takeovers
it checks the entire killed-worker signal bracket and control completion End.
It expands host spans by one second for log resolution and frontend spans by
the fixed pair bound plus one second. Breaks receive a separate one-second margin, so
span and break padding intentionally permit overlap across two seconds of
separation in total. Every required interval needs a preceding
and closing tracking row. The envelope is the upward-rounded maximum column-14
error over every intersecting interval, including touching boundaries and
updates in the same second. Source slots come from unique inventory components;
signal and control on the same slot still contribute both error terms. Decimal
nanosecond conversion precedes ceiling: `1.230e-04` is exactly 123000 ns, while
true sub-nanosecond digits still round outward.

The enforced causes are:

- `clock-coverage-missing`: missing endpoints/tail, invalid evidence, a row gap
  above 60 seconds, or overlapping cursor gap, log rotation or log loss.
- `clock-coverage-bound`: frontend maximum plus host maximum exceeds the fixed
  control or worker bound. Equality is accepted.
- `clock-coverage-break`: an overlapping step, restart, source change,
  non-Normal leap status or self-consistency break. Break intervals also receive
  a one-second margin.
- `clock-coverage-kill-bracket`: missing/invalid signal bracket, or its rounded-up
  half-width plus frontend and killed-worker error exceeds the worker bound.

- `clock-coverage-falsified`: any falsified probe record, or a positive cumulative
  falsification count, removes clock trust from every window judged from then on
  in the run.

The rehearsal owns the forward-sticky latch for the whole run ID. Each later
sequence, including the head caller, seeds its new Session from that latch. A
pool judged before the falsification is not judged again. Later clean reads
cannot clear the latch. It is set
before retention limits can drop an attempt. A disabled prober cannot set it.
The free rehearsal timing exception cannot waive a clock coverage cause.
It does not change the session's transport safety latch or admit a fault.
A frontend caller-probe outage adds no special cause. Existing caller mapping
still applies 100 ppm drift.

New runs without coverage evidence get `clock-coverage-missing`. Historical
archives may opt into `preCoverageEvidence: true` to retain their frozen verdicts.
This explicit path supplies no measured coverage. Live `ReadPool` never sets it.
An absent or null coverage field uses historical policy only with that marker.
A supplied coverage object always takes precedence, including an invalid object. Tests
check both paths at both pinned releases. The synthetic coverage fixture is
`internal/operatorevidence/judge-src/testdata/clock-coverage.json`; it is test
input, not a reconstruction of measurements missing from an archive.

`EvidenceRuntime.Media` only snapshots retained coverage on each phase judgment;
it does no host reads or waiting. Final pool preparation drains tracking tails
before constructing the final judge input, within the original pool collection
cutoff, leaving 110 seconds of the existing two-minute pool budget for the judge.
When less time remains, drain stops at that reserve or is skipped.
Falsification observed during drainage adds `clock-coverage-falsified` to the final judge input and fails closed.
A clean closing tail can remove `clock-coverage-missing` qualification;
unclosed tails remain undecided. Independent failures and refusals stay intact.
`FinalizeDiagnostics` still runs only after the pool deadline decision. It retains
diagnostics on pool decoding failure when no final judge ran. The final report
and judge input share the drained snapshot when final judging succeeds.
An optional isolated `ClockCoverageReader` can drain tracking evidence outside
`Session.Exchange`, whose unknown replies must still latch policy requests.
The rehearsal adapter provides this isolated reader. Without one, an uncovered
tail stays incomplete; no unsafe transport fallback is used.
Diagnostic cursors do not acknowledge the policy reader's cursor, so later
policy reads may repeat rows; duplicate intervals do not narrow the maximum.

Drain stops when every required slot has a closing row stamped at least
`ceil(last-required-end + applicable-bound + 1s)` and its latest read consumed
its entire file snapshot. The bound is zero for host-local endpoints and the
pair bound for frontend endpoints. The additive tracking-log `readComplete`
field distinguishes a complete read from a capped prefix or partial final
line. It is false when absent in older evidence. Collection repeats bounded
reads with one-second waits only while that condition is unmet. It uses at most
ten seconds within the original collection context and elapsed cutoff, before
the final judge; it cannot extend the pool or session budget. Windows with missing
receipts/brackets or invalid timestamps are not drain-resolvable and do not
hold the stop condition open. Their diagnostic causes remain. `drainComplete`
means no resolvable tail remains, rather than that every event is covered.
Timeout, missing adapter
or backlog produces incomplete diagnostics, never a media error or a pass.

For offline inspection, use stored files in the same clock domain:

```sh
go run ./scripts/dev/clock-coverage -report report.json -pool media-evidence.json \
  -frontend-slot shared -control-slot shared
```

The tool accepts a direct Pool or the retained artifact's `pool` envelope. New
reports already contain their windows and slots, so `-pool` and slot overrides
are optional. Add `-head-caller` when using `head-caller-evidence.json` to select
the report's separate head sequence. Old reports without event inputs explicitly report that limit.
When the 4a field is absent, the tool reconstructs timelines from stored
`clocks[*].Clocks.<slot>.trackingLog` samples. It merges repeated seconds using
the largest error and any non-Normal leap status, and deduplicates breaks.
Historical boot rows remain available as predecessors; only intervals that
intersect the required span can contribute a leap break or error bound.

Older sanitized pools use a 2000 timestamp origin while report clocks retain
real UTC. The tool aligns them by intersecting the millisecond start-time
buckets in matching `callerTiming` events across the run. It preserves the
remaining uncertainty in `timestampUncertaintyNs` and widens each checked
endpoint by that amount through the shared evaluator. Worker slots come from
event worker numbers, since retained receipt identities are aliases. Ambiguous
or inconsistent alignment is refused. The former
`report-and-pool-must-share-clock-domain` note described this real mismatch but
did not correct it; aligned replays now report the alignment explicitly.

Historical reports lack kill brackets. Their coverage, bound and break checks
still run through the shared evaluator. Kill results carry
`killBracketRule: "unavailable"` and a diagnostic note, not an invented cause.
`covered` counts the available rules; it does not prove the unavailable bracket
rule or establish a judge verdict. Synthetic or missing coverage must not be
substituted for stored measurements. Old verdict fixtures retain an explicit
pre-coverage path. The coverage fixture is generated deliberately. Named tests
cover `repair-gap-with-later-sample`, `omit-frontend-error` and
`omit-final-coverage`. Mutation tests reject use of the latest error, endpoint-only
coverage, omission of the break margin and a falsification latch that can clear.

## Rehearsal admission receipt (slice 5c)

Any launcher can deploy `internal/operatorgates/host/clock_probe.py`. Start each
endpoint with `python3 clock_probe.py --listen PRIVATE_IPV4:PORT --seconds 150`.
The frontend also needs `--output RUN_LOCAL_FILE` and all three
`--peer control=http://PRIVATE_IPV4:PORT`, `--peer w1=...`, `--peer w2=...`.
Use an isolated private network: this unauthenticated helper accepts private IPv4,
bypasses proxies, timestamps after TCP connect, and probes at 1 Hz (750 ms socket
timeout) into the existing 4 MiB capped format. Lifetime is bounded to 1–300 s.

After negative controls restore, rehearsal establishes/drains cursors before
starting helpers or the admission timer, without reading the probe file. This
preflight has a ten-second budget including a two-second guard to keep overflow
breaks outside the selector's margin. Startup history remains recorded. Then
parallel reads/selection start at 123 s within a fixed 150 s admission deadline;
setup has ten seconds inside that deadline and helper lifetimes use the remainder.
Non-cancelled exits wait through deadline +2 s before calibration (152 s plus
preflight, at most 162 s). Parent cancellation skips that wait and returns promptly;
deployment cleanup removes the helpers with their containers. Generated chrony
configuration declares hosts and caller unsmeared.

The 5 ppm growth policy uses runs 42302414107/42330487259: their maximum
successful-read residual-plus-skew was 0.251/0.147 ppm. Adding the configured
1 ppm allowance and 0.005 ppm printing pad at 200 ms gives about 1.256 ppm.
Selection still checks each measured growth rate. This shared-kernel data does
not characterize independent hosts; changed reference updates can refuse leases.

The real CLI archives `clock-selection.json`, `clock-admission-evidence.json`,
`clock-selection-policy.json` and `clock-selection-status.json`. Report
`clockSelection` and logs print digests, bounds and counts or the fixed refusal
cause. Archives omit addresses, source labels and opaque cursors. Selection stays
diagnostic; acceptance, caller flags and judge inputs do not use it.
`TestClockSelectionAdmissionArtifacts` exercises the real reader with oversized
logs, a transient read failure and the real CLI. PR admission is a separate gate.

Admission now re-measures an expired lease for at most four current poll periods
before invoking the selector. It requires a newer reference update and retains
tracking pages/cursors even when the lease is omitted. A failed refresh records
`clock-lease-expired`; admission retries until its existing deadline. This
changes only recorded receipt admission and adds no time to the helper deadline.

The unwired `LeaseGuard`, `Session.ReadLeases`, `StepWithLeases` and
`FailoverWithLeases` prepare fault enforcement for atomic activation. They age
from monotonic request-send (including transport), never renew the same update's
two-poll expiry, and check control pairs against B_control and worker pairs
against B_worker−K. Checks repeat after arm and after the exchange lock. Start,
unfreeze, replenish, required rollback and explicit recovery replacement bypass
clock/arm hooks while retaining inventory and unknown-result latches. Refusals
are `clock-lease-missing`, `clock-lease-expired`, `clock-lease-bound` and
`clock-config-differ`. Existing gate/verdict paths do not call these interfaces.

Signal brackets optionally retain `signalDiagnostics`: helper CPU IDs before/after,
thread involuntary/voluntary context-switch deltas, thread CPU time, and priority.
`threadCPUTimeNs` comes from `ru_utime`/`ru_stime` at microsecond resolution.
Usage deltas include the adjacent `getrusage` calls; CPU reads are outside them.
The one-shot helper attempts nice −20 around the signal, restores its original
priority before any subprocess or exit polling, and records `unchanged` if denied.
A restoration error records `nice--20-restore-failed`; the helper remains finite.
These fields never qualify evidence or change a verdict. Missing/malformed
optional diagnostics are ignored; existing bracket timestamps remain authoritative.
