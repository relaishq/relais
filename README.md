# Relais

**Deploy your voice AI agent while its call keeps running.**

Relais is a self-hosted WebRTC media server written in Go. A live call keeps
running when its media worker is redeployed, drained, or crashes (`kill -9`).
The caller keeps the same connection: no reconnect, no ICE restart (repeating
network-path setup), and no renegotiation (another media offer/answer exchange).
A compatible audio agent's saved state moves with the call.

Use it when you run live AI agents and want to deploy mid-call. Relais supplies
the media and recovery layer; you supply the speech and AI services.

Measured evidence, with workload and failure limits:

- Planned moves: **44 ms maximum video pause** in the one-hour test run.
  [Planned-move evidence](docs/guarantees.md#planned-move).
- Worker crashes: **529 ms maximum video pause** in that run.
  [Takeover evidence](docs/guarantees.md#crash-takeover-with-the-relay-buffer).
- Capacity: **50 concurrent calls** in the capacity check on the
  documented Linux host. This is a demonstrated load, not a maximum.
  [Capacity and soak](docs/guarantees.md#capacity-and-soak).

Measured in a one-hour run of 50 synthetic echo calls on one 32-vCPU Linux VM,
with every process and Redis on that host and the relay buffer on. Separate
hosts and real agent workloads at this load are not measured.
[Run conditions](docs/guarantees.md#capacity-and-soak).

## Status and limits

**Pre-1.0.** The [guarantee matrix](docs/guarantees.md)
is the evidence and the boundary of each claim. A surviving connection can
still have a media pause. Agent takeover restores a checkpoint; recent work
can repeat. Applications must make external side effects safe to repeat.

- The default relay is one process. Its tested restart path restores routes;
  cross-host relay failover is not established. See
  [relay restart](docs/guarantees.md#relay-restart) and the optional
  [same-host standby](docs/guarantees.md#same-host-standby-relay).
- Multi-process runs require Redis. Redis TLS and ACL usernames are supported.
  Loopback Redis port 6379 is refused by default; see [Managed Redis](docs/deploy.md#managed-redis).
- Custom agents need a fork today. The built-ins echo media or play a tone script.
- Desktop Chrome is tested; Safari is not. Agent continuity over the real
  internet is not measured. See [non-guarantees](docs/guarantees.md#explicit-non-guarantees).
- After an aborted move, a caller could not decrypt three packets. This occurred
  once in about 210 untraced rollbacks. The cause is unknown; this is not a stable
  failure rate. See [#51](docs/guarantees.md#51-caller-decryption-failure-after-rollback).
- Redeploying a changed agent or Relais build mid-call works only if the new
  build can read the old agent state and snapshot versions. Mixed-version rollout
  is not guaranteed. See [limits](docs/guarantees.md#explicit-non-guarantees) and
  [ADR 0006](docs/adr/0006-agent-interface.md#resume-and-replay).
- One caller per call: no rooms, fan-out, simulcast, or SVC (layered video). Media
  uses UDP only, with no TURN relay or TCP/TLS fallback. Control-plane crash
  recovery is not established. See [non-guarantees](docs/guarantees.md#explicit-non-guarantees).
- Private control APIs are unauthenticated. The signaling frontend is for
  controlled tests; a proven public service is outside the current evidence.
- The [legacy stored-frame pipeline](docs/legacy/plugin-pipeline.md) is
  experimental, unmaintained, and not used by live calls.

## Five-minute quick start

Use macOS or Linux, Go 1.26+, Git, current Chrome, and `redis-server` on PATH.
Install Redis with `brew install redis` or `sudo apt install redis-server`.
You do not need to start a Redis service: the demo starts its own throwaway
Redis on a free port. The Relais process binaries refuse Redis port **6379**.

Build from source; this is the install path that works today:

```sh
git clone https://github.com/relaishq/relais && cd relais && make build
./bin/relais-demo -agent=demo
```

Open the printed URL in Chrome. Click **Start call** and keep **Play returned
audio** enabled. You will see a canvas test pattern returned through the relay
and hear a rising tone scale. The counter advances as the agent consumes audio.

Try **Move call**, **Drain owner**, then **Kill owner (SIGKILL)**. A drain moves
the worker's calls away before stopping it. A takeover restores a crashed
worker's calls on another worker. Listen for the high resume chime and watch
the count continue. A crash can repeat tones since the last checkpoint.

The demo runs a relay, control plane, and three workers as separate processes.
Ctrl-C stops its children and its Redis. If the page port is busy, run
`./bin/relais-demo -agent=demo -http=127.0.0.1:9201`.
See [demo controls, camera mode, and continuity checks](docs/demo.md).

No release exists yet. When the first release is published, see
[installation](docs/install.md) for archive downloads and checksums.

## How it works

```text
Caller <== encrypted media ==> Relay <== private UDP ==> Workers + audio agent
                              | one public UDP port          |
                              +------ Control plane ---------+
                                           |
                                  Redis session store
```

The relay keeps the call's media address stable and routes media to the
owning worker. Workers handle WebRTC encryption and agent callbacks. The
control plane places calls, moves them, and detects failed workers. Workers
and control share encrypted session checkpoints in Redis; the relay reads
ownership without the snapshot key. It also buffers caller media for takeover.

A planned move exports the accepted transport and agent state. After a crash,
a new worker takes ownership and restores the latest safe checkpoint. Ownership
leases prevent two workers from sending as the same session. See the
[architecture](docs/architecture.md) for the protocol and failure boundaries.

## Run it yourself

The `start.sh` and `stop.sh` scripts start and stop the full topology on one
machine, including a dedicated Redis. They live in each release archive, or
under `deploy/local/` in a checkout. See [installation](docs/install.md) for
commands, then [deployment](docs/deploy.md) for ports, keys, start/stop order,
private networks, and UDP buffer settings.
The demo launcher is a test tool, not a production process supervisor.

## Connect your app

Your backend sends an SDP offer to `relais-signal` over authenticated HTTPS,
receives an answer and a call URL, and deletes that URL to hang up. SDP is the
text description of the media connection. The frontend has one shared bearer
token and no cross-origin browser access (CORS); keep the token in your backend.
See [connect an app](docs/connect-an-app.md) for requests and certificate setup.

## Build your agent

Implement `Process`, `Save`, `Restore`, and `Resume` from
[`pkg/agent`](pkg/agent/agent.go). Today you must fork Relais, add your factory to
the worker's built-in selection, and rebuild. All workers need compatible agents.
There is no runtime plugin or subprocess interface for live agents.

Agents receive encoded Opus audio, a compressed audio format; video stays on the
echo path. Speech-provider integration and resumable audio decoding are not
supplied. See [ADR 0006](docs/adr/0006-agent-interface.md) for the contract and
[agent continuity](docs/guarantees.md#agent-continuity) for what is proven.

## How it compares

Relais focuses on moving the media connection and checkpointed audio-agent state
between workers while keeping the caller's connection. Other tools use several
recovery paths. Compare the documented mechanisms, deployment scope, and limits
in [the fact-checked comparison](docs/comparison.md); it is not a benchmark.

## Documentation

| Goal | Read |
| --- | --- |
| Try moves and crashes | [Demo guide](docs/demo.md) |
| Install and operate | [Install](docs/install.md) · [Deploy](docs/deploy.md) |
| Connect a caller | [App guide](docs/connect-an-app.md) |
| Write an audio agent | [Agent contract](docs/adr/0006-agent-interface.md) |
| Understand proof and limits | [Guarantees](docs/guarantees.md) |
| Understand the design | [Architecture](docs/architecture.md) · [Decisions](docs/adr/) |
| Compare tools | [Comparison](docs/comparison.md) |
| Develop and test | [Contributing](CONTRIBUTING.md) · [Developer guides](docs/dev/) |
| Inspect the old plugin system | [Legacy pipeline](docs/legacy/plugin-pipeline.md) |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build, test, lint, and PR checks.

## License

Relais is licensed under the [Apache License 2.0](LICENSE). Copyright 2024-2026 Derek Wang; see [NOTICE](NOTICE).
