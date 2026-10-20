# Capacity evidence for #40

## 2026-10-20 — matched split checkpoint traffic, run 39441746025

[Run 39441746025](https://github.com/relaishq/relais/actions/runs/39441746025),
commit `5360c616cfadf99992b4c6b81187959ed263852a` (`5360c61`, draft PR #213),
completed the manual `split-capacity` job successfully. This is **one runner's
fixed-load measurement, not a capacity result**. The legacy history workload
exceeded the store byte gate; ordinary split traffic stayed below it, but
**sustained history churn at split defaults exceeded the gate**. The default
payload rate is under review in [#214](https://github.com/relaishq/relais/issues/214).
This does not increase the planning limit of about 200 calls per Redis primary.

Evidence: artifact `split-capacity-linux-39441746025`, containing
`split-capacity-linux.json`, `.approval.json`, `.log`, `split-capacity-build.txt`
and process logs in `load-run-2704053099/`. The original reporter at that commit
returned `blocking=[]` with 1,000 µs Save CPU/wall budgets. That reporter gated
whole-window traffic and missed the sustained-churn failure below. Its successful
workflow conclusion is not a passing churn result. A rerun with a separate
`churn` phase is required; the original raw evidence and verdicts remain intact.
Strict audio exceptions are also retained below.

Runner `runnervmxehtw`: `ubuntu-latest`, Linux/amd64, four vCPUs (AMD EPYC 7763),
Go 1.26.9. Three worker processes, one relay, one control, two caller processes
with `GOMAXPROCS=1`, standalone owned Redis and a counting RESP proxy shared the
runner. Topology was `loopback-processes`, without network namespaces or TLS.
Every step used fresh Redis; each matched pair used the same five binary hashes,
load, default relay buffer, 50 audio inputs/s/call and 30 VP8 frames/s/call.
Samples lasted 30 s except rotation (65 s). No load ramp was attempted.

Before history used an 8,192 B app limit and the documented Demo A overrides
`-agent-max-snapshot-bytes=24576`,
`-agent-snapshot-bytes-per-second=270336`,
`-agent-snapshot-burst-bytes=98304`. After used only
`-agent-split-checkpoints` and default limits. Echo used stock limits in both
arms, with that split switch off/on. The keyless history fixture has four
exchanges of 512 B per side: 2,871 B working JSON plus 4,205 B raw history in
split mode, or 7,087 B inline JSON before. Inputs change working state at about
50/s; every 250 accepted inputs changes history (nominally once/5 s). Churn
tries a history change on every input after input 400. Side-effecting no-op
intents once per turn exercise forced checkpoints; no provider was called.

### Per-step gates and recovery

All values below are measured on this runner. Traffic includes session, frame
and owners clients, requests plus replies; rates are per call-second over the
media window, including fault/move intervals. Gates remain 100 commands and
131,072 B/call-second. Ages are `p99 / maximum`, in ms, sampled every 100 ms.
The policy gates p99 at 550 ms; every sampled maximum was also below 550 ms.

| Arm/scenario | Seconds | Generator | Recorded whole-window service | Commands/call-s | B/call-s | Checkpoint age | Snapshot age | Live/lost calls | Cleanup |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| before/steady | 30 | valid | fail | 72.67 | 184,520 | 108.14 / 121.92 | 110.93 / 125.13 | 2 / 0 | pass |
| after/steady | 30 | valid | pass | 72.74 | 95,855 | 108.57 / 109.29 | 110.48 / 111.01 | 2 / 0 | pass |
| before/echo | 30 | valid | pass | 73.32 | 84,536 | 85.36 / 86.90 | 86.59 / 89.31 | 2 / 0 | pass |
| after/echo | 30 | valid | pass | 73.35 | 62,718 | 85.37 / 87.20 | 86.54 / 88.53 | 2 / 0 | pass |
| after/history-churn | 30 | valid | pass; **sustained churn fails** | 73.11 | 121,690 | 110.08 / 110.87 | 111.82 / 112.40 | 2 / 0 | pass |
| after/rotation | 65 | valid | pass | 74.75 | 97,367 | 114.58 / 115.47 | 116.83 / 118.53 | 2 / 0 | pass |
| after/n-plus-one | 30 | valid | pass | 70.67 | 92,781 | 111.43 / 112.22 | 113.72 / 114.26 | 2 / 0 | pass |
| after/drain | 30 | valid | pass | 72.60 | 95,613 | 119.72 / 120.23 | 122.07 / 122.89 | 2 / 0 | pass |

The original whole-window report failed before/steady at 184,519.52 > 131,072
B/call-second; its other checks passed. The separate sustained-churn audit also
fails the byte gate at **132,347.7 B/call-second**, despite the original churn
window average of 121,690.50. Both command rates remain below 100.
After/steady saved 88,665.00 B/call-second (48.05%); echo saved 21,817.63 B
(25.81%). These are matched workload observations, not Redis CPU or capacity
multipliers. Generators had zero skipped frames, no saturation, peak interval
send-lateness p99 of 2.36–4.43 ms (<20 ms), and peak CPU/GOMAXPROCS of
2.50–3.15% (<85%). Both callers completed every step.

**Audio qualification:** the asynchronous recording/playback fixture is judged
by the existing custom-agent policy, which excludes echo-style audio freshness,
audio loss and audio track-gap checks. It still requires returned audio,
connection survival, video delivery/recovery and decryption checks. The raw
strict audio yardsticks below remain failures or inconclusive; no loss-free or
continuous-audio guarantee follows from custom `service=pass`.

| Arm/scenario | Raw audio units lost | Raw video frames lost | Decryption failures | Event yardsticks outside pass |
| --- | --- | --- | --- | --- |
| before/steady | 1620 | 0 | 0 | audio_freshness: 0 fail / 1 inconclusive |
| after/steady | 1622 | 0 | 0 | audio_freshness: 0 fail / 1 inconclusive |
| before/echo | 0 | 0 | 0 | all pass |
| after/echo | 0 | 0 | 0 | all pass |
| after/history-churn | 1620 | 0 | 0 | audio_freshness: 0 fail / 1 inconclusive |
| after/rotation | 3546 | 0 | 0 | audio_freshness: 2 fail / 1 inconclusive, audio_loss: 1 fail / 0 inconclusive |
| after/n-plus-one | 1746 | 0 | 0 | audio_freshness: 0 fail / 2 inconclusive, audio_loss: 1 fail / 0 inconclusive |
| after/drain | 1619 | 0 | 0 | audio_freshness: 0 fail / 1 inconclusive |

All event video-loss, video-freshness, resume, first-content, first-new-content
and first-live checks passed. After N+1's strict audio-loss rows failed for both
takeovers; rotation's third drain had one strict audio-loss failure and its
second/third drains had audio-freshness failures. These are not hidden by the
workflow success. Echo's strict audio and video checks passed with zero loss.
All callers reported AuthTag/Replay/Other decryption counts of 0/0/0, and every
move/takeover reported zero decryption failures after resume.

- Churn: one `Capacity` refusal was observed, with 3,273 admission calls
  and 3,272 Handle/Save events. On that call, the next diagnostic
  sample had `inputs=1071`, consumed input count 1072 and `refused=1`, consistent
  with the refused candidate being rolled back before acceptance. Input progress
  subsequently reached 1486; the other caller reached 1458. Both stayed live.
  During full churn, history was still written about 8.5 times/s/call, with one
  admission refusal and zero budget skips. The refusal did not keep sustained
  wire traffic below the gate. This observes refusal and continued processing; it is not the
  separate exact-history crash proof in ADR 0012 §6(2).
- Rotation: three occupied workers drained one call each in 19.52, 13.45 and
  14.79 ms, then restarted with active key 2 plus decrypt key 1. All three had
  new PIDs and fresh post-start membership heartbeats. At
  `2026-10-20T14:49:50.089085631Z`, the authenticated inventory covered both live
  calls and all ten referenced tuples (five/call): `{"2":10}`, none on key 1.
  Both calls remained live through the end. The old decrypt key was not removed;
  unreferenced banks and continuous inventory after that instant were not tested.
- N+1: worker 0 held both calls when killed. Both takeovers were recorded;
  detection was 409.85 ms, with takeover operations of 21.39 and 23.12 ms.
  A later planned move took 13.29 ms. Final placement was two calls on worker 1;
  survivor peaks were worker 1 = 2 and worker 2 = 1. No call remained on the
  dead worker. This is a two-call recovery observation, not a capacity floor.
- Drain: both calls left worker 0 in a 26.04 ms API interval; their individual
  moves took 23.83 and 20.99 ms. Final placement was one call each on workers
  1 and 2. Ordinary planned moves in the matched steps took 11.40–18.10 ms.
- Cleanup: all eight scans passed after the standard 35 s relay wait. Control,
  live workers and relay route/hold/buffer/pending counts were zero. No owned
  process or listener remained. Only bounded ended/epoch retention, the released
  relay fencing hash and session counter remained; no v10 component keys remained.
  Absence after this wait does not distinguish explicit deletion from lease TTL.

Age coverage was complete under the sampler policy: 600 samples/30 s in ordinary
steps, 1299/65 s in rotation (one missing session sample), and 592/30 s in N+1
(eight missing session samples during recovery). There were no sampler errors.
These are sampled ages of observable owned calls, not continuous maxima across
an unowned takeover interval.

### Setup, steady state and complete lifecycle traffic

Setup is the pre-media interval. Steady excludes explicitly marked move, kill,
drain and rotation intervals. Commands and B below are **whole-topology totals**;
normalizing the short setup/fault intervals as sustained rates would mislead.
The last columns divide all observed setup/media/hangup/cleanup traffic by the
configured media call-seconds (60, or 130 for rotation), retaining cold attempts.

| Arm/scenario | Setup s | Setup commands / B | Steady s | Steady commands / B | Lifecycle commands/call-s | Lifecycle B/call-s |
| --- | --- | --- | --- | --- | --- | --- |
| before/steady | 1.333 | 124 / 325,029 | 29.981 | 4331 / 11,015,037 | 81.05 | 198,687.83 |
| after/steady | 1.330 | 125 / 137,825 | 29.983 | 4334 / 5,699,220 | 81.03 | 104,277.82 |
| before/echo | 1.328 | 130 / 137,484 | 29.990 | 4373 / 5,047,227 | 81.90 | 92,442.50 |
| after/echo | 1.329 | 132 / 112,369 | 29.991 | 4374 / 3,739,241 | 81.97 | 69,484.85 |
| after/history-churn | 1.329 | 125 / 128,280 | 29.981 | 4356 / 7,249,041 | 81.35 | 130,318.20 |
| after/rotation | 1.328 | 126 / 143,787 | 53.593 | 7755 / 10,254,987 | 78.67 | 101,294.75 |
| after/n-plus-one | 1.329 | 121 / 143,276 | 29.981 | 4335 / 5,684,405 | 81.43 | 104,503.68 |
| after/drain | 1.331 | 119 / 121,861 | 29.974 | 4299 / 5,648,486 | 80.80 | 103,787.52 |

The original churn media average (121,690.50 B/call-second) and lifecycle
average (130,318.20) hide the sustained failure. Churn begins at input 400,
about eight seconds into the sample. The proxy's full-churn bins **10–30** cover
21 seconds and measure **132,347.7 request+reply B/call-second**, **73.52
commands/call-second**, and **74,849.1 published payload B/call-second**. Payload
remained below the 80,000 B/s default while wire traffic exceeded 131,072 by
1,275.7 B/call-second. This is direct evidence that the default rate is too high
for this fixture's measured protocol residual. The peak was **306,885 B** across
the topology in bin 26, with no event running. These are sustained-window
measurements, not a new per-second burst limit. The corrected observer marks
`churn` from StartedAt + 9 s through the media window's end and gates that phase
independently; missing or truncated churn evidence blocks the report.
The ordinary after/steady phase alone was
72.27 commands and 95,040.51 B/call-second; adding its marked move gives the
95,854.52 headline (small observer/counter timing differences remain).

| Arm/scenario | Session B/call-s | Frame B/call-s | Owners B/call-s |
| --- | --- | --- | --- |
| before/steady | 148,356.93 | 31,122.57 | 5,040.02 |
| after/steady | 59,624.71 | 31,179.71 | 5,050.11 |
| before/echo | 48,305.94 | 31,179.77 | 5,050.05 |
| after/echo | 26,519.18 | 31,149.25 | 5,049.70 |
| after/history-churn | 85,502.00 | 31,138.01 | 5,050.49 |
| after/rotation | 61,040.98 | 31,307.20 | 5,018.60 |
| after/n-plus-one | 57,692.42 | 30,028.28 | 5,060.18 |
| after/drain | 59,339.50 | 31,149.67 | 5,124.25 |

### Component writes, keys and fields

A length-only RESP proxy counts attempted commands and exact request/reply
frames. Redis `MONITOR` observes actual internal Lua HSET field/value lengths;
initial empty placeholders are retained in those counts. Driver inventory,
cleanup scans and INFO reads bypass the proxy. The observer adds overhead in
both arms. Counts are Redis protocol bytes, not TLS record, NIC, replication,
managed durable-log or billing bytes; those are **NOT-RUN**.

Steady-phase successful writes below show `count @ writes/call-second` and
ciphertext B/write. A reused work/history component is not counted as a write.
In this original artifact, the churn step's `steady` phase includes its ramp and
full churn; the bin audit above isolates sustained churn.
The fixture's accepted input progress was 48.6–49.7/s/call in ordinary samples;
transport commits remained about 10/s despite the much faster input changes.

| Arm/scenario | Legacy | Checkpoint b | Working | History |
| --- | --- | --- | --- | --- |
| before/steady | 588 @ 9.8062; 13,108–14,136 B | — | — | — |
| after/steady | — | 589 @ 9.8222; 665–1,520 B | 507 @ 8.4548; 2,907–2,907 B | 10 @ 0.1668; 4,241–4,241 B |
| before/echo | 603 @ 10.0533; 3,328–3,570 B | — | — | — |
| after/echo | — | 603 @ 10.0532; 339–387 B | — | — |
| after/history-churn | — | 597 @ 9.9563; 662–1,517 B | 506 @ 8.4386; 2,907–2,907 B | 372 @ 6.2039; 4,241–4,241 B |
| after/rotation | — | 1049 @ 9.7867; 665–1,526 B | 912 @ 8.5086; 2,907–2,907 B | 20 @ 0.1866; 4,241–4,241 B |
| after/n-plus-one | — | 587 @ 9.7894; 665–1,532 B | 500 @ 8.3385; 2,907–2,907 B | 10 @ 0.1668; 4,241–4,241 B |
| after/drain | — | 577 @ 9.6249; 665–1,521 B | 502 @ 8.3739; 2,907–2,907 B | 10 @ 0.1668; 4,241–4,241 B |

In after/steady setup, successful writes were checkpoint 20 × 665–666 B,
setup 2 × 1,298–1,300 B, work 2 × 2,907 B and history 2 × 4,241 B.
No setup write occurred in its steady phase. Raw working/history lengths
2,871/4,205 become 2,907/4,241: 6 B component encoding plus 30 B AEAD overhead.
Across all custom split steps, checkpoint ciphertext was 662–1,532 B, setup
1,296–1,300 B, work 2,907 B and history 4,241 B. Echo used 36 B explicit empty
work/history at initialization and no app-component rewrites in steady state.

For after/steady's 589 commits, all 11 key arguments below were present each
time. Prefix `relais:load:matched:` is 20 B; IDs are 16 B. Each raw name gets
7 B RESP bulk framing. Numkeys adds 8 B once: **556 + 77 + 8 = 641 B/commit**,
377,549 B across those attempts. This is included in wire totals.

| Argument | Key (T = {sess:16-byte-ID}) | Raw B | RESP B |
| --- | --- | --- | --- |
| 01 | lease:T | 49 | 56 |
| 02 | record:T | 50 | 57 |
| 03 | epoch:T | 49 | 56 |
| 04 | routes:T | 50 | 57 |
| 05 | ended:T | 49 | 56 |
| 06 | setup:T | 49 | 56 |
| 07 | checkpoint:T | 54 | 61 |
| 08 | work:T:0 | 50 | 57 |
| 09 | work:T:1 | 50 | 57 |
| 10 | history:T:0 | 53 | 60 |
| 11 | history:T:1 | 53 | 60 |

The publication HSET has three one-byte field names. After/steady's actual
field bytes and other session HSET fields are below; lease renew fields are
reported separately from publication. Other frame/owner HSET fields remain in
the raw report and wire totals, not in the checkpoint payload cap.

| HSET key/field | Field-name B/write | Value B/write | Observed writes (after/steady phase) |
| --- | --- | --- | --- |
| checkpoint / b | 1 | 665–1,520 | 589 |
| checkpoint / m | 1 | 246 | 589 |
| checkpoint / t | 1 | 8 | 589 |
| lease / state_seq | 9 | 2–3 | 589 |
| lease / worker | 6 | 15 | 60 |
| lease / epoch | 5 | 1 | 60 |
| lease / expires | 7 | 13 | 60 |
| setup / 0 (setup phase) | 1 | 0 or 1,298–1,300 | 4 |
| setup / 1 (setup phase) | 1 | 0 | 2 |
| lease / component_init_epoch (setup) | 20 | 1 | 2 |
| lease / layout (setup) | 6 | 2 | 2 |

Setup also wrote the lease fields above (sequence 1–2 B) and two empty `b/m/t`
placeholders before publication. Actual metadata plus timestamp and names is
`246+8+3 = 257 B`, below the ADR's 384 B allocation including its framing reserve.
Steady after payload publication was 2,303,047 B (38,405.74 B/call-second),
including 635,415 B checkpoint, 144,894 B metadata, 4,712 B timestamps,
1,767 B field names, 1,473,849 B work and 42,410 B history.
Hot publication max was 4,684 B; total publication max 8,900 B in that phase.
Across all custom split phases maxima were hot 4,696 B and total 9,906 B.
These publication sizes omit command RESP and other lease HSETs, which remain
in wire totals; admission additionally reserves 78 B protocol framing.

Rotation's marked 11.412219 s interval contained 1,969 commands and
2,403,779 request+reply B (105,316.02 B/call-second). It published checkpoint
219 times, work 195 times, history 6 times and setup twice; the two setup values
totalled 2,594 B. The largest observed whole-topology second during that step
was 294,801 B (147,400.5 B/call if simply divided by two), above 128 KiB;
the policy is a window-average gate and has no proven per-second burst capacity.
No larger burst allocation is justified by this observation.

### Attempts, script submissions and Save cost

Write attempts below count legacy or v10 commit submissions. Failed commit
attempts occurred only during setup, which also recorded cold `NOSCRIPT`
fallbacks; there were no failed media-window commit attempts. The observer
does not separately classify each failed commit. `NOSCRIPT` totals include
other scripts.
`SCRIPT LOAD` command counts were zero in every step: fallback used EVAL.
Script-body submissions count EVAL/SCRIPT LOAD requests, including repeated
warm EVAL bodies; they are not counts of distinct server-side compilations.

| Arm/scenario | Commit attempts | Failed attempts | Script-body submissions | NOSCRIPT replies | SCRIPT LOAD |
| --- | --- | --- | --- | --- | --- |
| before/steady | 626 | 0 | 744 | 5 | 0 |
| after/steady | 627 | 2 | 744 | 7 | 0 |
| before/echo | 645 | 0 | 758 | 5 | 0 |
| after/echo | 647 | 2 | 761 | 8 | 0 |
| after/history-churn | 633 | 1 | 749 | 7 | 0 |
| after/rotation | 1307 | 2 | 1542 | 8 | 0 |
| after/n-plus-one | 630 | 2 | 760 | 8 | 0 |
| after/drain | 617 | 1 | 745 | 6 | 0 |

Application Save time includes the app's own work and excludes host admission
and host envelope serialization. CPU is Linux thread CPU; values below are
means, not tail latency. Save CPU/wall budget was
1,000 µs/event; all passed with no unavailable CPU samples. Admission is measured
separately on the production path, including refused callbacks. Checkpoint skip
reasons were `interval` and `budget`; no budget skips occurred. Refused app
transactions are counted separately from skipped checkpoints.
Echo has no custom-agent Save/admission timing block and is omitted here.

| Arm/scenario | Events / Saves | Save CPU / wall µs/event | Save CPU / wall ms/call-s | Skips interval / budget | Forced success / fail; max ms |
| --- | --- | --- | --- | --- | --- |
| before/steady | 3271 / 3271 | 58.02 / 58.14 | 3.162 / 3.169 | 3600 / 0 | 10 / 0; 4.602 |
| after/steady | 3270 / 3270 | 25.67 / 25.09 | 1.399 / 1.367 | 3610 / 0 | 10 / 0; 5.897 |
| after/history-churn | 3272 / 3272 | 21.77 / 20.63 | 1.187 / 1.125 | 3605 / 0 | 10 / 0; 5.738 |
| after/rotation | 7163 / 7163 | 22.51 / 21.09 | 1.240 / 1.162 | 7935 / 0 | 24 / 0; 7.987 |
| after/n-plus-one | 3161 / 3161 | 21.99 / 21.09 | 1.158 / 1.111 | 3465 / 0 | 8 / 0; 4.191 |
| after/drain | 3272 / 3272 | 22.44 / 21.66 | 1.224 / 1.181 | 3626 / 0 | 10 / 0; 5.226 |

| Arm/scenario | Admission calls | CPU / wall µs/admission | CPU / wall ms/call-s |
| --- | --- | --- | --- |
| before/steady | 3271 | 227.51 / 233.39 | 12.401 / 12.722 |
| after/steady | 3270 | 384.74 / 396.18 | 20.964 / 21.588 |
| after/history-churn | 3273 | 352.85 / 365.11 | 19.246 / 19.915 |
| after/rotation | 7163 | 351.80 / 363.87 | 19.383 / 20.048 |
| after/n-plus-one | 3160 | 343.04 / 362.15 | 18.064 / 19.070 |
| after/drain | 3272 | 348.76 / 361.12 | 19.015 / 19.689 |

After/steady used **384.74 µs CPU/admission and 20.964 ms CPU/call-second**,
versus before/steady's **227.51 µs and 12.401 ms**. After admission CPU is about
**15×** application Save CPU (1.399 ms/call-second) and **7×** the 3 ms/s/call
estimate discussed in #196. Save alone understates host cost; admission needs
its own performance follow-up. These are means from the separately instrumented
production admission path, not the audio/event latency tail.

### ADR 0012 §2 and §6 comparison

[ADR 0012](../adr/0012-agent-state-store-schema.md#2-defaults-sdk-shape-and-traffic)
calls its traffic arithmetic projections, not measured traffic. It states no
universal percentage error margin. The margins below are its explicit byte
headrooms or the difference to an explicit cap; “not isolated” and “not run” do
not validate a projection. Full-limit arithmetic (3,072/8,192 B) must not be
presented as measured by this 2,871/4,205 B fixture.

| Projection / cap | ADR value and margin | Measured value / comparison | Holds for this run? |
| --- | --- | --- | --- |
| Working / history / total app limits | 3,072 / 8,192 / 11,264 B | 2,871 / 4,205 / 7,076 raw B; spare 201 / 3,987 / 4,188 B | Yes for fixture; full limits not exercised |
| Seven / eight example exchanges | 7,358 / 8,409 B; seven fits 8,192 | Four exchanges 4,205 B; seven/eight not sent | NOT-RUN at seven/eight |
| Encoded host allowance / configurable working ceiling | 2,048 / 65,536 B | Host-only bytes not isolated; 65,536 B opt-in not exercised | NOT-RUN at bounds |
| Hot checkpoint cap / worst-case estimate | 7,168 B / 6,966 B; 202 B headroom | Max published hot 4,696 B; +78 B admission reserve = 4,774 B | Fits; not a worst-case proof |
| Setup cap | 4,096 B | Max ciphertext 1,300 B; spare 2,796 B | Yes for fixture |
| History cap / framing reserve | 8,448 B / 256 B over raw max | 4,241 B = 4,205+6+30; actual overhead 36 B | Yes; full 8,192 B not sent |
| Total transaction cap | 20,480 B | Max publication 9,906 B; +78 B reserve = 9,984 B | Yes for fixture |
| Payload rate / periodic plus forced reservation | 80,000 B/s; 11×7,168 = 78,848, spare 1,152 | Ordinary steady publication 38,405.74; full churn 74,849.1 B/call-s | Payload bucket admits sustained wire traffic above the gate; default under review |
| Payload burst | 40,960 B = two 20,480 B transactions | Max publication 9,906 B; largest rotation second 294,801 wire B/topology | Transactions fit; no per-second capacity proof |
| Cadence / component change frequency | 100 ms; 10 commits/s; history once/5 s | Steady 9.8222 commits/s, work 8.4548/s, history 0.1668/s; input ≈49/s | Consistent with cadence; actual rates used |
| Legacy baseline / optimistic setup removal | 3,474 / 2,072 B; 2,048 B host allowance | Legacy echo cipher 3,328–3,570 B; setup cipher 1,296–1,300 B | Different encoding/workload; removal not isolated |
| AEAD / old admission reserve | 30 / 42 B | 30 B AEAD; app component adds 6 B encoding, total 36 B | AEAD holds; 42 B was a reserve |
| Full history base64 expansion | 8,192 → 10,924 B | 4,205 B raw history sent; legacy inline 7,087 B JSON | Full-size expansion NOT-RUN |
| Reference packing and framing | 303 B packed allowance +3 names +78 framing =384 | m 246 +t 8 +names 3 +78 reserve =335 B; spare 49 B | Yes for observed 15 B owner |
| Five digests / IDs / revisions and other packed widths | 160 / 5 / 40 B; epoch8, version4, banks3, format1, length2, owner64, setup-generation8, time8 | Observer records packed m=246 and t=8; individual m subfields not decoded | Packed bound fits; individual widths not independently measured |
| Old 1 KiB app payload projection | 69,200 B/s | No 1 KiB app case; echo cipher 35,756.74 B/call-s | Unmatched workload, not a validation |
| Old 8 KiB app payload projection | 164,760 B/s | 7,087 B inline fixture: steady legacy cipher 134,182.57 B/call-s | Smaller fixture below projection; full 8 KiB NOT-RUN |
| New transport/host excluding work | 38,640 B/s | Steady b/m/t/names 13,120.52 B/call-s | Below estimate; host allowance not filled |
| New full work at 10/s, full history once/5 s | 71,304.4 B/s | Fixture published payload 38,405.74 B/call-s | Below estimate; full-limit projection NOT-RUN |
| Both app components change once/5 s | 40,904.8 B/s | Working changes frequently; no matched low-churn case | NOT-RUN |
| Both full app fields change every checkpoint | 151,880 B/s >80,000; must refuse | Fixture full-churn publication 74,849.1 B/call-s; one pre-acceptance refusal | Refusal observed, but wire gate fails even at fixture sizes; full-size case NOT-RUN |
| Old base64 history at once/5 s versus 10/s | 2,184.8 versus 109,240 B/s | Legacy inline full blobs, no isolated history wire counter | Not isolated |
| Setup saving and older-run ratios | 20,720 B/s; 41.2% of 50,277 session or 24.4% of 84,904 total | Matched history total saving 48.05%; echo 25.81% | Historical ratios are not matched predictions |
| 11-key commit list (16 B prefix) | 512 raw +77 framing +8 numkeys =597 B | 20 B prefix: 556+77+8=641; subtract 11×4 →597 | Exact after prefix normalization |
| Old six-key list | 321 B | Same-build legacy commit has two keys, 119 B (111 B at 16 B prefix) | Baseline assumption does not hold |
| Clock and reservation lease-only list | 59 B each at 16 B prefix | 63 B with 20 B prefix; lease-only source path in both arms | Shape/length holds; not a separately aggregated runtime counter |
| Net key-list change at 10/s | −2,480 B/s, assumes old six-key lists | At 20 B prefix +5,220 B/s; normalized to 16 B prefix, (597−111)×10 =+4,860; clock/reserve already lease-only | Normalized projection delta 7,340 B/s exceeds 2,888 spare |
| Old / new protocol estimate | 15,237 / 12,757 B/s; further args reserve 800 | Media session residual ≈21,175.51 B/call-s after subtracting published values | About 7,618.51 above 12,757+800; exceeds 2,888 spare |
| Expected-root/checkpoint args | 80 B/commit =800 B/s | Included in wire totals, not separately length-bucketed | Not isolated |
| Frame / owners clients | 31,371 / 3,256 B/call-s; owners reference came from four calls | After/steady at two calls: 31,179.71 / 5,050.11 | Frame below; largely fixed topology owners cost is diluted differently, not a matched format regression |
| Derived payload ceiling / selected rate spare | 82,888 / 80,000 B/s; spare 2,888 | Non-publication wire 57,404.82–57,916.63 implies ceilings 73,155.37–73,667.18 | Projection misses beyond stated spare; use largest residual below |
| Mixed high-working-churn traffic | 119,488.4 B/call-s; spare 11,583.6 to 131,072 | Ordinary fixture 95,854.52; sustained history churn 132,347.7 | Ordinary fixture below; history churn has a different history rate and exceeds the wire gate |
| Command / wire gates (§6(4)) | 100 / 131,072 per call-s | Full churn 73.52 /132,347.7 despite passing whole-window and lifecycle averages | **No: sustained churn fails bytes**; legacy history also fails |
| Checkpoint AND snapshot p99 (§6(4)) | 550 ms each; report maxima | After max p99 119.72 /122.07 ms; max samples 120.23 /122.89 ms | Yes for sampled ages |
| Matched traffic and fields (§6(3)) | Same runner/build/load; report setup, steady, bytes, attempts, scripts, Save | Eight steps; five hashes; fields and limits above | Measured with observer; no TLS/NIC/log measurement |
| Recovery, forced barriers, churn (§6(4)) | No call loss/decryption/cleanup failure; N+1/drain; report forced latency | Two N+1 survivors; 26.04 ms drain; forced max 7.987 ms; one refusal; 0 call/decryption losses | Yes for async policy; strict audio exceptions above |
| Contract failures / exact-history crash proof (§6(1–2)) | Fault injection and exact committed history across recording/wait/playback | This traffic run checks surviving calls and fixture restore, not exact history equality | NOT-RUN by this run; separate evidence required |
| Managed durable Cluster (§6(5)) | Separate approval; failover, redirects, TLS, loaded gates | Standalone loopback Redis only | NOT-RUN |

The ADR's owners estimate of 3,256 B/call-second came from a four-call step;
this run has two calls. Owners traffic is largely a fixed per-topology cost, so
fewer calls inflate its per-call rate. That is not a matched format regression.
The **session protocol residual alone** is still about 7,618.51 B/call-second
above the ADR's `12,757+800` estimate, beyond its 2,888 B/s margin.

The ADR's protocol saving assumed old six-key lists. The current same-build
before arm already uses two commit keys and lease-only clock/reservation keys;
there is no further clock/reservation key-list saving to realize. This is an
inapplicable historical projection, not a measured default-cap violation.
Using exactly the observer's ordinary after media window (steady plus move),
wire is 95,854.01 B/call-second and published values are 38,449.19, leaving
**57,404.82 B/call-second** for all other protocol/clients. This conservative
residual includes RESP, commands, metadata outside publication and framing
that admission may already reserve; it is not an isolated script-overhead
measurement. It exceeds the ADR's `31,371+3,256+12,757+800 = 48,184`
non-payload estimate by **9,220.82 B/s**, beyond its **2,888 B/s** spare.
The after media-window residuals (including marked faults/moves, excluding
setup/hangup/cleanup) span:

| After scenario | Non-publication request+reply B/call-second |
| --- | --- |
| Steady | 57,404.82 |
| Drain | 57,535.22 |
| History churn | 57,571.13 |
| N+1 | **57,916.63** |

Use the largest measured residual for a provisional allocation. An 80,000 B/s
payload plus 57,916.63 would total **137,916.63 B/s**, exceeding the unchanged
131,072 gate by **6,844.63 B/s**. Beyond that extrapolation, the full-churn bins
already demonstrate an actual failure at 132,347.7 B/call-second while payload
is below the current default.

**Candidate allocation for a separate review/run, not a default change:**
70,000 payload B/s and 6,144 hot B/checkpoint, retaining 40,960 B burst and the
existing app/setup/history/transaction limits. The largest measured residual
leaves `131,072−57,916.63−2,888 = 70,267.37 B/s`; rounding down to 70,000 leaves
**3,155.37 B/s** gate headroom. The paired hot cap is necessary because
`11×7,168 =78,848 >70,000`; `11×6,144 =67,584` leaves 2,416 B/s for cold writes.
A full framed history once/5 s costs at most `8,448/5 =1,689.6 B/s`, leaving
726.4 B/s under that reserved pattern. The observed largest hot publication
plus framing reserve was `4,696+78 =4,774 B`, below 6,144.

Measured checkpoint ciphertext is substantially smaller than the ADR's 1,402 B
transport estimate: echo 339–387 B; the history fixture 662–1,532 B including
host metadata. A sizing estimate with full **3,072 B working state and 2,048 B
host allowance is about 5,908–6,183 B**, close to 6,144 on either side. This is
an estimate, not an isolated host-size or worst-case codec measurement. The
ADR's old 6,966 B projection also exceeds the proposed smaller cap. Adoption of
6,144 remains conditional on the full-size/worst-case codec proof and a hosted
rerun. The other lever is reducing protocol overhead: the session client spends
about **2.15 KB of non-payload protocol per commit** (about 21,176 B/call-second
at 9.82 commits/s). Any such saving needs measurement; neither policy ceilings
nor the about-200-calls-per-primary planning limit increases with byte savings.

This correction adds a separate sustained-churn observer phase and applies the
same 100-command/128-KiB gates to it in both the load verdict and report. Missing
or truncated phase evidence blocks the report. The hosted job must be rerun;
with current defaults the churn step is expected to fail. No default changed.
A suitable workload-specific audio check is still needed before strict audio
continuity claims; this run does not close the other crash/managed-store gates.


## Final hosted Linux run 38147888686

[Run 38147888686](https://github.com/relaishq/relais/actions/runs/38147888686),
commit `b7e221b`, used the same settings as run 38142256615 below. The relay
buffer was on (the product default). This run includes the final verdict rules:
caller stalls, later-track takeover recovery, crashes with invalid generators,
and ambiguous gaps that no longer hide independent failures.

| Trial | Call counts | Usable result |
| --- | --- | --- |
| Concentrated worker, move all | 2, 4, 8 | Pass; worker floor 8 calls |
| Concentrated worker at 16 | 16 | Inconclusive: generator saturated |
| Relay, spread placement and one move | 2, 4, 8 | Pass; relay floor 8 calls and 1,370.7 forwarded datagrams/s |
| Relay at 16 | 16 | Inconclusive: generator saturated |
| N+1 kill, then planned move | 8 | Pass; validated T = 8 |
| Loaded drain | 8 | Pass; 44 ms drain |

Store use stayed at about 67 client commands and 85 KB of Redis request plus
reply per call-second in every step, within the 100-command and 128 KiB
budget. Cost is `NOT-RUN(no input prices)`. As with the earlier runs, these are
floors set by the shared runner's load generator, not capacity limits. The
relay packets-per-second limit remains **NOT-RUN**.

## Earlier hosted Linux run 38142256615

[Run 38142256615](https://github.com/relaishq/relais/actions/runs/38142256615),
commit `be8901a`, passed the recorded acceptance policy with the **product-default
relay buffer on** (`relay_buffer_enabled=true`). Linux/amd64, Go 1.26.9,
a shared four-vCPU runner, namespace LAN, audio plus the harness VP8 fixture,
30 seconds per step and two callers per generator process with GOMAXPROCS=1.
Worker ramps used two workers; relay and fault samples used three. Evidence is
in the run's `capacity-linux-38142256615` artifact. Service-exit, takeover-count,
whole-call gap, shared-window weighting, store budget, drain-empty and
survivor-peak checks were enabled. The three subsequent verdict refinements
for caller stalls, later-track takeover recovery and crashes with invalid
generators have not yet been exercised in a hosted run.

| Trial | Call counts | Usable result |
| --- | --- | --- |
| Concentrated worker, move all | 2, 4, 8 | Pass; worker floor 8 calls |
| Concentrated worker at 16 | 16 | Inconclusive: generator interval p99/skipped-frame limits |
| Relay, spread placement and one move | 2, 4 | Pass; relay floor 4 calls and 684 forwarded datagrams/s |
| Relay at 8 | 8 | Inconclusive: peak one-second-interval send-lateness p99 20.68 ms exceeds 20 ms |
| N+1 kill, then planned move | 4 | Pass; two killed calls recovered, one subsequent move |
| Loaded drain | 4 total, 2 drained | Pass; 27.25 ms API interval, worker confirmed empty |

The generator process that limited the relay's eight-call step had a **whole-step
send-lateness p99 of 5.3 ms**. The acceptance cause uses the **peak one-second
interval p99**, not that whole-step quantile. No service capacity or failure
claim follows from the inconclusive step.

For C=8, R=4 and W=3, `T = min((W-1)*C - 1, R) = 4` was validated.
N+1 survivors peaked at **3 of 8 calls (37.5%)** and two calls; drain survivors
peaked at two each. This validates recovery at a demonstrated floor, not
survivors near capacity. The relay packets-per-second limit remains **NOT-RUN**;
684 datagrams/s is a demonstrated lower bound. Cost is
`NOT-RUN(no input prices)`.

| Trial | Calls | Client commands/call-second | Redis request + reply B/call-second | Total CPU seconds/call-second |
| --- | ---: | ---: | ---: | ---: |
| Worker | 8 | 67.03 | 85,282 | 0.033171 |
| Relay | 4 | 66.86 | 84,904 | 0.044120 |
| N+1 | 4 | 65.69 | 84,592 | 0.041693 |
| Drain | 4 | 66.78 | 85,148 | 0.041180 |

The passing relay step's Redis request + reply split is **50,277 B session
store**, **31,371 B frame cache**, and **3,256 B owners/routes per call-second**.
These sum to 84,904 B/call-second and fit the 100 commands/128 KiB budgets.
Buffer state comes from the running relay gauge, not from an inferred replay.

### Run-to-run variance

At identical workload settings, runs 38136419832 and 38147888686 demonstrated a
relay floor of **8**, while run 38142256615 demonstrated **4**. Both demonstrated worker
floor **8**. The product and instrumentation changed between commits, and the
shared runner's generator interval peaks constrain these floors. This is
variance in usable evidence, not proof that relay capacity halved. Track trends
with the **worker floor and per-call store traffic and CPU**, comparing rates
at matched call counts, workload and layout. Do not use T as the trend measure:
it also changes with
the relay's generator-limited floor and spare-worker formula. Neither run
supports independent-machine or sustained-load sizing.

## Earlier hosted Linux run 38136419832

[Run 38136419832](https://github.com/relaishq/relais/actions/runs/38136419832),
commit `f132ffb`, passed its recorded acceptance policy. Linux/amd64,
Go 1.26.9, shared four-vCPU AMD EPYC 9V74 runner. The workload was audio plus
the harness VP8 fixture, 30 seconds per step, two callers per generator process,
GOMAXPROCS=1. Topology: Linux namespaces with an unshaped LAN, one relay,
control and Redis. Worker ramps used two workers; relay and fault samples used
three. Reports and process logs are in the run's `capacity-linux-38136419832`
artifact.

The relay buffer was **on**, through the former capacity-only override
`-caller-buffer=true`; positive buffer packet/byte gauges confirm it. The load driver now passes no
buffer override and records the running buffer configuration explicitly.
This historical run therefore does not measure the pre-#35 buffer-off product
default. The newer guards and product default were exercised in run
38142256615 above; this earlier artifact remains evidence under its older
acceptance policy.

| Trial | Call counts | Usable result |
| --- | --- | --- |
| Concentrated worker, move all | 2, 4, 8 | Pass; worker lower bound 8 calls |
| Relay, spread placement and one move | 2, 4, 8 | Pass; relay lower bound 8 calls and 1371 packets/s |
| Both ramps at 16 | 16 | Inconclusive: generator saturation |
| N+1 kill, then planned move | 8 | Recorded pass; 3 killed calls recovered, one subsequent move |
| Loaded drain | 8 total, 3 drained | Recorded pass; 43 ms including the old status/result-recording overhead |

**Eight was the highest usable step in this earlier sample**. It was a
generator-constrained lower bound on this shared runner. Sixteen exceeded
generator interval p99/skipped-frame limits and supplies no service-capacity claim. These are
short samples, not sustained-load or real-camera limits.

For C=8, R=8 and W=3, the sample load was
`T = min((W-1)*C - 1, R) = 8`. Eight calls over three workers is a **floor**,
not an operating recommendation. Survivors peaked at **5 of 8 calls (62.5%)**
in the N+1 sample; the other survivor peaked at four. This proves failover
mechanics at that load, not survivors near capacity. Drain survivors peaked
at four each. The **relay packets-per-second limit is NOT-RUN**; only a lower
bound of **1371 forwarded datagrams/s** was measured.

## Earlier run: store and CPU observations

The kill-step report originally overstated counter and CPU rates by dividing
the retired worker's five-second contribution by its own window. The table
below recomputes raw first/last deltas using the common **30-second × call
count** denominator for every process. It does not rerun the trial. The other
steps change only slightly because their observed windows were almost 30 s.

| Trial | Calls | Client commands/call-second | Redis request + reply B/call-second | Total CPU seconds/call-second |
| --- | ---: | ---: | ---: | ---: |
| Worker | 8 | 66.84 | 84,759 | 0.022055 |
| Relay | 8 | 66.99 | 84,929 | 0.024675 |
| N+1 | 8 | **65.94** | **84,524** | **0.023588** |
| Drain | 8 | 67.01 | 84,886 | 0.024237 |

The obsolete N+1 values were 85.69 commands and 110,902 B/call-second.
Complete client command counts include encrypted session storage, owners and
routes, and frame cache. Lua internal commands and the driver's INFO connection
are excluded. The historical byte counters have no per-client split; that
split is **NOT-RUN for this artifact**. New reports label each client.

These observations fit the objective-derived **100 commands and 128 KiB**
window-average budget, with about 33% command margin and 35% byte margin.
The throughput assumptions and binary-unit rounding are stated in
[capacity.md](capacity.md). Actual Redis sizing for 500 calls is **NOT-RUN**.

## Earlier run: memory and marginal slope

Memory/call is **total sampled RSS divided by concurrent calls**. It includes
fixed service and idle-worker overhead. Go services report time-averaged
lifetime RSS peaks, including setup; Redis reports time-averaged current RSS.
The mixture is labelled in raw process observations. These figures are not
live-heap size or a marginal-memory guarantee.

| Layout | Calls | Total sampled RSS MiB | RSS MiB/call |
| --- | ---: | ---: | ---: |
| Worker ramp, two workers | 2 | 119.96 | 59.98 |
| Worker ramp, two workers | 4 | 125.19 | 31.30 |
| Worker ramp, two workers | 8 | 134.14 | 16.77 |
| Relay ramp, three workers | 2 | 213.75 | 106.87 |
| Relay ramp, three workers | 4 | 234.02 | 58.50 |
| Relay ramp, three workers | 8 | 234.49 | 29.31 |

Empirical total-RSS slopes for the worker layout are **2.62 MiB/additional
call** from 2→4 and **2.24 MiB/call** from 4→8 (2.36 MiB/call over 2→8).
Relay-layout slopes are **10.14** and **0.12 MiB/call**, respectively. Their
instability prevents a reliable relay marginal-memory estimate. Short runs,
setup peaks and allocator history limit both slopes; they are descriptive
secants, not independent-host sizing rules. Saturated 16-call steps are
excluded from this comparison.

## Cost and remaining evidence

**NOT-RUN — monetary cost:** no machine or transfer prices were supplied.
At the latest T=4 (earlier T=8), any input-priced allocated cost/call-hour is an
**upper-bound estimate** at a demonstrated floor, assuming unchanged workload and prices. It includes
all three workers plus relay/control/store, including spare capacity.
It is not an operating recommendation. Cost validation now requires a passing
relay step and a validated planned sample load, and reports missing prerequisites
explicitly.

Product-default buffer-on hosted acceptance is recorded above. **NOT-RUN —
relay packet rate limit, sustained capacity, region/far-caller guarantees,
60-second consent and independent-machine sizing.**

## Latest local Mac smoke (buffer-on)

The single 2026-10-11 darwin/arm64, Go 1.26.9 loopback smoke used two calls,
two callers per generator process, and four 20-second steps. All verification
checks finished before the smoke. The relay reported **buffer-on** in every
step, and every service resource window was complete.

| Trial | Verdict | Commands/call-second | Redis B/call-second |
| --- | --- | ---: | ---: |
| Worker move-all | Pass | 67.87 | 86,429 |
| Relay move | Pass | 67.55 | 85,357 |
| N+1 kill/move | **Inconclusive: generator saturation** | 66.65 | 85,317 |
| Loaded drain | Pass, 4.35 ms API interval; worker confirmed empty | 68.17 | 86,034 |

The N+1 generator's peak one-second-interval send-lateness p99 was **22.4 ms**
and skipped frames reached **1.25%**, exceeding the unchanged 20 ms/1% limits.
Its whole-step send-lateness p99 was 2.4 ms. The command exited 1, the outcome
was inconclusive, and T=2 remained unvalidated. This is a generator limitation,
not a service failure. No retry, buffer override or threshold change was used.
Cost printed `NOT-RUN(no input prices)`. All **27 owned process PIDs were gone**,
and the four per-step reports matched the top-level report.

VERIFIED — gofmt/diff, native and Linux vet, lint v2.14.0, build, all-package
memory race coverage, and Redis-backed package race coverage using an owned
non-6379 Redis that was reaped. The initial ten-minute all-package run timed
out only in the full call harness; that package passed with the repository's
30-minute full-harness allowance. Both shared-package CI regressions and all
three capacity verdict regressions were reproduced red before their fixes.

PARTIAL — this single local smoke did not validate N+1 load. It does not
replace the passing hosted T=4 evidence or establish namespace/host sizing.

## Earlier local Mac evidence (pre-#35)

Earlier two-call, 20-second loopback trials passed with the former forced-on
relay buffer. They are separate local checks and do not establish hosted
capacity or namespace behavior. The pre-#35 product-default smoke is recorded
separately below; it does not replace the hosted trials.

The single pre-#35 default smoke ran on 2026-10-11, darwin/arm64, Go 1.26.9,
with two calls and four 20-second steps. This run's validation checks finished before the smoke.
All four generators were valid and every service resource window was complete.
The running relay reported **buffer-off**.

| Trial | Verdict | Commands/call-second | Redis B/call-second |
| --- | --- | ---: | ---: |
| Worker move-all | Pass | 67.97 | 86,543 |
| Relay move | Pass | 67.75 | 85,911 |
| N+1 kill/move | **Fail: audio and video loss** | 66.27 | 85,872 |
| Loaded drain | Pass, 8.88 ms API interval; worker confirmed empty | 68.27 | 86,420 |

The command exited 1 and the planned load remained **unvalidated**. This is
the expected pre-#35 buffer-off recovery result; no buffer override or threshold
change was used to turn it into a pass. Cost printed `NOT-RUN(no input prices)`.
Per-step and top-level reports agree, and all 27 owned process PIDs were absent
after cleanup. The relay step's Redis split was **50,169 session-store**,
**4,418 owners/routes**, and **31,323 frame-cache B/call-second**, summing to the
reported total. This local observation is not Linux namespace evidence.

VERIFIED — format/diff, vet, Linux vet, lint v2.14.0, build and changed-package
race tests on memory and owned non-6379 Redis. Regression evidence includes
red→green unexpected-exit/takeover/gap guards and shared-window weighting;
a real child exit retains its error. Partial drain, empty-worker confirmation,
report agreement, cost prerequisites, runtime buffer state, client labels,
default TLS and dial timeout are tested. The test Redis was reaped.

Post-#35 product-default buffer-on hosted evidence is recorded above. This
pre-#35 local smoke made no passing N+1 operating-load claim.
