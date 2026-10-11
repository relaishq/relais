# ADR 0007: Tune TCP recovery on low-latency private routes

Relais deployments should set `rto_min 5ms quickack 1` on Linux routes between
relay, control plane, workers and store in low-latency private datacentre
networks. Keep public and caller routes unchanged. This is deployment policy;
the netns harness applies it by default and supports an untuned A/B baseline.

## Evidence and mechanism

| Lossy DC move run (20 trials) | Private tuning | Verdict | Caller audio gap | Audio / video gap p95 |
| --- | --- | --- | --- | --- |
| [38169245053](https://github.com/relaishq/relais/actions/runs/38169245053) | Off | Fail | 19 at 25–38ms; one at 253ms | 37.6 / 37.5ms |
| [38169250273](https://github.com/relaishq/relais/actions/runs/38169250273) | 5ms, quick ACK | Pass | All at 23–43ms | 43.1 / 47.0ms |

Controller paused move durations were 22.4–26.7ms plus one 238.9ms outlier
untuned, and 23.3–25.3ms tuned. These are distinct from caller audio gaps.
The tuned gap p95 was higher (43.1/47.0ms versus 37.6/37.5ms); treat this as
single-run variation within the unchanged thresholds, not a demonstrated
population p95 improvement.

The untuned outlier spent 224.7ms in resume and 2.3ms in export. Store counters
increased by one each for `TCPLossProbes`, `TCPLossProbeRecovery` and
`TcpRetransSegs`; worker-0 also had one loss probe and retransmission.
`TCPTimeouts` remained zero. Resume made four Redis commands and one proxied
HTTP call. The untuned verdict failed a separate barrier-resend probe, rather
than its run p95. Tuning preserves every acceptance threshold and fault probe.

The runs used `6.17.0-1022-azure` (Ubuntu package
`6.17.0-1022.22-azure`, base `6.17.13`). In upstream
[Linux 6.17's loss-probe scheduler](https://github.com/torvalds/linux/blob/v6.17/net/ipv4/tcp_output.c#L2870),
one outstanding segment adds the effective minimum retransmission timeout to
twice the measured round-trip time before a tail-loss probe (TLP), capped by
the remaining retransmission timeout. That floor is normally about 200ms.
[A locked route metric overrides the socket floor](https://github.com/torvalds/linux/blob/v6.17/include/net/tcp.h#L844).
Lowering it addresses small private request/response loss; quick ACK reduces
delayed-ACK waiting. These sources and the A/B support the TLP diagnosis;
namespace counters do not identify a specific lost socket or packet. The
exact Ubuntu vendor source was unavailable, so its patches were not audited.
The A/B changed both metrics and does not isolate their individual effects.

## Tradeoffs

Go's HTTP and Redis transports have no portable per-socket option for this
policy. Application retries or hedges would need durable operation identities
and replay semantics for fencing, export and uncertain resume. That is a
larger change with ownership risks, so retain the current application behavior.

A 5ms floor can cause spurious retransmissions on jittery or congested paths.
Use it only on measured low-latency private networks; check their RTT and jitter
before rollout and keep a saved route configuration for rollback. It does not
reduce initial SYN recovery, tune public traffic, or establish a general WAN
policy. One passing run is evidence, not a reliability bound; repeat the tuned
lane and full matrix before accepting this branch.

See the [operator commands](../dev/private-route-tuning.md) and
[diagnostic limits](../dev/move-latency.md).
