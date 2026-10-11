# Network profile acceptance (#37)

Build first as the normal user. Run on native Linux with root and iproute2,
iptables (including the u32 match), Redis and ffmpeg. The driver owns a
throwaway, password-protected Redis at the store namespace's port 16379.
It refuses external Redis for netns and never uses port 6379.

```sh
make build
sudo -E env RELAIS_REDIS_ADDR= ./bin/crash-run -topology=netns -profile=region -scenario=move -runs=10 -after=60s
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
intervals, the same units as the caller yardstick's per-track event gap.
The run's baseline p95 pools these intervals across trials. It never compares
against a separate LAN run or the old 416ms phase-1 median.

Moves judge audio and video independently. Their nearest-rank p95 must be
at most the pooled baseline p95 + 50ms and at most **150ms**, inclusive.
Takeovers require each track's pause strictly below **2s**. On `lan` and
`lossy-dc`, whose caller links are lossless, they require trusted **zero lost
content** in #32's event window. On `region` and `far-caller`, they require
**no loss beyond the network's own**, using the same-run comparison below.
The raw loss remains visible; the report does not relabel it as zero.
Both require zero decryption failures, continuous connection, one offer/answer,
no reconnect or renegotiation, and the 60-second consent observation.
Whole-call ffmpeg decode findings remain explicit diagnostics because these
profiles deliberately lose packets. Invalid payload identities still fail,
and unavailable decoding makes the row inconclusive. Takeover content loss
still gates independently through the caller event window and its profile policy.

Ten trials are required. Each track needs at least ten baseline intervals.
A gap baseline p95-minus-median above 50ms makes the row inconclusive. This
budget includes normal intra-frame packet bursts and video cadence. The
independent #32 content-age baseline still needs ten samples and a spread of
at most 20ms. The caller policy explicitly records content-age median ceilings:
LAN/lossy DC 50ms, region 150ms, far caller 250ms. These describe the network
freshness policy; they do not replace the **150ms move-gap ceiling**. #32's
per-metric trust, loss and freshness reports are retained unchanged.

Known failures take precedence over unknown metrics. Rows carry conclusive, fail or inconclusive states and reasons; move p95
judgment belongs to the full run, not each individual trial. The run reports `pass`, `fail`, or `inconclusive`; both
fail and inconclusive exit nonzero. A short developer run cannot pass the
60-second consent gate. Too few trials, noisy baselines, untrusted loss,
unavailable decoding or missing fault evidence are never silently passed.

### Content loss on lossy caller links

Audio has no retransmission, so losing a caller-link packet can lose an audio
unit even without a takeover. Video counts identified frames that never
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

Pool the ten event windows and their baseline windows separately for audio
and video. Each call must supply a baseline. Each track needs at least 100
baseline sent units, at least as many baseline sent units as event sent units,
and at least ten baseline windows. Missing or untrusted counts, insufficient
coverage, or an untrusted caller loss verdict make that comparison
**inconclusive**. Other known failures still win.

For caller-link loss, calculate the measured baseline rate `baseline lost /
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
`lan` and `lossy-dc` prove exact zero-loss takeover, while `region` and
`far-caller` prove no loss beyond the network's own at the stated confidence
and observed sample size.

Moves retain their existing per-track gap, integrity and connection gates;
they do not currently gate on raw content loss. They record the same raw
windows and comparison, so caller-link loss cannot introduce a separate
zero-loss requirement for a short move window.

### Lossy DC fault evidence

Small random loss alone will seldom drop one of ten barrier packets, and TCP
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
them. Random profile loss still applies during the acceptance event. Both resend and uncertain-outcome counts must be
positive for lossy-DC acceptance. A missing count is inconclusive. The driver
proxy is private through the existing management link; the original DC
source/destination qdiscs still apply to both halves of the HTTP round trip.

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
namespaces or links after teardown. Job timeout: 30 minutes; driver: 20 minutes.

The orchestrator can dispatch all lanes with:

```sh
gh workflow run ci.yml --repo relaishq/relais --ref feat/37-network-profiles
```

Native Linux tests verify actual region/far-caller RTT, qdisc switching,
root-qdisc preservation, barrier-only loss/removal and cleanup. They are
opt-in using `RELAIS_TEST_NETNS_REQUIRE=1`, consistent with #36. macOS unit
checks and Linux cross-vet do not prove runtime shaping or these thresholds.
Hosted results and posting the first results to #31 belong to the orchestrator.
