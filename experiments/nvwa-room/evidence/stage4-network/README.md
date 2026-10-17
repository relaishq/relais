# Stage 4: DataChannel versus WebSocket across emulated networks

Status: CI round 2 (`39002753889`) confirmed private Redis authentication and
fast startup failure handling. Workers then rejected the private room-audio bus;
no Linux performance result was produced. The audio-address fix and complete
[address audit](netns-address-audit.md) await the orchestrator's next dispatch.
This spike will not merge to main; the existing WS-for-data decision remains
unchanged.

## Method

`network-driver` reuses the spike's real room, relay, two workers, control plane,
Redis, signal HTTPS service, audio bus, Pion callers, and command ledger. It does
not substitute an echo-only Pion endpoint. Two clients send the existing stage-3
payloads: 1,000 messages each, 30 messages/s each, alternating reliable
`trigger`/`stop` commands and state. This is 15 commands/s plus 15 state updates/s
per client. The state JSON payload is 55 bytes; trigger and stop payloads are
`{"id":"wave","version":1,"start_tick":0}` and `{}`. Serialized envelope sizes
are recorded in each raw sample and summarized by message type. Both clients
send audio throughout, including WS runs.

The old driver awaited each reply before sending the next message. That would
reduce offered load when RTT rises. The new driver schedules sends independently
of replies, does not wait for state receipts, and records scheduling lag. Command
requests can overlap and have a 10 s deadline. All transports use the same count,
cadence, payloads, message limits and application correctness checks. The new
`-network-state-channel` room mode emits a distinct `state-update` to the peer on
the state channel. Reliable command snapshots omit state, so they cannot supply
a hidden reliable state path. Earlier drivers and reports retain their behavior.
This is a controlled state-lane experiment, not a claim that earlier stage-3
latencies are directly comparable.

Each profile runs two paired repeats of each scenario: normal, disconnect,
worker kill, and lost receipt. Every repeat runs WS, reliable DC and unreliable
DC sequentially, with the order reversed on the second repeat. The middle
position rotates by scenario. There are 24 fresh topologies per profile. Sending
lasts about 33.3 s, followed by command drainage and 5 s of observation. Fault
injection follows that steady window. This keeps fault recovery separate from
network tail latency and state-gap statistics. Each process has a 30 s readiness bound, with an immediate failure if any
started child exits; the error includes that first child's last 20 log lines
(up to 8 KiB), with the Redis credential redacted. The whole setup has a 60 s
bound; a run retains its 3 minute cap. Two consecutive startup failures stop the
profile. A 24 minute internal profile budget sends TERM to the current driver
and waits up to 20 s for cleanup before escalation. The comparison step retains
its 28 minute cap and the whole job its 35 minute cap.

## Namespace paths and isolation

Linux uses `internal/nettopology.Open`, its owned veths and bridges, and its exact
existing profiles. The parent stays on the host management link. A separate
caller subprocess contains **both clients and every socket they create**. The
room and Redis use the `store` namespace, signal uses the relay namespace, and
each worker and the control plane have their own private namespace.

```text
WS: caller process / caller namespace
    -> impaired caller veth -> caller bridge -> relay public interface
    -> IP routing inside relay -> private bridge -> room in store namespace

DC: caller process / caller namespace (shared audio PeerConnection)
    -> impaired caller veth -> caller bridge -> relay public UDP endpoint
    -> private relay leg -> worker -> private upstream WebSocket -> room

State delivery to the other client traverses the reverse chosen path.
Both clients share one caller namespace and its link queue.
```

WS has no loopback, host-management, or direct bridge shortcut. Its private room
route explicitly uses the relay's caller-facing address as gateway. DC's SDP
advertises the relay's caller-facing UDP address. The caller firewall retains
DROP policies and opens only room TCP, signal TCP, and relay media UDP. The
private room returns caller traffic through the private relay address. Forwarding
is enabled only in the owned relay namespace; no host forwarding, firewall,
qdisc, route metrics, or machine settings change. Actual caller route lookups,
requested settings, observed qdiscs, plan addresses, and untuned private routes
are saved in `network-settings.json`. No `rto_min`/quickack tuning is applied.

Unix-socket control provides only driver-owned metrics, status and worker kill.
It does not carry game traffic or recovery requests. Credentials and signal
setup cross the caller link. Private Redis keeps protected mode enabled and requires a random per-run
password, supplied in a run-directory `redis-auth.conf` with mode 0600. Clients
receive it only through `RELAIS_REDIS_PASSWORD`; it is absent from argv, public
configuration and artifacts. The config is removed after authenticated readiness
(or startup failure). Loopback Redis arguments remain unchanged. Each topology
owns Redis on an ephemeral port;
6379, 9101 and 9201 are excluded. Only owned children are stopped. No browsers,
DevTools, Docker, VMs or paid infrastructure are used.

## Exact profiles

These are **netem emulations**, not measurements from actual Wi-Fi or the public
Internet. Delay and loss apply to all packets, including voice and signaling.
`internal/nettopology` splits caller RTT/jitter equally between the two caller
veth ends. One direction crosses one shaped egress; a round trip crosses both.
The loss setting below is **per direction**, not a combined RTT probability.
Private RTT/jitter divide by four per egress because a private round trip crosses
four shaped egresses; private loss divides by two per endpoint pair.

| Profile | Caller netem, each direction | Private endpoint netem, each egress |
|---|---|---|
| `lan` | unshaped | unshaped |
| `region` | `limit 10000 delay 10000us loss 0.5%` | `limit 10000 delay 125us` |
| `far-caller` | `limit 10000 delay 75000us 10000us loss 2%` | `limit 10000 delay 125us` |
| `lossy-dc` | unshaped | `limit 10000 delay 375us 125us loss 0.05% reorder 0.1%` |
| `poor-wifi` (spike only) | `limit 10000 delay 15000us 7500us loss 2% 25% reorder 1% 25%` | unshaped |

Each impaired setting is applied with `tc qdisc replace dev INTERFACE root netem`
inside an owned namespace. Poor Wi-Fi therefore has a 30 ms nominal caller RTT,
15 ms configured end-to-end jitter parameter, 2% directional loss with 25%
correlation, and 1% reordering with 25% correlation. Correlation adds burstiness;
it is not a measured Wi-Fi burst model. Netem's default delay distribution is
used. No bandwidth cap, queue discipline tuning, MTU change, or fixed random seed
is added. The profile name `lossy-dc` means loss in the private **data centre**;
it is not a DataChannel-specific impairment.

## Variants

- `ws`: a direct ordered, reliable WebSocket from each caller to the room, plus
  Relais for voice.
- `dc-reliable`: separate `control` and `state` DataChannels on the audio PC;
  both are ordered and reliable.
- `dc-unreliable`: reliable, ordered `control`, and unordered `state` with
  `maxRetransmits=0`. This state policy was **already present** in the spike's
  existing Go DC client. A small opt-in policy argument adds the reliable state
  baseline. The worker negotiates the requested policy; production defaults in
  `pkg/` do not change.

The partially reliable state path can lose updates on either caller leg. The
private worker-to-room WebSocket still uses TCP. SCTP also retains association
congestion control and implementation limits. Thus UDP and unordered state do
not imply that all sources of head-of-line delay disappear.

## Metrics and fault correctness

Raw results preserve every sent sample, reply and received observation. Summary
percentiles use nearest rank and pool the steady windows across the eight runs
per profile/transport. `summary.json` retains sample counts, wire sizes, producer
lag, reorder/stale counts, failure names, and recovery distributions.

- Command RTT starts when the transport accepts the send and ends at its reply.
  Report p50/p95/p99/max over successful commands. Missing/error commands are
  counted and fail correctness; they are never fabricated as a long RTT.
- State age is origin `sent_ns` to the peer's distinct `state-update` receipt.
  This includes both clients' caller crossings. All processes share one kernel
  clock. Repeated snapshots and state echoes are excluded. Report p50/p95/p99.
- The head-of-line stall **indicator** is the longest gap between consecutive
  distinct received state updates, within each steady window. Nominal spacing
  is 66.7 ms. This is an observed gap, not proof that TCP caused it. Missing
  leading/trailing updates do not create a consecutive-update gap; delivery
  counts must be read with this statistic.
- Report expected and delivered commands, peer state deliveries, unique states,
  duplicate state deliveries, duplicate command receipts, and reordered states.
  Stale unordered state rejected by the room is separately counted.
- Disconnect: apply a command, hold its receipt, cut client 1, wait the same
  explicit 250 ms offline interval, rebuild its audio PC and data connection,
  rejoin with the same credentials, then complete a snapshot request. Measure
  cut to that snapshot reply. Record the generation increase and query/retry.
- Worker kill: apply a command with a pending receipt and SIGKILL the owned w1.
  DC waits for the control plane's actual takeover event with loss cause
  `data_session_reconnect_required`, then rebuilds the call on w2 and proves a
  snapshot reply. WS keeps its data socket and proves a snapshot on that socket;
  the fault receipt is suppressed without pausing the surviving WS writer.
  Measure kill to that reply. DC detection uses 20 ms status polling, included
  in the recovery number. This measures the architecture's reconnect policy,
  not just wire speed.
- Lost receipt and normal: query the recorded result, retry the identical
  command and require the identical applied result. Across every run, all
  expected command side effects occur, the deliberate retry deduplicates
  exactly once, and held fault commands apply exactly once.

Two recovery samples per transport/profile are a small diagnostic sample. Keep
raw values; do not treat their p99 as a stable recovery tail estimate.

## Reproduce and collect

Build as the normal user (Go dependencies must be available to the offline build):

```bash
go mod download
bash experiments/nvwa-room/scripts/build.sh "$SCRATCH"
python3 experiments/nvwa-room/scripts/run-network.py \
  --scratch "$SCRATCH" --out "$RESULTS" --topology loopback --profile lan
```

CI runs the **same** build script and comparison command, changing only paths,
`--topology netns`, `--profile PROFILE`, and running the comparison with sudo for
namespace setup. The comparison defaults remain two repeats, 1,000 messages,
30 Hz. `RELAIS_NVWA_TEST_BIN="$SCRATCH/bin" go test -race ./experiments/nvwa-room/... ./internal/spike/...` checks private-address integration, bus validation, channel policies, the
real room send queue, reliable snapshot isolation, and fault barriers locally.
`go vet ./...`, gofmt, and golangci-lint v2.14.0 cover the checkout.

After the orchestrator pushes this branch:

```bash
gh workflow run ci.yml --repo relaishq/relais --ref spike/nvwa-room \
  -f network_profile_lane=nvwa-dc-vs-ws
```

Only this lane runs when selected. All other jobs, including the aggregate,
quality, media matrices and self-hosted cluster gate, skip. Five `ubuntu-latest`
jobs run independently. The round-2 budget keeps two repeats and 1,000
messages/client for all 24 cells. The measured local mean was 40.36 s/cell.
Because sends are independent of receipts, added RTT does not multiply by 1,000.
The following are planning allowances, not Linux measurements:

| Profile | Budget per cell | 24-cell comparison | Expected job including 3–5 min setup |
|---|---|---|---|
| `lan` | 40.4 s measured local baseline | 16.2 min | 19–21 min |
| `region` | 42.5 s | 17.0 min | 20–22 min |
| `far-caller` | 55 s | 22.0 min | 25–27 min |
| `lossy-dc` | 45 s | 18.0 min | 21–23 min |
| `poor-wifi` | 50 s | 20.0 min | 23–25 min |

The added allowances cover serial signaling/reconnect requests, delayed command
drainage, loss tails, and namespace setup. The far-caller plan leaves about
6 minutes below the comparison step limit and 8 minutes below the job limit
at its expected upper duration. The internal 24-minute budget leaves nearly
4 minutes for graceful shutdown and step overhead before the external timeout.
If those allowances are insufficient, the profile fails explicitly with partial
evidence; it does not silently reduce the trial count. Expected total consumption
is roughly 110–120 runner minutes. Account concurrency can extend wall time.
Namespace isolation/profile tests run before each matrix; cleanup assertions and
ownership restoration run even on failure. The runner forwards SIGTERM and
SIGINT to its owned driver and waits for that driver's topology cleanup.
Before namespace creation, each driver writes a 0600 `namespace-owner.json`
with its UID/PID. An `if: always()` workflow step invokes
`network-driver -cleanup-root PROFILE_OUTPUT` before the unchanged cleanup
assertion. This reuses `internal/nettopology` stale cleanup with a mandatory
allowlist from that profile's owner records. It accepts only exact owned names
for the current UID, skips living or reused PIDs, and deletes links only for
matched stale topology tokens. It never performs a broad UID-only sweep. A new
native Linux regression preserves an unrelated stale owner and a live owner;
that test cannot run on macOS. Redis package installation is
prevented from starting a system service. Artifacts are named:

```text
nvwa-network-lan
nvwa-network-region
nvwa-network-far-caller
nvwa-network-lossy-dc
nvwa-network-poor-wifi
```

Each contains `summary.json`, `summary.md`, the ledger, gzipped raw results,
network settings, topology metadata, logs, cleanup evidence, kernel description,
and binary/source hashes. Signal tokens, private certificates, Redis auth config and caller config
are excluded. Ownership restoration keeps any residual Redis auth file at 0600. To merge downloaded artifacts into the results section:

```bash
gh run download RUN_ID --repo relaishq/relais --pattern 'nvwa-network-*' --dir "$ARTIFACTS"
python3 experiments/nvwa-room/scripts/summarize-network.py "$ARTIFACTS" --from-summaries \
  --readme experiments/nvwa-room/evidence/stage4-network/README.md
```

## Local verification

The final-source loopback smoke ran all 12 variant/scenario cells (one repeat,
60 messages/client at the same 30 Hz cadence) in 104.5 s. All cells passed,
including actual DC takeover classification and the unchanged WS socket.
Per transport, it delivered 240/240 commands and 240/240 peer state updates,
with zero duplicate command receipts or state updates. Every deliberate retry
deduplicated exactly once. Binary and source hashes are retained with the
scratch evidence. The full default 24-cell loopback comparison also passed in 973.0 s
(968.75 s summed cell times): 48,000/48,000 scheduled application messages,
zero duplicate receipts/updates, and 24 successful exactly-once retry checks.
Its wefty run is `run_af6fa7a8b7b7e72988cac91e8e05141f`. It used the same default
command as CI, apart from topology/profile and paths. Those binaries precede
only the final namespace route observations and TCP/UDP port-number collision
guard. The final-source smoke covers those source revisions; both build/source
manifests remain separate. Loopback is a correctness check, not a substitute for
the Linux profiles or a performance conclusion.

Local checks passed: `go test -race ./experiments/nvwa-room/...` (2.73 s final
invocation, including cached unchanged packages), `go vet ./...` (1.95 s),
golangci-lint v2.14.0 `run ./...` (2.52 s, zero issues), Linux amd64 cross-build
(4.21 s), gofmt, `actionlint`, and the three evidence-reducer tests. Lint uses a
scratch cache; no machine-wide tools or settings were installed or changed.

## Linux results

<!-- RESULTS:START -->

| Profile | Transport | Command RTT | State age | Longest state gap | Delivery/duplicates | Disconnect | Worker kill | Status |
|---|---|---|---|---|---|---|---|---|
| lan / region / far-caller / lossy-dc / poor-wifi | ws / dc-reliable / dc-unreliable | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN | NOT-RUN | Pending CI |

<!-- RESULTS:END -->

## Interpretation limits

Both transports cross the same caller impairment with no direct WS shortcut.
Their private paths intentionally differ: DC has relay/worker legs and an
upstream TCP connection, while WS routes directly to the room. `lossy-dc` can
therefore penalize DC across more private hops; this is a property of the chosen
architecture. WS is plaintext WS in a private emulation, while DC uses DTLS;
this is not a controlled comparison of equal TLS cost. Both callers share one
queue; no competing household traffic or asymmetric caller networks are modeled.
The runner has virtual Ethernet, shared CPU scheduling, software netem and Linux
TCP, not browser TCP on Wi-Fi hardware. Veth offload settings stay at runner
defaults; their effect is not isolated. The same Pion Go clients drive both
variants; native browser and mobile behavior remain unmeasured. No claim covers
NAT traversal, TURN, UDP blocking, bandwidth starvation, or a real remote private
subnet deployment. Netem loss is independent across paired runs, not a replayed
identical packet-loss trace. Use the paired reversals and raw tails to judge
noise. Failures remain failures in the ledger and summary; they do not become
successful zero-latency or zero-loss samples.

## CI round 1 follow-up verification

The private Redis credential construction regression was observed red before
its fix. A local real-process test then bound Redis to a non-loopback private
address, rejected an unauthenticated ping, and accepted both an authenticated
ping and the actual Relais `processrun.StoreConfig` client. Protected mode stayed
enabled. Unit checks also verify the unchanged loopback command, private config
mode/removal, fast child-exit diagnostics, credential redaction, and a bounded
readiness timeout. Python process tests exercise two-startup-failure stop,
SIGTERM/SIGINT forwarding with cleanup, and the profile budget. Linux namespace
reclamation remains NOT-RUN locally and is included in the next CI test binary.

Round-2 local results: all 12 loopback smoke cells passed in 103.69 s, with
240/240 commands and 240/240 peer state updates per transport. Real runner/driver
SIGTERM and SIGINT checks returned in 0.686 s and 0.645 s respectively, with all
owned room, signal, control and Redis listeners closed. Uncached race tests for
all spike packages plus `internal/nettopology` passed in 3.17 s. `go vet ./...`
passed in 1.26 s; golangci-lint v2.14.0 on `./...` passed in 2.96 s with zero
issues. All three reducer tests and three runner process tests passed (0.42 s
and 1.94 s wall time). Linux driver/test cross-builds, gofmt and actionlint passed.
These timings are local; no namespace runtime success is claimed.

## CI round 2 address follow-up

The spike audio factory now accepts RFC1918 IPv4 bus addresses as well as
loopback, while rejecting public and unspecified destinations. Local defaults
stay on loopback. Room listener failures now explain the required explicit
private CIDR instead of incorrectly demanding loopback. The full executable
address/URL/Origin audit and unchanged local-only tools are listed in
[netns-address-audit.md](netns-address-audit.md). No `pkg/` production default was
changed. The new combined private-bind integration test passed for all three
transports, including the real worker upstream WS, private-IP TLS signal,
commands, state delivery and two-way audio. It changes no machine interface.

Round-3 verification: the full uncached race suite, with private binding enabled,
passed in 6.74 s (all three private-address variants passed in 3.27 s within that
suite). The 12-cell loopback smoke passed in 102.54 s with 240/240 commands and
240/240 peer states per transport and zero duplicates. `go vet ./...` passed in
1.31 s, golangci-lint v2.14.0 on `./...` in 2.57 s with zero issues, and gofmt in
0.09 s. All three reducer tests and all three runner tests passed (0.26 s and
1.77 s wall time), as did actionlint and the Linux driver cross-build (1.39 s).
Native Linux namespace/profile execution remains NOT-RUN locally.
