# Checkpoint age and bounded takeover margins

Context: a checkpoint is a successful fenced session-store write. The 100 ms
snapshot timer does not bound its age: media continues while copies wait for
storage. Phase 1 uses 8192 RTP indexes and 128 SRTCP indexes to cover 100 ms
between copies, 400 ms of detection and a 50 ms detector tick. Those margins
assume bounded index rates, including caller sequence gaps.

Decision: the store records successful write time atomically with each blob.
Redis uses `TIME`; Memory uses its own clock. Before copying media counters,
the worker samples that same store clock and persists the sample inside the
encrypted version-6 snapshot. Takeover reads metadata bound to those exact
bytes. It exposes **checkpoint age** (since successful write) and **snapshot
age** (since the counter copy). Safety uses snapshot age, so a delayed successful
put cannot disguise old counters as fresh. Reply latency is added conservatively
using local elapsed time. No worker wall clocks are compared. An observed store
clock inversion makes recovery unsafe and causes definitive loss. The store
clock must remain stable between samples: `TIME` cannot detect a backward step
that still leaves the reported time after the saved write time.

`mediaworker.Config.CheckpointEnvelope` and
`controlplane.Config.CheckpointEnvelope` configure the same envelope. Defaults:
maximum age 550 ms, maximum caller RTP packet rate 5,000/s per SSRC, maximum
SRTCP packet rate 100/s, and safety factor 1.25 (minimum 1). RTP sources must
advance sequence numbers once per packet and comply with the configured caller
rate, including packets lost before the worker. As in #7/#8, this is the input
bound of the guarantee. Workers also cap encrypted packet sends with token
buckets before live encryption: bursts of 64 RTP packets and 16 SRTCP packets.
Source sequence gaps raise the observed rate but consume one packet token;
a relay outage must not permanently block echo. Excess send bursts are dropped.
Per-session rate observations retain the highest measured rate from
windows of at least 100 ms and survive moves. Takeover uses the larger of the
recorded rate and configured maximum. Recorded rates never reduce the bound.
The worker and control plane must receive matching rate settings.

For snapshot age `a` seconds, rate bound `r`, safety factor `f`, and burst `b`,
the required margin is `ceil(a * r * f) + b + 1`. The default plain margins
cover at most `(8192 - 65) / 6250 = 1300.32 ms` for RTP and
`(128 - 17) / 125 = 888 ms` for SRTCP. The selected 550 ms age is inside both.
An envelope event occurs if age or observed rate exceeds the configured limit,
or if the required margin exceeds a plain margin. The policy is **scaled**:
use the larger required margin without reducing either plain margin. The #8
caller reserve remains at least 10,000 indexes. It grows to
`max(10000, ceil(2 * r) + 64)` for the two-second outage target; the default
rate therefore reserves 10,064 indexes. Longer recoveries reserve through the
current adoption deadline, using elapsed time on the control plane from the
original outage. A failed target's fresh snapshot cannot reset that outage.
Confirmation keeps the original bytes and margins; an already adopted tenure
acknowledges them without spending counters, while a cold target rechecks the
growing reserve. Retained advances, the first packet,
unsent-stream wrap runway, cache replay reservations and alternate-target retries
still pass the existing sequence-budget guards. The conservative cap for a new
RTP margin is at most 22,766 (`2^15 - 10000 - 2`). The effective cap subtracts
the enlarged caller reserve: 22,702 by default, or 12,702 at 10,000/s. Retained
advances can lower it further. Default rates still permit two 8192-index target
attempts; higher rates can permit fewer.
SRTCP also retains its `2^31 - 1` key-lifetime guard. If the required margin
cannot fit, apply **definitive loss**: release the lease, clear the relay route
and frame cache, forget the call, and count one terminal envelope event. Never
resume that snapshot. A confirmed live export uses zero margins. Recovery of
an uncertain export or adoption must use the latest fenced store checkpoint.

Consequences: long stalls may end a call instead of sending undecryptable media.
The token buckets can drop traffic above the configured send rate. Observed
source rates above the configured bound can reduce the recovery budget or cause
definitive loss. The guarantee requires callers to stay within the source bound.
Planned live-export moves retain phase-1 behavior. The new owner receives the
original checkpoint write time, checkpoint age and snapshot age in ResumeOptions,
also through the private HTTP API, and retains them in its subsequent snapshots
for the relay buffer and agents. `SessionCheckpoint` exposes local age since the
last acknowledged write and the original takeover observations.

Attempts, successes, failures, prior checkpoint age and measured rates are
persisted in each successful encrypted snapshot. Success rate is
`successes / attempts`, as known at that checkpoint, including its successful
write. Failed writes cannot durably update an unavailable store; those later
failures appear after the next successful write, while process metrics count
them immediately. An uncertain write reply is a local failure; the store's
committed blob and write timestamp remain authoritative at takeover.
Metrics count successful/failed writes, terminal envelope policy events and
checkpoint age, without per-session labels. Full export remains #39.
