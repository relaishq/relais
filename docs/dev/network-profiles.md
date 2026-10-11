# Network profile acceptance (#37)

Build first as the normal user. Run on native Linux with root and iproute2,
iptables (including the u32 match), Redis and ffmpeg. The driver owns a
throwaway, password-protected Redis at the store namespace's port 16379.
It refuses external Redis for netns and never uses port 6379.

```sh
make build
sudo -E env RELAIS_REDIS_ADDR= ./bin/crash-run -topology=netns -profile=region -scenario=move -runs=20 -after=60s
sudo -E env RELAIS_REDIS_ADDR= ./bin/crash-run -topology=netns -profile=far-caller -scenario=takeover -runs=10 -after=60s
```

Profiles are `lan`, `region`, `far-caller`, and `lossy-dc`. Scenarios are
`move` (the existing operator move API) and `takeover` (owning-worker SIGKILL).
Both enable the relay buffer. The original unshaped LAN cache/PLI comparison,
SIGTERM drain, buffer comparison and relay continuity modes remain available.
A shaped run must select a scenario; loopback shaping is refused.

## Path settings

Netem controls egress on an interface. See the
[iproute2 netem manual](https://github.com/iproute2/iproute2/blob/main/man/man8/tc-netem.8).
Each endpoint's owned veth has two directions: peer egress in the role
namespace, and host-link egress in the fabric namespace. A datacentre round
trip crosses four qdiscs, so each gets one quarter of the desired RTT.
The caller impairment lives only on the caller veth: half the RTT on each
end. The relay's public veth stays unshaped to avoid doubling the RTT.
Management veths and host-root interfaces are never shaped.

| Profile | Caller veth, each direction | DC veth, each direction |
| --- | --- | --- |
| lan | Unshaped | Unshaped |
| region | 10ms delay, 0.5% loss | 125us delay |
| far-caller | 75ms delay, ±10ms jitter, 2% loss | 125us delay |
| lossy-dc | Unshaped | 375us delay, ±125us jitter, 0.05% loss, 0.1% reorder |

Loss percentages for the caller apply in each direction. A DC one-way path
crosses two 0.05% loss stages, giving about 0.1% loss. Its RTT is 1.5ms with
up to 0.5ms total jitter. Far caller RTT is 150ms with up to 20ms jitter;
the jitter can itself reorder packets. Explicit DC reorder has a delay, as
required by netem. The queue limit is 10,000 packets per shaped interface.
All other netem options use kernel defaults and are visible in qdisc output.

The path covers caller↔relay, relay↔workers, workers↔store, and
control↔workers. Other private exchanges get the same DC endpoint settings.
A run records every endpoint's requested settings and actual `tc qdisc show`
in `network-settings.json` and `profile-report.json`. Closing the topology
removes its interfaces, attached qdiscs and namespaces, including partial
setup and canceled runs. Selecting LAN after another profile removes netem.

## Report and judgment

Each trial runs one continuous audio/video call. It takes the baseline from
the last three seconds of that same call before the acceptance event, under
the same profile and load (one call). The baseline uses received packet
intervals. Crash-run computes event gaps from retained arrivals over
`[Start, End + return bound + 20ms margin]`. The return bound is the maximum
caller-link delay plus jitter in the applied plan, cross-checked against half
the trial's baseline content age. If that estimate is larger, it extends the
window further. The first arrival after this window closes is included, so a
gap crossing its end is counted. This catches held packets that reach a far
caller after the controller event ends. Missing arrival coverage or a baseline
that disagrees with the link envelope makes the evidence inconclusive. #32's
caller yardstick and other callers keep their existing windows.
The run's baseline p95 pools these intervals across trials. It never compares
against a separate LAN run or the old 416ms phase-1 median.

Moves judge audio and video independently. Their nearest-rank p95 must be
at most the pooled baseline p95 + 50ms and at most **150ms**, inclusive.
Takeovers require each track's pause strictly below **2s**. When every media
link is lossless (currently `lan`), they require trusted **zero lost content**
in #32's event window. Loss on any media link (currently `region`, `far-caller`
and `lossy-dc`) requires **no loss beyond the network's own**, using the
same-run comparison below. The applied link plan and recorded qdisc select
the rule: verified positive loss on a caller, relay or worker link uses the
baseline comparison. Loss
confined to control or store links does not change the content-loss rule.
Every impairment, including loss-only, installs netem. Missing or mismatched
recorded qdisc loss makes the policy inconclusive. The raw loss remains
visible; the report does not relabel it as zero.
Both require zero decryption failures, continuous connection, one offer/answer,
no reconnect or renegotiation, and the 60-second consent observation.
Whole-call ffmpeg decode findings remain explicit diagnostics because these
profiles deliberately lose packets. Invalid payload identities still fail,
and unavailable decoding makes the row inconclusive. Takeover content loss
still gates independently through the caller event window and its profile policy.

Moves require **20 trials** for every profile. Nearest-rank p95 selects
the 19th sorted value at 20 trials, so a single timing outlier does not become
the p95. At ten trials, p95 selects the maximum. The move thresholds stay
at baseline p95 + 50ms and the absolute 150ms ceiling. This sample count does
not guarantee a precise population p95; all trials remain visible, and known
integrity or connection failures in any trial still fail the run. Every move
trial must also stay strictly below 2s, even when p95 excludes it.

Takeovers retain **ten trials**. Their pause ceiling checks every trial,
not the displayed p95, so one pause at or above 2s still fails acceptance.
Each track needs at least ten baseline intervals.
A gap baseline p95-minus-median above 50ms makes the row inconclusive. This
budget includes normal intra-frame packet bursts and video cadence. The
profile content-age baseline needs ten samples and a p95-minus-median spread
of at most **20ms + applied caller round-trip jitter**. The jitter is summed
from the caller→relay and relay→caller endpoints in the applied link plan.
Private-link jitter does not increase this limit. Thus far caller allows 40ms;
LAN, region and lossy DC retain 20ms. The caller policy explicitly records content-age median ceilings:
LAN/lossy DC 50ms, region 150ms, far caller 250ms. These are **diagnostic**,
not acceptance gates; they do not replace the **150ms move-gap ceiling**. #32's
per-metric trust, loss and freshness reports are retained unchanged. Each row
records `AudioSpreadMargin` and `VideoSpreadMargin`: the profile spread limit
minus that track's content-age p95-minus-median. The run records
`ContentAgeSpreadLimit`, also printed in `PROFILE_POLICY`.

Known failures take precedence over unknown metrics. Rows carry conclusive, fail or inconclusive states and reasons; move p95
judgment belongs to the full run, not each individual trial. The run reports `pass`, `fail`, or `inconclusive`; both
fail and inconclusive exit nonzero. A short developer run cannot pass the
60-second consent gate. Too few trials, noisy baselines, untrusted loss,
unavailable decoding or missing fault evidence are never silently passed.

### Content loss on lossy media links

Audio has no retransmission, so losing a packet on any media link can lose an
audio unit even without a takeover. Video counts identified frames that never
returned complete, independent of decoding. Relay totals and netem drop
counters mix packets, media kinds, retransmissions and control traffic; they
cannot cheaply attribute these content units to a direction and event window.
This policy uses a statistical comparison, not exact per-unit attribution.

The caller retains `Measurement.ContentLoss.Event` and up to three
`Measurement.ContentLoss.Baselines` per row. Each baseline window has exactly
the event window's length and comes from the same call, profile and load.
Baseline windows do not overlap. The newest ends 500ms before the event
window begins; earlier events and their settling periods, startup, and expired
history are excluded. Counts include only identified successful writes.
Audio counts units, including a short final Opus unit, rather than dividing
lost duration by 20ms. Video counts sent frames, not packets. Duplicates and
reordering count once. Missing content uses the same observation horizon as
#32's raw loss; late complete returns are not lost content.

Pool all acceptance event windows and their baseline windows separately for audio
and video. Each call must supply a baseline. Each track needs at least 100
baseline sent units, at least as many baseline sent units as event sent units,
and at least ten baseline windows. Missing or untrusted counts, insufficient
coverage, or an untrusted caller loss verdict make that comparison
**inconclusive**. A pooled baseline above **10%** content loss is inconclusive
("baseline too lossy to judge"). A 100%-loss baseline cannot pass. Expired event
history stays inconclusive even when the earlier raw verdict set `Failed`.
Definite lost calls, terminal takeover loss, and takeover timeouts fail.
A single noisy trial does not invalidate a healthy pooled baseline. The pooled
comparison remains diagnostic for moves. Other known failures still win.

For media-link loss, calculate the measured baseline rate `baseline lost /
baseline sent` and the expected event loss `event sent × baseline rate`.
Use a one-sided [exact Clopper-Pearson upper rate](https://www.itl.nist.gov/div898/software/dataplot/refman1/auxillar/propconf.htm),
then a binomial predictive upper count for the event denominator. Each tail
uses alpha **0.0125** (98.75%): two tails per track, four in total, share a 5%
error budget. The combined one-sided confidence is at least **95%** under
independent, stationary Bernoulli content loss. Baseline uncertainty is
included; even a zero-loss baseline has a positive upper rate. This is a
sampling bound, not a guarantee that every small additional loss is detectable.

Event loss **above** the upper count fails; loss **at or below** it passes the
comparison. The run still needs every other takeover criterion to pass. Raw
per-row sent/lost counts and baseline windows stay in the measurement.
`AudioContentLoss` and `VideoContentLoss` in each row and summary report the
pooled event and baseline counts, baseline rate and upper rate, expected
count, upper count, confidence, method, verdict and reasons. Their scope is
explicitly `pooled run`; rows do not run separate statistical tests.
`BoundAvailable` distinguishes an unavailable bound from a zero bound, and
`GatesAcceptance` distinguishes the takeover gate from move diagnostics. Thus
a lossless media path proves exact zero-loss takeover, while a lossy media
path proves no loss beyond the network's own at the stated confidence and
observed sample size. This includes relay-to-worker DC loss.

Pooling also has a **per-trial burst guard** for takeovers, separately for
audio and video. Count consecutive lost identified successful writes in send
order. A valid return by the observation horizon breaks the run. Fail when the
longest event run is at least `max(4, longest baseline run anywhere in this
run + 1)`. This catches a concentrated takeover loss that fits the pooled
count bound. Scattered losses still use the pooled comparison. Missing burst
evidence, including old reports without `RunTrusted`, is inconclusive.

Moves retain their per-track gap, integrity and connection gates;
they do not currently gate on raw content loss. They record the same raw
windows and comparison, so media-link loss cannot introduce a separate
zero-loss requirement for a short move window.

### Lossy DC fault evidence

Small random loss alone will seldom drop one of the few barrier packets, and TCP
can hide reply loss with retransmission. Each lossy trial therefore includes
an explicit uncertainty probe before its acceptance event. A driver-owned
HTTP proxy lets a real worker resume commit, then substitutes one malformed
successful reply. The real private API client classifies that as uncertain;
the real controller settles or rolls back. The probe's caller event, including
its operator error and measurement, stays in `probe_events` and the full
caller report. The acceptance baseline starts after a six-second recovery
window. Whole-call decryption and connection checks also cover the probe.
This tests uncertainty recovery, not a claim that the failed operator request
was a successful planned move.

For a second preliminary planned move in each lossy trial, the driver
briefly drops only private `RLS\x01` drain markers on **owned worker
namespace ingress**, restricted to each private veth and the relay private
source address. After 40ms it removes the rules, allowing the real 75ms resend.
Ingress loss leaves the relay send successful and the receiver silent. An
OUTPUT drop would return `EPERM` to the sender through the
[Linux local output path](https://github.com/torvalds/linux/blob/v6.8/net/ipv4/ip_output.c#L96-L125)
and [netfilter drop verdict](https://github.com/torvalds/linux/blob/v6.8/net/netfilter/core.c#L583-L601).
Media packets are not selected; the original barrier deadline is unchanged.
The relay's new `relais_relay_barrier_resends_total` counter counts actual
resend attempts. Both scenario baselines start after the preliminary move's full
six-second settling window. Forced probes are excluded from profile p95;
their measurements remain in the report, and whole-call integrity covers
them. Each lossy-DC trial must have exactly two earlier **move** probes; every
other profile must have zero. Any earlier crash takeover (`Kind == "takeover"`)
fails. Deferred planned-move outcomes in the status history keep their operator
caller record and do not add an automatic crash event. The barrier-resend
probe requires resumed audio/video and gaps at most **225ms** (150ms + 75ms).
The uncertain-reply probe requires resumed audio/video and gaps strictly below
**2s**; its operator error may represent a safe rollback. Both probe verdicts
and maximum gaps appear in `PROFILE_RESULT`. Random profile loss still
applies during the acceptance event. Both resend and **injected discarded
reply** counts must be positive for lossy-DC acceptance. A missing injected
count is inconclusive. `InjectedDiscardedReplies` counts
proxy injections; `ObservedUncertainMoveResumes` separately counts controller
uncertain target-resume paths through
`relais_control_uncertain_move_resumes_total`. Missing controller samples are
reported as unavailable, rather than zero. Each lossy-DC trial requires a
positive observed counter **delta**, measured from before the probes through
the last successful scrape. Zero, missing samples, or a counter reset leaves
that path NOT-PROVEN and acceptance inconclusive. Neither counter proves a
target commit. The driver proxy is private through the existing management
link; the original DC
source/destination qdiscs still apply to both halves of the HTTP round trip.

### Slow move diagnosis

Each row retains the acceptance event's controller result in `event_result`,
including total `Duration`, barrier `Drain`, `Export`, and `Resume` timings.
Preliminary fault probes remain separate in `probe_events`. The
`PROFILE_RESULT` summary names `slowest_trial`, selected by the largest audio
or video gap, even if p95 excludes that outlier. It prints both gaps and
the event's phase timings. `slowest_other` is total duration minus barrier,
export and resume: transfer, routing, release and coordination overhead
combined. Detailed diagnostics time their private calls separately. Missing phase data is
reported as unavailable.

Per-phase HTTP/Redis traces and TCP counter deltas now distinguish these
costs. The native A/B supports tail-loss-probe delay on small private RPCs;
see the table below and [move latency](move-latency.md). The move thresholds
still apply to DC loss.

## CI and evidence boundary

`netns-network-profiles (PROFILE, SCENARIO)` runs the four profiles × two
scenarios on nightly schedules and manual workflow dispatch only. It is not
a PR or main-push gate. The existing `netns-crash-run` LAN job retains its
isolation and cleanup coverage, excluding `Test(LinuxProfile|Profile)` so
profile-specific test failures cannot block its LAN trials. Platform-independent
profile tests still run in the unit job; native profile tests run in the matrix.
Each matrix lane builds as the ordinary user, then runs namespace tests and
the driver as root. Each trial retains service logs, `caller-report.json` and
`metrics.json`. JSON durations are nanoseconds; the log also prints the thresholds and p95s
with readable units. Profile JSON results upload on every run; failures and
cancellations also upload all logs. Always-run leftover checks detect owned
namespaces or links after teardown. Move jobs use 20 trials, a 35-minute
driver timeout and a 45-minute job timeout. Takeovers retain ten trials,
a 20-minute driver timeout and a 30-minute job timeout.

Every matrix entry fails the workflow on fail or inconclusive, including
lossy-DC moves. Private-route tuning now defaults on, following
[ADR 0007](../adr/0007-private-route-tcp-recovery.md); no acceptance threshold
changed. The manual `private_route_tuning=false` input retains the baseline.

In run 38154313823, the DC trial process observation averaged 78.46s.
Twenty such trials project to about 26 minutes before job setup, so the
move timeouts include trial and setup headroom. Adding ten trials to all
four move lanes projects to about **46 additional runner-minutes** per
matrix run (roughly 11 minutes each for LAN, region and far caller, and
13 minutes for DC loss). This is a projection from the observed DC run
and the other profiles' five-second warmup plus 60-second consent window,
not a hosted measurement of the new configuration. Takeover cost is unchanged.
The **whole nightly matrix** projects to about **155 runner-minutes** and
about **30 minutes wall clock** when all eight lanes run concurrently. This
public repository uses standard GitHub-hosted runners, so the matrix has [**no
monetary runner charge**](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
The runner-time cost still matters.

The orchestrator can dispatch all lanes with:

```sh
gh workflow run ci.yml --repo relaishq/relais --ref investigate/87-move-latency
```

Native Linux tests verify actual region/far-caller RTT, qdisc switching,
root-qdisc preservation, barrier-only loss/removal and cleanup. They are
opt-in using `RELAIS_TEST_NETNS_REQUIRE=1`, consistent with #36. macOS unit
checks and Linux cross-vet do not prove runtime shaping or these thresholds.
Hosted results and posting the first results to #31 belong to the orchestrator.

## Private TCP recovery and diagnostics (#87)

Private DC routes use `rto_min 5ms quickack 1` by default. Disable with
`-private-route-tuning=false` for an untuned baseline. Every profile report
records `private_route_tuning`; enabled reports include observed route metrics.
Host, caller and fabric routes remain unchanged.

| Lossy DC move run (20 trials) | Tuning | Verdict | Caller move duration | Audio / video gap p95 |
| --- | --- | --- | --- | --- |
| [38169245053](https://github.com/relaishq/relais/actions/runs/38169245053) | Off | Fail | 19 at 25–38ms; one at 253ms | 37.6 / 37.5ms |
| [38169250273](https://github.com/relaishq/relais/actions/runs/38169250273) | On | Pass | All at 23–43ms | 43.1 / 47.0ms |

Profile move reports retain per-phase private HTTP/Redis timings, connection
reuse and namespace TCP counters. Diagnostic collection errors are explicit
and non-fatal. See [move latency](move-latency.md) for the TLP evidence and
limits, and [private-route deployment](private-route-tuning.md) for commands.
